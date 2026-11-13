package convert

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/notifications"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/providers/ingressnginx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/asaleem9/routeparity/internal/model"

	// Emitters register themselves on import.
	_ "github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/emitters/envoygateway"
	_ "github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/emitters/standard"
)

const i2gwModule = "github.com/kubernetes-sigs/ingress2gateway"

// InProcess converts with the ingress2gateway library linked into this
// binary. The version is fixed at build time and reported in Output.Converter.
type InProcess struct{}

var _ Converter = InProcess{}

// Convert implements Converter.
func (InProcess) Convert(ctx context.Context, manifests io.Reader, opts Options) (*Output, error) {
	class := opts.IngressClass
	if class == "" {
		class = ingressnginx.NginxIngressClass
	}
	emitter := opts.Emitter
	if emitter == "" {
		emitter = "standard"
	}
	// The class default lives in ingress2gateway's command-line flags, not in
	// the library, so it has to be passed explicitly: without it the provider
	// selects no Ingresses.
	flags := map[string]map[string]string{
		ingressnginx.Name: {ingressnginx.NginxIngressClassFlag: class},
	}

	out := &Output{Converter: i2gwModule + "@" + i2gwVersion()}
	resources, report, err := i2gw.ToGatewayAPIResources(
		ctx, "", manifests, []string{ingressnginx.Name}, emitter, flags, opts.AllowExperimental, true)
	out.Notices = notices(report)
	if err != nil {
		return out, fmt.Errorf("ingress2gateway: %w", err)
	}

	for _, r := range resources {
		for _, key := range sortedKeys(r.Gateways) {
			gw := r.Gateways[key]
			gw.SetGroupVersionKind(kindIn(gatewayv1.GroupVersion, "Gateway"))
			out.Gateways = append(out.Gateways, gw)
		}
		for _, key := range sortedKeys(r.HTTPRoutes) {
			route := r.HTTPRoutes[key]
			route.SetGroupVersionKind(kindIn(gatewayv1.GroupVersion, "HTTPRoute"))
			out.HTTPRoutes = append(out.HTTPRoutes, route)
		}

		var convErr error
		other := func(obj runtime.Object, gvk schema.GroupVersionKind) {
			u, err := toUnstructured(obj, gvk)
			if err != nil && convErr == nil {
				convErr = err
			}
			if err == nil {
				out.Other = append(out.Other, *u)
			}
		}
		for _, key := range sortedKeys(r.GatewayClasses) {
			o := r.GatewayClasses[key]
			other(&o, kindIn(gatewayv1.GroupVersion, "GatewayClass"))
		}
		for _, key := range sortedKeys(r.GRPCRoutes) {
			o := r.GRPCRoutes[key]
			other(&o, kindIn(gatewayv1.GroupVersion, "GRPCRoute"))
		}
		for _, key := range sortedKeys(r.TLSRoutes) {
			o := r.TLSRoutes[key]
			other(&o, kindIn(gatewayv1.GroupVersion, "TLSRoute"))
		}
		for _, key := range sortedKeys(r.TCPRoutes) {
			o := r.TCPRoutes[key]
			other(&o, kindIn(gatewayv1alpha2.GroupVersion, "TCPRoute"))
		}
		for _, key := range sortedKeys(r.UDPRoutes) {
			o := r.UDPRoutes[key]
			other(&o, kindIn(gatewayv1alpha2.GroupVersion, "UDPRoute"))
		}
		for _, key := range sortedKeys(r.BackendTLSPolicies) {
			o := r.BackendTLSPolicies[key]
			other(&o, kindIn(gatewayv1.GroupVersion, "BackendTLSPolicy"))
		}
		for _, key := range sortedKeys(r.ReferenceGrants) {
			o := r.ReferenceGrants[key]
			other(&o, kindIn(gatewayv1beta1.GroupVersion, "ReferenceGrant"))
		}
		if convErr != nil {
			return out, fmt.Errorf("ingress2gateway output: %w", convErr)
		}
		out.Other = append(out.Other, r.GatewayExtensions...)
	}
	return out, nil
}

// kindIn builds a GroupVersionKind from the group and version a Gateway API
// package declares for itself.
func kindIn(gv metav1.GroupVersion, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: kind}
}

func sortedKeys[V any](m map[types.NamespacedName]V) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func toUnstructured(obj runtime.Object, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gvk.Kind, err)
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

func notices(report *notifications.Report) []Notice {
	bySource := report.Notifications()
	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	var out []Notice
	for _, source := range sources {
		for _, n := range bySource[source] {
			notice := Notice{Level: string(n.Type), Source: source, Message: n.Message}
			for _, obj := range n.CallingObjects {
				if obj == nil {
					continue
				}
				notice.Objects = append(notice.Objects, model.ObjectRef{
					Kind:      kindOf(obj),
					Namespace: obj.GetNamespace(),
					Name:      obj.GetName(),
				})
			}
			out = append(out, notice)
		}
	}
	return out
}

// kindOf names an object's kind. Typed objects built in memory usually carry
// no TypeMeta, so the Go type name is the fallback.
func kindOf(obj client.Object) string {
	if kind := obj.GetObjectKind().GroupVersionKind().Kind; kind != "" {
		return kind
	}
	name := fmt.Sprintf("%T", obj)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// i2gwVersion reports the ingress2gateway version this binary was built
// against, read from the module information the Go linker embeds.
func i2gwVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == i2gwModule {
			if dep.Replace != nil {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}
