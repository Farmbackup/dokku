package scheduler_k3s

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dokku/dokku/plugins/common"
	resty "github.com/go-resty/resty/v2"
	"github.com/ryanuber/columnize"
)

// CommandInitialize initializes a k3s cluster on the local server
func CommandInitialize(taintScheduling bool) error {
	if err := isK3sInstalled(); err == nil {
		return fmt.Errorf("k3s already installed, cannot re-initialize k3s")
	}

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()

	networkInterface := getGlobalNetworkInterface()
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("Unable to get network interfaces: %w", err)
	}

	serverIp := ""
	for _, iface := range ifaces {
		if iface.Name == networkInterface {
			addr, err := iface.Addrs()
			if err != nil {
				return fmt.Errorf("Unable to get network addresses for interface %s: %w", networkInterface, err)
			}
			for _, a := range addr {
				if ipnet, ok := a.(*net.IPNet); ok {
					if ipnet.IP.To4() != nil {
						serverIp = ipnet.IP.String()
					}
				}
			}
		}
	}

	if len(serverIp) == 0 {
		return fmt.Errorf(fmt.Sprintf("Unable to determine server ip address from network-interface %s", networkInterface))
	}

	common.LogInfo1Quiet("Initializing k3s")

	common.LogInfo2Quiet("Updating apt")
	aptUpdateCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command: "apt-get",
		Args: []string{
			"update",
		},
		StreamStdio: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call apt-get update command: %w", err)
	}
	if aptUpdateCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from apt-get update command: %d", aptUpdateCmd.ExitCode)
	}

	common.LogInfo2Quiet("Installing k3s dependencies")
	aptInstallCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command: "apt-get",
		Args: []string{
			"-y",
			"install",
			"ca-certificates",
			"curl",
			"open-iscsi",
			"nfs-common",
			"wireguard",
		},
		StreamStdio: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call apt-get install command: %w", err)
	}
	if aptInstallCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from apt-get install command: %d", aptInstallCmd.ExitCode)
	}

	common.LogInfo2Quiet("Downloading k3s installer")
	client := resty.New()
	resp, err := client.R().
		Get("https://get.k3s.io")
	if err != nil {
		return fmt.Errorf("Unable to download k3s installer: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("Missing response from k3s installer download: %w", err)
	}

	if resp.StatusCode() != 200 {
		return fmt.Errorf("Invalid status code for k3s installer script: %d", resp.StatusCode())
	}

	f, err := os.CreateTemp("", "sample")
	if err != nil {
		return fmt.Errorf("Unable to create temporary file for k3s installer: %w", err)
	}
	defer os.Remove(f.Name())

	if err := f.Close(); err != nil {
		return fmt.Errorf("Unable to close k3s installer file: %w", err)
	}

	err = common.WriteSliceToFile(common.WriteSliceToFileInput{
		Filename: f.Name(),
		Lines:    strings.Split(resp.String(), "\n"),
		Mode:     os.FileMode(0755),
	})
	if err != nil {
		return fmt.Errorf("Unable to write k3s installer to file: %w", err)
	}

	fi, err := os.Stat(f.Name())
	if err != nil {
		return fmt.Errorf("Unable to get k3s installer file size: %w", err)
	}

	if fi.Size() == 0 {
		return fmt.Errorf("Invalid k3s installer filesize")
	}

	token := getGlobalGlobalToken()
	if len(token) == 0 {
		n := 5
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("Unable to generate random node name: %w", err)
		}

		token := strings.ToLower(fmt.Sprintf("%X", b))
		if err := CommandSet("--global", "token", token); err != nil {
			return fmt.Errorf("Unable to set k3s token: %w", err)
		}
	}

	nodeName := serverIp
	n := 5
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("Unable to generate random node name: %w", err)
	}
	nodeName = strings.ReplaceAll(strings.ToLower(fmt.Sprintf("ip-%s-%s", nodeName, fmt.Sprintf("%X", b))), ".", "-")

	args := []string{
		// initialize the cluster
		"--cluster-init",
		// disable local-storage
		"--disable", "local-storage",
		// expose etcd metrics
		"--etcd-expose-metrics",
		// use wireguard for flannel
		"--flannel-backend=wireguard-native",
		// bind controller-manager to all interfaces
		"--kube-controller-manager-arg", "bind-address=0.0.0.0",
		// bind proxy metrics to all interfaces
		"--kube-proxy-arg", "metrics-bind-address=0.0.0.0",
		// bind scheduler to all interfaces
		"--kube-scheduler-arg", "bind-address=0.0.0.0",
		// gc terminated pods
		"--kube-controller-manager-arg", "terminated-pod-gc-threshold=10",
		// specify the node name
		"--node-name", nodeName,
		// allow access for the dokku user
		"--write-kubeconfig-mode", "0644",
		// specify a token
		"--token", token,
	}
	if taintScheduling {
		args = append(args, "--node-taint", "CriticalAddonsOnly=true:NoSchedule")
	}

	common.LogInfo2Quiet("Running k3s installer")
	installerCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command:     f.Name(),
		Args:        args,
		StreamStdio: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call k3s installer command: %w", err)
	}
	if installerCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from k3s installer command: %d", installerCmd.ExitCode)
	}

	if err := common.TouchFile(RegistryConfigPath); err != nil {
		return fmt.Errorf("Error creating initial registries.yaml file")
	}

	common.LogInfo2Quiet("Setting registries.yaml permissions")
	registryAclCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command: "setfacl",
		Args: []string{
			"-m",
			"user:dokku:rwx",
			RegistryConfigPath,
		},
		StreamStdio: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call setfacl command: %w", err)
	}
	if registryAclCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from setfacl command: %d", registryAclCmd.ExitCode)
	}

	clientset, err := NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("Unable to create kubernetes client: %w", err)
	}

	for _, manifest := range KubernetesManifests {
		common.LogInfo2Quiet(fmt.Sprintf("Installing %s@%s", manifest.Name, manifest.Version))
		err = clientset.ApplyKubernetesManifest(ctx, ApplyKubernetesManifestInput{
			Manifest: manifest.Path,
		})
		if err != nil {
			return fmt.Errorf("Unable to apply kubernetes manifest: %w", err)
		}
	}

	for key, value := range ServerLabels {
		common.LogInfo2Quiet(fmt.Sprintf("Labeling node %s=%s", key, value))
		if err != nil {
			return fmt.Errorf("Unable to create kubernetes client: %w", err)
		}

		err = clientset.LabelNode(ctx, LabelNodeInput{
			Name:  nodeName,
			Key:   key,
			Value: value,
		})
		if err != nil {
			return fmt.Errorf("Unable to patch node: %w", err)
		}
	}

	common.LogInfo2Quiet("Updating traefik config")
	contents, err := templates.ReadFile("templates/traefik-config.yaml")
	if err != nil {
		return fmt.Errorf("Unable to read traefik config template: %w", err)
	}

	err = os.WriteFile("/var/lib/rancher/k3s/server/manifests/traefik-custom.yaml", contents, 0600)
	if err != nil {
		return fmt.Errorf("Unable to write traefik config: %w", err)
	}

	common.LogInfo2Quiet("Installing helm charts")
	err = installHelmCharts(ctx, clientset)
	if err != nil {
		return fmt.Errorf("Unable to install helm charts: %w", err)
	}

	common.LogVerboseQuiet("Done")

	return nil
}

