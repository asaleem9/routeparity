// Package lab runs routing setups in a local kind cluster, with every backend
// replaced by an echo server, so that their behaviour can be measured without
// touching a real environment.
package lab

import (
	"context"
	_ "embed" // for the vendored manifests
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// Versions of what the lab installs. They are recorded in every snapshot,
// because a result only holds for the versions it was measured on.
const (
	// DefaultClusterName is the kind cluster the lab creates.
	DefaultClusterName = "routeparity-lab"
	// KubernetesVersion is the node version. ingress-nginx's final release is
	// tested up to Kubernetes 1.35, so the lab does not use anything newer.
	KubernetesVersion = "v1.35.8"
	nodeImage         = "kindest/node:" + KubernetesVersion + "@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
	// IngressNGINXVersion is the controller release in the vendored manifest.
	// It is the project's final release.
	IngressNGINXVersion = "v1.15.1"
)

// ingressNGINXManifest is deploy/static/provider/baremetal/deploy.yaml from
// kubernetes/ingress-nginx at controller-v1.15.1 (Apache-2.0), unmodified. It
// is embedded because the repository is archived: nothing guarantees the file
// stays downloadable, and its images are pinned by digest.
//
//go:embed manifests/ingress-nginx.yaml
var ingressNGINXManifest []byte

// Node ports the lab's entry Services listen on inside the cluster. Each is
// published on a host port chosen when the cluster is created.
const (
	nodePortNGINXHTTP    = 30080
	nodePortNGINXHTTPS   = 30443
	nodePortGatewayHTTP  = 31080
	nodePortGatewayHTTPS = 31443
)

// Ports are the host ports, on 127.0.0.1, that lead to the two proxies.
//
// Probes reach the proxies over these plain TCP mappings, not through a
// Kubernetes port-forward. A port-forward closes its whole tunnel when the
// far end resets one connection, and a proxy resetting a connection is one of
// the answers the lab has to be able to record.
type Ports struct {
	NGINXHTTP    int
	NGINXHTTPS   int
	GatewayHTTP  int
	GatewayHTTPS int
}

// portsConfigMap records the host ports in the cluster, so that a later run
// that reuses the cluster can find them.
const (
	portsConfigMap          = "routeparity-lab"
	portsConfigMapNamespace = "kube-system"
)

// Cluster is a kind cluster the lab controls.
type Cluster struct {
	Name string
	// Config connects to the cluster's API server.
	Config *rest.Config
	// Ports are the host ports that lead to the proxies.
	Ports Ports
	// Created is true when this run created the cluster, false when it found
	// one with the same name and reused it.
	Created bool

	provider *cluster.Provider
	// kubeconfigDir holds a kubeconfig that kind writes while creating the
	// cluster. It exists so that kind does not touch the user's own.
	kubeconfigDir string
}

// EnsureCluster returns the named kind cluster, creating it if it does not
// exist. It never reads or writes the user's kubeconfig.
func EnsureCluster(name string) (*Cluster, error) {
	provider := cluster.NewProvider()
	existing, err := provider.List()
	if err != nil {
		return nil, fmt.Errorf("list kind clusters (is Docker running?): %w", err)
	}

	c := &Cluster{Name: name, provider: provider}
	if !slices.Contains(existing, name) {
		dir, err := os.MkdirTemp("", "routeparity-lab-")
		if err != nil {
			return nil, err
		}
		c.kubeconfigDir = dir
		if c.Ports, err = freePorts(); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		mapping := func(nodePort, hostPort int) v1alpha4.PortMapping {
			return v1alpha4.PortMapping{
				ContainerPort: int32(nodePort), HostPort: int32(hostPort),
				ListenAddress: "127.0.0.1", Protocol: v1alpha4.PortMappingProtocolTCP,
			}
		}
		err = provider.Create(name,
			cluster.CreateWithV1Alpha4Config(&v1alpha4.Cluster{
				Nodes: []v1alpha4.Node{{
					Role:  v1alpha4.ControlPlaneRole,
					Image: nodeImage,
					ExtraPortMappings: []v1alpha4.PortMapping{
						mapping(nodePortNGINXHTTP, c.Ports.NGINXHTTP),
						mapping(nodePortNGINXHTTPS, c.Ports.NGINXHTTPS),
						mapping(nodePortGatewayHTTP, c.Ports.GatewayHTTP),
						mapping(nodePortGatewayHTTPS, c.Ports.GatewayHTTPS),
					},
				}},
			}),
			cluster.CreateWithWaitForReady(3*time.Minute),
			// Without an explicit path kind merges the cluster into
			// ~/.kube/config and switches the current context to it.
			cluster.CreateWithKubeconfigPath(filepath.Join(dir, "kubeconfig")),
			cluster.CreateWithDisplayUsage(false),
			cluster.CreateWithDisplaySalutation(false),
		)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("create kind cluster %q: %w", name, err)
		}
		c.Created = true
	}

	kubeconfig, err := provider.KubeConfig(name, false)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig of kind cluster %q: %w", name, err)
	}
	c.Config, err = clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("kubeconfig of kind cluster %q: %w", name, err)
	}

	client, err := kubernetes.NewForConfig(c.Config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if c.Created {
		if err := savePorts(ctx, client, c.Ports); err != nil {
			// Without the record the cluster cannot be reused, and nothing
			// else would ever remove it.
			_ = c.Delete()
			return nil, fmt.Errorf("record the host ports of kind cluster %q: %w", name, err)
		}
		return c, nil
	}
	if c.Ports, err = loadPorts(ctx, client); err != nil {
		return nil, fmt.Errorf("kind cluster %q cannot be reused (%w); delete it with `routeparity lab down --name %s`", name, err, name)
	}
	return c, nil
}

