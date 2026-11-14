package convert

import (
	"bytes"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/asaleem9/routeparity/internal/model"
)

// Manifests renders the World's selected Ingresses, and the Services they may
// reference by port name, as the multi-document YAML stream a Converter reads.
func Manifests(w *model.World) ([]byte, error) {
	var buf bytes.Buffer
	write := func(obj any) error {
		doc, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		buf.WriteString("---\n")
		buf.Write(doc)
		return nil
	}
	for i := range w.Ingresses {
		ing := w.Ingresses[i]
		ing.APIVersion = networkingv1.SchemeGroupVersion.String()
		ing.Kind = "Ingress"
		if err := write(&ing); err != nil {
			return nil, fmt.Errorf("marshal %s: %w", model.IngressRef(&ing), err)
		}
	}

	names := make([]string, 0, len(w.Services))
	byName := map[string]corev1.Service{}
	for key, svc := range w.Services {
		names = append(names, key.String())
		byName[key.String()] = svc
	}
	sort.Strings(names)
	for _, name := range names {
		svc := byName[name]
		svc.APIVersion = "v1"
		svc.Kind = "Service"
		if err := write(&svc); err != nil {
			return nil, fmt.Errorf("marshal Service %s: %w", name, err)
		}
	}
	return buf.Bytes(), nil
}