// CommandClusterAdd adds a server to the k3s cluster
func CommandClusterAdd(role string, remoteHost string, allowUknownHosts bool, taintScheduling bool) error {
	if err := isK3sInstalled(); err != nil {
		return fmt.Errorf("k3s not installed, cannot join cluster")
	}

	if role != "server" && role != "worker" {
		return fmt.Errorf("Invalid server-type: %s", role)
	}

	token := getGlobalGlobalToken()
	if len(token) == 0 {
		return fmt.Errorf("Missing k3s token")
	}

	if taintScheduling && role == "worker" {
		return fmt.Errorf("Taint scheduling can only be used on the server role")
	}

	networkInterface := getGlobalNetworkInterface()
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("Unable to get network interfaces: %w", err)
	}

	serverIp := ""
	for _, iface := range ifaces {
		if iface.Name == networkInterface {
			addr, err := iface.Addrs()
			if err != nil {
				return fmt.Errorf("Unable to get network addresses for interface %s: %w", networkInterface, err)
			}
			for _, a := range addr {
				if ipnet, ok := a.(*net.IPNet); ok {
					if ipnet.IP.To4() != nil {
						serverIp = ipnet.IP.String()
					}
				}
			}
		}
	}

	if len(serverIp) == 0 {
		return fmt.Errorf(fmt.Sprintf("Unable to determine server ip address from network-interface %s", networkInterface))
	}

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()

	// todo: check if k3s is installed on the remote host

	k3sVersionCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command: "k3s",
		Args: []string{
			"--version",
		},
		CaptureOutput: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call k3s version command: %w", err)
	}
	if k3sVersionCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from k3s --version command: %d", k3sVersionCmd.ExitCode)
	}

	k3sVersion := ""
	k3sVersionLines := strings.Split(string(k3sVersionCmd.Stdout), "\n")
	if len(k3sVersionLines) > 0 {
		k3sVersionParts := strings.Split(k3sVersionLines[0], " ")
		if len(k3sVersionParts) != 4 {
			return fmt.Errorf("Unable to get k3s version from k3s --version: %s", k3sVersionCmd.Stdout)
		}
		k3sVersion = k3sVersionParts[2]
	}
	common.LogDebug(fmt.Sprintf("k3s version: %s", k3sVersion))

	common.LogInfo1(fmt.Sprintf("Joining %s to k3s cluster as %s", remoteHost, role))
	common.LogInfo2Quiet("Updating apt")
	aptUpdateCmd, err := common.CallSshCommand(common.SshCommandInput{
		Command: "apt-get",
		Args: []string{
			"update",
		},
		AllowUknownHosts: allowUknownHosts,
		RemoteHost:       remoteHost,
		StreamStdio:      true,
		Sudo:             true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call apt-get update command over ssh: %w", err)
	}
	if aptUpdateCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from apt-get update command over ssh: %d", aptUpdateCmd.ExitCode)
	}

	common.LogInfo2Quiet("Installing k3s dependencies")
	aptInstallCmd, err := common.CallSshCommand(common.SshCommandInput{
		Command: "apt-get",
		Args: []string{
			"-y",
			"install",
			"ca-certificates",
			"curl",
			"open-iscsi",
			"nfs-common",
			"wireguard",
		},
		AllowUknownHosts: allowUknownHosts,
		RemoteHost:       remoteHost,
		StreamStdio:      true,
		Sudo:             true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call apt-get install command over ssh: %w", err)
	}
	if aptInstallCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from apt-get install command over ssh: %d", aptInstallCmd.ExitCode)
	}

	common.LogInfo2Quiet("Downloading k3s installer")
	curlTask, err := common.CallSshCommand(common.SshCommandInput{
		Command: "curl",
		Args: []string{
			"-o /tmp/k3s-installer.sh",
			"https://get.k3s.io",
		},
		AllowUknownHosts: allowUknownHosts,
		RemoteHost:       remoteHost,
		StreamStdio:      true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call curl command over ssh: %w", err)
	}
	if curlTask.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from curl command over ssh: %d", curlTask.ExitCode)
	}

	common.LogInfo2Quiet("Setting k3s installer permissions")
	chmodCmd, err := common.CallSshCommand(common.SshCommandInput{
		Command: "chmod",
		Args: []string{
			"0755",
			"/tmp/k3s-installer.sh",
		},
		AllowUknownHosts: allowUknownHosts,
		RemoteHost:       remoteHost,
		StreamStdio:      true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call chmod command over ssh: %w", err)
	}
	if chmodCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from chmod command over ssh: %d", chmodCmd.ExitCode)
	}

	u, err := url.Parse(remoteHost)
	if err != nil {
		return fmt.Errorf("failed to parse remote host: %w", err)
	}

	nodeName := u.Hostname()
	n := 5
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("Unable to generate random node name: %w", err)
	}
	nodeName = strings.ReplaceAll(strings.ToLower(fmt.Sprintf("ip-%s-%s", nodeName, fmt.Sprintf("%X", b))), ".", "-")

	args := []string{
		// disable local-storage
		"--disable", "local-storage",
		// use wireguard for flannel
		"--flannel-backend=wireguard-native",
		// specify the node name
		"--node-name", nodeName,
		// server to connect to as the main
		"--server",
		fmt.Sprintf("https://%s:6443", serverIp),
		// specify a token
		"--token",
		token,
	}

	if role == "server" {
		args = append([]string{"server"}, args...)
		// expose etcd metrics
		args = append(args, "--etcd-expose-metrics")
		// bind controller-manager to all interfaces
		args = append(args, "--kube-controller-manager-arg", "bind-address=0.0.0.0")
		// bind proxy metrics to all interfaces
		args = append(args, "--kube-proxy-arg", "metrics-bind-address=0.0.0.0")
		// bind scheduler to all interfaces
		args = append(args, "--kube-scheduler-arg", "bind-address=0.0.0.0")
		// gc terminated pods
		args = append(args, "--kube-controller-manager-arg", "terminated-pod-gc-threshold=10")
		// allow access for the dokku user
		args = append(args, "--write-kubeconfig-mode", "0644")
	} else {
		// disable etcd on workers
		args = append(args, "--disable-etcd")
		// disable apiserver on workers
		args = append(args, "--disable-apiserver")
		// disable controller-manager on workers
		args = append(args, "--disable-controller-manager")
		// disable scheduler on workers
		args = append(args, "--disable-scheduler")
		// bind proxy metrics to all interfaces
		args = append(args, "--kube-proxy-arg", "metrics-bind-address=0.0.0.0")
	}

	if taintScheduling {
		args = append(args, "--node-taint", "CriticalAddonsOnly=true:NoSchedule")
	}

	common.LogInfo2Quiet(fmt.Sprintf("Adding %s k3s cluster", nodeName))
	joinCmd, err := common.CallSshCommand(common.SshCommandInput{
		Command:          "/tmp/k3s-installer.sh",
		Args:             args,
		AllowUknownHosts: allowUknownHosts,
		RemoteHost:       remoteHost,
		StreamStdio:      true,
		Sudo:             true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call k3s installer command over ssh: %w", err)
	}
	if joinCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from k3s installer command over ssh: %d", joinCmd.ExitCode)
	}

	clientset, err := NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("Unable to create kubernetes client: %w", err)
	}

	common.LogInfo2Quiet("Waiting for node to exist")
	nodes, err := waitForNodeToExist(ctx, WaitForNodeToExistInput{
		Clientset:  clientset,
		NodeName:   nodeName,
		RetryCount: 20,
	})
	if err != nil {
		return fmt.Errorf("Error waiting for pod to exist: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("Unable to find node after joining cluster, node will not be annotated/labeled appropriately access registry secrets")
	}

	err = copyRegistryToNode(ctx, remoteHost)
	if err != nil {
		return fmt.Errorf("Error copying registries.yaml to remote host: %w", err)
	}

	labels := ServerLabels
	if role == "worker" {
		labels = WorkerLabels
	}

	for key, value := range labels {
		common.LogInfo2Quiet(fmt.Sprintf("Labeling node %s=%s", key, value))
		if err != nil {
			return fmt.Errorf("Unable to create kubernetes client: %w", err)
		}

		err = clientset.LabelNode(ctx, LabelNodeInput{
			Name:  nodeName,
			Key:   key,
			Value: value,
		})
		if err != nil {
			return fmt.Errorf("Unable to patch node: %w", err)
		}
	}

	common.LogInfo2Quiet("Annotating node with connection information")
	err = clientset.AnnotateNode(ctx, AnnotateNodeInput{
		Name:  nodes[0].Name,
		Key:   "dokku.com/remote-host",
		Value: remoteHost,
	})
	if err != nil {
		return fmt.Errorf("Unable to patch node: %w", err)
	}

	common.LogVerboseQuiet("Done")
	return nil
}

