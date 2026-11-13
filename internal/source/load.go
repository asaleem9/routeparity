// Package source reads Kubernetes manifests into the objects a parity run needs.
package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// Result is everything read from a set of manifests.
type Result struct {
	Objects model.Objects
	// Ignored counts objects of kinds the tool does not use, by "group/Kind".
	Ignored map[string]int
	// Warnings are problems that did not stop loading, such as an object in
	// an API version that cannot be read.
	Warnings []string
}

// LoadPaths reads every manifest named by paths. A path may be a file, a
// directory (read recursively for .yaml, .yml and .json files), or "-" for
// stdin.
func LoadPaths(paths []string, stdin io.Reader) (*Result, error) {
	if len(paths) == 0 {
		return nil, errors.New("no manifest paths given")
	}
	res := &Result{Ignored: map[string]int{}}
	for _, p := range paths {
		if p == "-" {
			if stdin == nil {
				return nil, errors.New("path \"-\" given but stdin is not available")
			}
			if err := res.decode(stdin, "stdin"); err != nil {
				return nil, err
			}
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := res.decodeFile(p); err != nil {
				return nil, err
			}
			continue
		}
		var files []string
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		for _, f := range files {
			if err := res.decodeFile(f); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

// Load reads manifests from a single stream. The origin names the stream in
// error messages.
func Load(r io.Reader, origin string) (*Result, error) {
	res := &Result{Ignored: map[string]int{}}
	if err := res.decode(r, origin); err != nil {
		return nil, err
	}
	return res, nil
}

func (res *Result) decodeFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	return res.decode(f, path)
}

func (res *Result) decode(r io.Reader, origin string) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for doc := 1; ; doc++ {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: document %d: %w", origin, doc, err)
		}
		if len(obj.Object) == 0 {
			continue // empty document between separators
		}
		if obj.IsList() {
			err := obj.EachListItem(func(item runtime.Object) error {
				u, ok := item.(*unstructured.Unstructured)
				if !ok {
					return fmt.Errorf("list item has unexpected type %T", item)
				}
				return res.add(u, origin)
			})
			if err != nil {
				return fmt.Errorf("%s: document %d: %w", origin, doc, err)
			}
			continue
		}
		if err := res.add(&obj, origin); err != nil {
			return fmt.Errorf("%s: document %d: %w", origin, doc, err)
		}
	}
}

func (res *Result) add(u *unstructured.Unstructured, origin string) error {
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" {
		return errors.New("object has no kind")
	}
	namespace := u.GetNamespace()
	defaultNamespace(&namespace)
	name := namespace + "/" + u.GetName()

	switch {
	case gvk.Group == "networking.k8s.io" && gvk.Kind == "Ingress":
		if gvk.Version != "v1" {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s: Ingress %s uses %s, which is not served by current clusters; skipped", origin, name, gvk.GroupVersion()))
			return nil
		}
		var ing networkingv1.Ingress
		if err := res.convert(u, &ing, origin, "Ingress "+name); err != nil {
			return err
		}
		defaultNamespace(&ing.Namespace)
		res.Objects.Ingresses = append(res.Objects.Ingresses, ing)

	case gvk.Group == "" && gvk.Kind == "Service":
		var svc corev1.Service
		if err := res.convert(u, &svc, origin, "Service "+name); err != nil {
			return err
		}
		defaultNamespace(&svc.Namespace)
		res.Objects.Services = append(res.Objects.Services, svc)

	case gvk.Group == gatewayv1.GroupName && gvk.Kind == "Gateway":
		// v1beta1 and v1 share a schema for the fields used here.
		var gw gatewayv1.Gateway
		if err := res.convert(u, &gw, origin, "Gateway "+name); err != nil {
			return err
		}
		defaultNamespace(&gw.Namespace)
		res.Objects.Gateways = append(res.Objects.Gateways, gw)

	case gvk.Group == gatewayv1.GroupName && gvk.Kind == "HTTPRoute":
		var route gatewayv1.HTTPRoute
		if err := res.convert(u, &route, origin, "HTTPRoute "+name); err != nil {
			return err
		}
		defaultNamespace(&route.Namespace)
		res.Objects.HTTPRoutes = append(res.Objects.HTTPRoutes, route)

	default:
		key := gvk.Kind
		if gvk.Group != "" {
			key = gvk.Group + "/" + gvk.Kind
		}
		res.Ignored[key]++
	}
	return nil
}

// convert fills a typed object. A field the type does not know is reported as
// a warning rather than dropped silently, since a dropped field can change
// routing; it is not an error, because manifests written for a newer API
// version legitimately carry fields this build has not seen.
func (res *Result) convert(u *unstructured.Unstructured, into any, origin, what string) error {
	err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, into, true)
	if err == nil {
		return nil
	}
	if runtime.IsStrictDecodingError(err) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s: %v", origin, what, err))
		return nil
	}
	return fmt.Errorf("%s does not match the schema of %s: %w", what, u.GetAPIVersion(), err)
}

// defaultNamespace mirrors kubectl: an object with no namespace in a file
// lands in "default".
func defaultNamespace(ns *string) {
	if *ns == "" {
		*ns = "default"
	}
}
