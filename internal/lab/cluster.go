// Package lab runs routing setups in a local kind cluster, with every backend
// replaced by an echo server, so that their behaviour can be measured without
// touching a real environment.
package lab

import (
	"context"
	_ "embed" // for the vendored manifests
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

// Cluster is a kind cluster the lab controls.
type Cluster struct {
	Name string
	// Config connects to the cluster's API server.
	Config *rest.Config
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
		err = provider.Create(name,
			cluster.CreateWithNodeImage(nodeImage),
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
	return c, nil
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