// CommandClusterList lists the nodes in the k3s cluster
func CommandClusterList(format string) error {
	if format != "stdout" && format != "json" {
		return fmt.Errorf("Invalid format: %s", format)
	}
	if err := isK3sInstalled(); err != nil {
		return fmt.Errorf("k3s not installed, cannot list cluster nodes")
	}

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()

	clientset, err := NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("Unable to create kubernetes client: %w", err)
	}

	nodes, err := clientset.ListNodes(ctx, ListNodesInput{})
	if err != nil {
		return fmt.Errorf("Unable to list nodes: %w", err)
	}

	output := []Node{}
	for _, node := range nodes {
		output = append(output, kubernetesNodeToNode(node))
	}

	if format == "stdout" {
		lines := []string{"name|ready|roles|version"}
		for _, node := range output {
			lines = append(lines, node.String())
		}

		columnized := columnize.SimpleFormat(lines)
		fmt.Println(columnized)
		return nil
	}

	b, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("Unable to marshal json: %w", err)
	}

	fmt.Println(string(b))
	return nil
}

// CommandClusterRemove removes a node from the k3s cluster
func CommandClusterRemove(nodeName string) error {
	if err := isK3sInstalled(); err != nil {
		return fmt.Errorf("k3s not installed, cannot remove node")
	}

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()

	common.LogInfo1Quiet(fmt.Sprintf("Removing %s from k3s cluster", nodeName))
	clientset, err := NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("Unable to create kubernetes client: %w", err)
	}

	common.LogVerboseQuiet("Getting node remote connection information")
	node, err := clientset.GetNode(ctx, GetNodeInput{
		Name: nodeName,
	})
	if err != nil {
		return fmt.Errorf("Unable to get node: %w", err)
	}

	common.LogVerboseQuiet("Checking if node is a remote node managed by Dokku")
	if node.RemoteHost == "" {
		return fmt.Errorf("Node %s is not a remote node managed by Dokku", nodeName)
	}

	common.LogVerboseQuiet("Uninstalling k3s on remote host")
	removeCmd, err := common.CallSshCommand(common.SshCommandInput{
		Command:          "/usr/local/bin/k3s-uninstall.sh",
		Args:             []string{},
		AllowUknownHosts: true,
		RemoteHost:       node.RemoteHost,
		StreamStdio:      true,
		Sudo:             true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call k3s uninstall command over ssh: %w", err)
	}

	if removeCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from k3s uninstall command over ssh: %d", removeCmd.ExitCode)
	}

	common.LogVerboseQuiet("Deleting node from k3s cluster")
	err = clientset.DeleteNode(ctx, DeleteNodeInput{
		Name: nodeName,
	})
	if err != nil {
		return fmt.Errorf("Unable to delete node: %w", err)
	}

	common.LogVerboseQuiet("Done")

	return nil
}