// freePorts asks the kernel for four unused ports. They are held open
// together until all four are known, so that they are distinct.
func freePorts() (Ports, error) {
	var ports [4]int
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return Ports{}, fmt.Errorf("find a free port: %w", err)
		}
		listeners = append(listeners, l)
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	return Ports{NGINXHTTP: ports[0], NGINXHTTPS: ports[1], GatewayHTTP: ports[2], GatewayHTTPS: ports[3]}, nil
}

func savePorts(ctx context.Context, client kubernetes.Interface, ports Ports) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: portsConfigMapNamespace, Name: portsConfigMap},
		Data: map[string]string{
			"nginx-http":    strconv.Itoa(ports.NGINXHTTP),
			"nginx-https":   strconv.Itoa(ports.NGINXHTTPS),
			"gateway-http":  strconv.Itoa(ports.GatewayHTTP),
			"gateway-https": strconv.Itoa(ports.GatewayHTTPS),
		},
	}
	_, err := client.CoreV1().ConfigMaps(portsConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

func loadPorts(ctx context.Context, client kubernetes.Interface) (Ports, error) {
	cm, err := client.CoreV1().ConfigMaps(portsConfigMapNamespace).Get(ctx, portsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Ports{}, errors.New("it was not created by this version of routeparity")
	}
	if err != nil {
		return Ports{}, err
	}
	var ports Ports
	for key, into := range map[string]*int{
		"nginx-http": &ports.NGINXHTTP, "nginx-https": &ports.NGINXHTTPS,
		"gateway-http": &ports.GatewayHTTP, "gateway-https": &ports.GatewayHTTPS,
	} {
		n, err := strconv.Atoi(cm.Data[key])
		if err != nil || n <= 0 {
			return Ports{}, fmt.Errorf("its record of host ports is damaged at %q", key)
		}
		*into = n
	}
	return ports, nil
}

// Delete removes the cluster.
func (c *Cluster) Delete() error {
	return DeleteCluster(c.Name, c.kubeconfigDir)
}

// DeleteCluster removes the named kind cluster. It reports whether a cluster
// of that name does not exist as ErrNoCluster.
func DeleteCluster(name, kubeconfigDir string) error {
	provider := cluster.NewProvider()
	existing, err := provider.List()
	if err != nil {
		return fmt.Errorf("list kind clusters (is Docker running?): %w", err)
	}
	if !slices.Contains(existing, name) {
		return ErrNoCluster
	}

	// kind removes the cluster's entry from a kubeconfig on delete. Point it
	// at a file of our own so that it leaves the user's alone.
	dir := kubeconfigDir
	if dir == "" {
		dir, err = os.MkdirTemp("", "routeparity-lab-")
		if err != nil {
			return err
		}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := provider.Delete(name, filepath.Join(dir, "kubeconfig")); err != nil {
		return fmt.Errorf("delete kind cluster %q: %w", name, err)
	}
	return nil
}

// ErrNoCluster means the named kind cluster does not exist.
var ErrNoCluster = errors.New("no such kind cluster")

// LoadLocalImage copies an image from the local Docker daemon into the
// cluster's nodes. It reports false, without error, when Docker has no such
// image; the cluster then pulls the image from its registry as usual.
func (c *Cluster) LoadLocalImage(ctx context.Context, image string) (bool, error) {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil // Docker answered: not present
		}
		return false, nil // no Docker CLI: nothing local to load
	}

	archive, err := os.CreateTemp("", "routeparity-image-*.tar")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(archive.Name()) }()
	_ = archive.Close()
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", archive.Name(), image).CombinedOutput(); err != nil {
		return false, fmt.Errorf("docker save %s: %w: %s", image, err, out)
	}

	nodes, err := c.provider.ListInternalNodes(c.Name)
	if err != nil {
		return false, fmt.Errorf("list nodes of kind cluster %q: %w", c.Name, err)
	}
	for _, node := range nodes {
		f, err := os.Open(archive.Name())
		if err != nil {
			return false, err
		}
		err = nodeutils.LoadImageArchive(node, f)
		_ = f.Close()
		if err != nil {
			return false, fmt.Errorf("load %s into node %s: %w", image, node.String(), err)
		}
	}
	return true, nil
}
