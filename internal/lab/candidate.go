package lab

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// Candidate is a Gateway API configuration to run next to the Ingresses.
type Candidate struct {
	Gateways   []gatewayv1.Gateway
	HTTPRoutes []gatewayv1.HTTPRoute
	// Other holds everything else that belongs to the configuration, such as
	// GRPCRoutes, ReferenceGrants and implementation policies. It is applied
	// as given.
	Other []unstructured.Unstructured
	// Source says where the configuration came from, for the report: the
	// user's manifests, or a converter and its version.
	Source string
	// Unverified lists what the source itself reported as untranslated.
	Unverified []model.Unverified
}

// Empty reports whether the candidate routes nothing.
func (c *Candidate) Empty() bool {
	return c == nil || (len(c.Gateways) == 0 && len(c.HTTPRoutes) == 0)
}

// backends records every Service the candidate's HTTPRoutes send traffic to
// and every certificate its Gateways present, so that the lab provides them.
// It returns what the candidate refers to that the lab cannot stand in for.
func (c *Candidate) backends(set *backendSet, wantSecret func(types.NamespacedName, ...string), namespaces map[string]bool) []model.Unverified {
	var unverified []model.Unverified
	for i := range c.Gateways {
		gw := &c.Gateways[i]
		namespaces[gw.Namespace] = true
		for _, l := range gw.Spec.Listeners {
			if l.TLS == nil {
				continue
			}
			host := ""
			if l.Hostname != nil {
				host = string(*l.Hostname)
			}
			for _, ref := range l.TLS.CertificateRefs {
				if ref.Kind != nil && *ref.Kind != "Secret" {
					continue
				}
				ns := gw.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace)
				}
				wantSecret(types.NamespacedName{Namespace: ns, Name: string(ref.Name)}, host)
			}
		}
	}

	for i := range c.HTTPRoutes {
		route := &c.HTTPRoutes[i]
		namespaces[route.Namespace] = true
		routeRef := model.ObjectRef{Kind: "HTTPRoute", Namespace: route.Namespace, Name: route.Name}
		add := func(ref gatewayv1.BackendObjectReference) {
			isService := (ref.Kind == nil || *ref.Kind == "Service") && (ref.Group == nil || *ref.Group == "" || *ref.Group == "core")
			if !isService {
				kind := "an object"
				if ref.Kind != nil {
					kind = "a " + string(*ref.Kind)
				}
				unverified = append(unverified, model.Unverified{
					Reason:  model.ReasonUnsupported,
					Feature: fmt.Sprintf("backend %s is %s", ref.Name, kind),
					Object:  &routeRef,
					Detail:  "only Service backends are replaced by the echo",
				})
				return
			}
			if ref.Port == nil {
				return // invalid for a Service; the implementation reports it
			}
			ns := route.Namespace
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}
			set.add(ns, string(ref.Name), "", *ref.Port)
		}
		for _, rule := range route.Spec.Rules {
			for _, b := range rule.BackendRefs {
				add(b.BackendObjectReference)
			}
			for _, f := range rule.Filters {
				if f.RequestMirror != nil {
					add(f.RequestMirror.BackendRef)
				}
			}
		}
	}
	for i := range c.Other {
		if ns := c.Other[i].GetNamespace(); ns != "" {
			namespaces[ns] = true
		}
	}
	return unverified
}

// labObjects returns the candidate as the lab applies it, in order: Gateways
// assigned to the lab's GatewayClass, then everything else, then HTTPRoutes.
// It also returns the names of the Gateways and HTTPRoutes, whose status the
// lab checks afterwards.
func (c *Candidate) labObjects(className string) (objs []*unstructured.Unstructured, gateways, routes []types.NamespacedName, err error) {
	for i := range c.Gateways {
		gw := c.Gateways[i].DeepCopy()
		gw.TypeMeta = metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway"}
		gw.Spec.GatewayClassName = gatewayv1.ObjectName(className)
		// Requested addresses belong to the cluster the manifest came from.
		gw.Spec.Addresses = nil
		gw.Status = gatewayv1.GatewayStatus{}
		u, err := cleaned(gw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("Gateway %s/%s: %w", gw.Namespace, gw.Name, err)
		}
		objs = append(objs, u)
		gateways = append(gateways, types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name})
	}
	for i := range c.Other {
		u := c.Other[i].DeepCopy()
		switch u.GetKind() {
		case "GatewayClass", "EnvoyProxy":
			// The lab runs its own class and proxy settings.
			continue
		}
		stripMetadata(u)
		objs = append(objs, u)
	}
	for i := range c.HTTPRoutes {
		route := c.HTTPRoutes[i].DeepCopy()
		route.TypeMeta = metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute"}
		route.Status = gatewayv1.HTTPRouteStatus{}
		u, err := cleaned(route)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
		objs = append(objs, u)
		routes = append(routes, types.NamespacedName{Namespace: route.Namespace, Name: route.Name})
	}
	return objs, gateways, routes, nil
}

func cleaned(obj runtime.Object) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: content}
	stripMetadata(u)
	return u, nil
}

// stripMetadata removes what a cluster export carries and a fresh apply must
// not: identity assigned by the source cluster, and status.
func stripMetadata(u *unstructured.Unstructured) {
	for _, field := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields", "generation", "selfLink", "ownerReferences", "finalizers"} {
		unstructured.RemoveNestedField(u.Object, "metadata", field)
	}
	unstructured.RemoveNestedField(u.Object, "status")
	annotations := u.GetAnnotations()
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	if len(annotations) == 0 {
		unstructured.RemoveNestedField(u.Object, "metadata", "annotations")
	} else {
		u.SetAnnotations(annotations)
	}
}