// CommandReport displays a scheduler-k3s report for one or more apps
func CommandReport(appName string, format string, infoFlag string) error {
	if len(appName) == 0 {
		apps, err := common.DokkuApps()
		if err != nil {
			return err
		}
		for _, appName := range apps {
			if err := ReportSingleApp(appName, format, infoFlag); err != nil {
				return err
			}
		}
		return nil
	}

	return ReportSingleApp(appName, format, infoFlag)
}

// CommandSet set or clear a scheduler-k3s property for an app
func CommandSet(appName string, property string, value string) error {
	common.CommandPropertySet("scheduler-k3s", appName, property, value, DefaultProperties, GlobalProperties)
	return nil
}

// CommandShowKubeconfig displays the kubeconfig file contents
func CommandShowKubeconfig() error {
	if !common.FileExists(KubeConfigPath) {
		return fmt.Errorf("Kubeconfig file does not exist: %s", KubeConfigPath)
	}

	b, err := os.ReadFile(KubeConfigPath)
	if err != nil {
		return fmt.Errorf("Unable to read kubeconfig file: %w", err)
	}

	fmt.Println(string(b))

	return nil
}

func CommandUninstall() error {
	if err := isK3sInstalled(); err != nil {
		return fmt.Errorf("k3s not installed, cannot uninstall")
	}

	common.LogInfo1("Uninstalling k3s")
	uninstallerCmd, err := common.CallExecCommand(common.ExecCommandInput{
		Command:     "/usr/local/bin/k3s-uninstall.sh",
		StreamStdio: true,
	})
	if err != nil {
		return fmt.Errorf("Unable to call k3s uninstaller command: %w", err)
	}
	if uninstallerCmd.ExitCode != 0 {
		return fmt.Errorf("Invalid exit code from k3s uninstaller command: %d", uninstallerCmd.ExitCode)
	}

	return nil
}
