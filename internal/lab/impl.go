package lab

import (
	"bytes"
	"compress/gzip"
	_ "embed" // for the vendored manifests
	"fmt"
	"io"
	"sort"

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
}

// labGatewayClass is the GatewayClass name the lab uses for every implementation.
const labGatewayClass = "routeparity"

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
	EnvoyGateway.Name: EnvoyGateway,
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
