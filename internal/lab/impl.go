package lab

import (
	"bytes"
	"compress/gzip"
	_ "embed" // for the vendored manifests
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Implementation is a Gateway API implementation the lab can run.
type Implementation struct {
	// Name is how the implementation is selected and reported.
	Name string
	// Version is the release in the embedded manifest.
	Version string
	// GatewayAPIVersion is the Gateway API bundle that release installs.
	GatewayAPIVersion string
	// ClassName is the GatewayClass the lab creates for it. Every Gateway of
	// the candidate is assigned to this class.
	ClassName string

	// manifest returns the implementation's install manifest.
	manifest func() ([]byte, error)
	// controller is the Deployment that must be available before Gateways
	// are applied.
	controller types.NamespacedName
	// class is the GatewayClass and whatever it refers to, as YAML.
	class string
	// proxyNamespace and proxySelector find the Service in front of the data
	// plane that serves the lab's GatewayClass.
	proxyNamespace string
	proxySelector  string
	// perGateway means each Gateway owns a separate proxy. Its Service is
	// selected by gateway.networking.k8s.io/gateway-name in that namespace.
	perGateway bool
}

// labGatewayClass is the GatewayClass name the lab uses for every implementation.
const labGatewayClass = "routeparity"

// Each implementation has its own class name, matching its controller.
const nginxGatewayClass = "routeparity-nginx"

// envoyGatewayManifest is install.yaml from the Envoy Gateway v1.10.0 release
// (Apache-2.0), gzip-compressed and otherwise unmodified:
//
//	https://github.com/envoyproxy/gateway/releases/download/v1.10.0/install.yaml
//	sha256 f87f4d59b983aa760da52a2a0002c612028106fe69beefdf3d30cfb03905944e
//
// It includes the Gateway API v1.6.1 CRDs. It is embedded so that a lab run
// measures a known version and does not depend on a download.
//
//go:embed manifests/envoy-gateway.yaml.gz
var envoyGatewayManifest []byte

// EnvoyGateway is Envoy Gateway.
//
// Its GatewayClass makes two choices that suit a lab. The proxy Service is a
// ClusterIP, because kind assigns no load balancer addresses and Envoy
// Gateway does not report a Gateway as programmed until its Service has one.
// And all Gateways of the class share one proxy, so there is a single
// address to send probes to however many Gateways the configuration has.
var EnvoyGateway = Implementation{
	Name:              "envoy-gateway",
	Version:           "v1.10.0",
	GatewayAPIVersion: "v1.6.1",
	ClassName:         labGatewayClass,
	manifest:          func() ([]byte, error) { return gunzip(envoyGatewayManifest) },
	controller:        types.NamespacedName{Namespace: "envoy-gateway-system", Name: "envoy-gateway"},
	class: `
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: ` + labGatewayClass + `
  namespace: envoy-gateway-system
spec:
  mergeGateways: true
  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        type: ClusterIP
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: ` + labGatewayClass + `
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: ` + labGatewayClass + `
    namespace: envoy-gateway-system
`,
	proxyNamespace: "envoy-gateway-system",
	proxySelector:  "gateway.envoyproxy.io/owning-gatewayclass=" + labGatewayClass,
}

var implementations = map[string]Implementation{
	EnvoyGateway.Name:       EnvoyGateway,
	NGINXGatewayFabric.Name: NGINXGatewayFabric,
	Istio.Name:              Istio,
}

// istioManifest contains Gateway API v1.6.1 experimental CRDs followed by
// Istio 1.32.0's base and istiod charts, rendered with Helm. There are no
// sidecars, CNI or ambient components; Istio provisions each Gateway's proxy.
// See hack/vendor-istio.sh for sources, checksums and render options.
//
//go:embed manifests/istio.yaml.gz
var istioManifest []byte

// Istio runs a separate Envoy proxy for each Gateway. The defaults ConfigMap
// changes only its Service type, because kind has no external load balancer.
var Istio = Implementation{
	Name:              "istio",
	Version:           "v1.32.0",
	GatewayAPIVersion: "v1.6.1",
	ClassName:         "routeparity-istio",
	manifest:          func() ([]byte, error) { return gunzip(istioManifest) },
	controller:        types.NamespacedName{Namespace: "istio-system", Name: "istiod"},
	class: `apiVersion: v1
kind: ConfigMap
metadata:
  name: routeparity-istio-defaults
  namespace: istio-system
  labels:
    gateway.istio.io/defaults-for-class: routeparity-istio
data:
  service: |
    spec:
      type: ClusterIP
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: routeparity-istio
spec:
  controllerName: istio.io/gateway-controller
`,
	perGateway: true,
}

// nginxGatewayFabricManifest combines these unmodified upstream bundles:
//
// https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml
// sha256 356c2b8e286a964dc20995435659ad1095386329974793af10bbfe3eff977fe5
// https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/v2.8.1/deploy/crds.yaml
// sha256 a652a8cdbade3a7663de77c4b293844f2126ac0a18421310129adac61bf926af
// https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/v2.8.1/deploy/default/deploy.yaml
// sha256 b0fb6c75293da684235e491a39f819d69d3e7fc0ceb08daadd8064b418d8f7b8
//
// The upstream certificate-generator Job provisions control/data-plane mTLS.
// The lab changes the watched class to routeparity-nginx and the proxy Service to
// ClusterIP. No routing or path-normalisation settings are changed.
//
//go:embed manifests/nginx-gateway-fabric.yaml.gz
var nginxGatewayFabricManifest []byte

// NGINXGatewayFabric runs one NGINX proxy per Gateway, unlike Envoy's shared
// proxy. Both HTTP and HTTPS still reach the proxy through the lab's NodePorts.
var NGINXGatewayFabric = Implementation{
	Name:              "nginx-gateway-fabric",
	Version:           "v2.8.1",
	GatewayAPIVersion: "v1.6.3",
	ClassName:         nginxGatewayClass,
	controller:        types.NamespacedName{Namespace: "nginx-gateway", Name: "nginx-gateway"},
	manifest: func() ([]byte, error) {
		data, err := gunzip(nginxGatewayFabricManifest)
		if err != nil {
			return nil, err
		}
		text := strings.ReplaceAll(string(data), "--gatewayclass=nginx", "--gatewayclass="+nginxGatewayClass)
		text = strings.ReplaceAll(text, "  name: nginx\nspec:\n  controllerName:", "  name: "+nginxGatewayClass+"\nspec:\n  controllerName:")
		text = strings.ReplaceAll(text, "type: LoadBalancer", "type: ClusterIP")
		text = strings.ReplaceAll(text, "      externalTrafficPolicy: Local\n", "")
		return []byte(text), nil
	},
	class: `apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: ` + nginxGatewayClass + `
spec:
  controllerName: gateway.nginx.org/nginx-gateway-controller
  parametersRef:
    group: gateway.nginx.org
    kind: NginxProxy
    name: nginx-gateway-proxy-config
    namespace: nginx-gateway
`,
	perGateway: true,
}

// validate rejects incomplete implementations before creating a cluster.
func (i Implementation) validate() error {
	if i.Name == "" || i.Version == "" || i.GatewayAPIVersion == "" || i.ClassName == "" || i.manifest == nil ||
		i.controller.Namespace == "" || i.controller.Name == "" || i.class == "" ||
		(!i.perGateway && (i.proxyNamespace == "" || i.proxySelector == "")) {
		return fmt.Errorf("incomplete Gateway implementation %q", i.Name)
	}
	return nil
}

// ImplementationByName looks an implementation up by name.
func ImplementationByName(name string) (Implementation, bool) {
	impl, ok := implementations[name]
	return impl, ok
}

// ImplementationNames lists the implementations the lab can run.
func ImplementationNames() []string {
	names := make([]string, 0, len(implementations))
	for name := range implementations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func gunzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("embedded manifest: %w", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("embedded manifest: %w", err)
	}
	return out, nil
}
