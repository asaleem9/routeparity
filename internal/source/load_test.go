package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ingressDoc = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
spec:
  ingressClassName: nginx
  rules:
  - host: example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web
            port:
              number: 80
`

func TestLoadReadsEveryKindItUses(t *testing.T) {
	manifests := ingressDoc + `
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: gw
  namespace: shop
spec:
  gatewayClassName: envoy
  listeners:
  - name: http
    port: 80
    protocol: HTTP
---
# An empty document between separators is fine.
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: route
  namespace: shop
spec:
  hostnames: ["example.com"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
`
	res, err := Load(strings.NewReader(manifests), "test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	o := res.Objects
	if len(o.Ingresses) != 1 || len(o.Services) != 1 || len(o.Gateways) != 1 || len(o.HTTPRoutes) != 1 {
		t.Fatalf("got %d Ingresses, %d Services, %d Gateways, %d HTTPRoutes; want 1 of each",
			len(o.Ingresses), len(o.Services), len(o.Gateways), len(o.HTTPRoutes))
	}
	if got := o.Ingresses[0].Namespace; got != "default" {
		t.Errorf("Ingress with no namespace landed in %q, want default", got)
	}
	if got := o.Services[0].Namespace; got != "shop" {
		t.Errorf("Service namespace = %q, want shop", got)
	}
	if res.Ignored["apps/Deployment"] != 1 || res.Ignored["ConfigMap"] != 1 {
		t.Errorf("Ignored = %v, want one apps/Deployment and one ConfigMap", res.Ignored)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
}

func TestLoadUnpacksLists(t *testing.T) {
	// The shape `kubectl get ingress,service -o json` produces.
	list := `{
	  "apiVersion": "v1", "kind": "List",
	  "items": [
	    {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": {"name": "a", "namespace": "x", "creationTimestamp": "2025-03-01T10:00:00Z"}},
	    {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": {"name": "b", "namespace": "x"}},
	    {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "s", "namespace": "x"}}
	  ]
	}`
	res, err := Load(strings.NewReader(list), "test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Objects.Ingresses) != 2 || len(res.Objects.Services) != 1 {
		t.Fatalf("got %d Ingresses and %d Services, want 2 and 1", len(res.Objects.Ingresses), len(res.Objects.Services))
	}
	if res.Objects.Ingresses[0].CreationTimestamp.IsZero() {
		t.Error("creationTimestamp of an exported Ingress was lost; precedence depends on it")
	}
}

func TestLoadWarnsInsteadOfDroppingSilently(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		manifest := strings.Replace(ingressDoc, "ingressClassName: nginx", "ingressClassName: nginx\n  ingressClasName: typo", 1)
		res, err := Load(strings.NewReader(manifest), "test")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(res.Objects.Ingresses) != 1 {
			t.Fatalf("Ingress with an unknown field was not loaded")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ingressClasName") {
			t.Errorf("warnings = %v, want one naming the unknown field", res.Warnings)
		}
	})
	t.Run("removed API version", func(t *testing.T) {
		manifest := strings.Replace(ingressDoc, "networking.k8s.io/v1", "networking.k8s.io/v1beta1", 1)
		res, err := Load(strings.NewReader(manifest), "test")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(res.Objects.Ingresses) != 0 {
			t.Error("a v1beta1 Ingress was loaded as if it were v1")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "v1beta1") {
			t.Errorf("warnings = %v, want one naming the API version", res.Warnings)
		}
	})
}

func TestLoadReportsWhereItFailed(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{"malformed YAML", ingressDoc + "\n---\nkind: [unclosed", "document 2"},
		{"object without a kind", "metadata:\n  name: x\n", "'Kind' is missing"},
		{"wrong type for a field", strings.Replace(ingressDoc, "number: 80", "number: eighty", 1),
			"Ingress default/web does not match the schema of networking.k8s.io/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.manifest), "input.yaml")
			if err == nil {
				t.Fatal("Load accepted the input")
			}
			if !strings.Contains(err.Error(), "input.yaml") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name input.yaml and %q", err, tt.want)
			}
		})
	}
}

func TestLoadPaths(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	named := func(name string) string {
		return strings.Replace(ingressDoc, "name: web\nspec", "name: "+name+"\nspec", 1)
	}
	write("a.yaml", named("from-yaml"))
	write("nested/b.yml", named("from-yml"))
	write("notes.txt", "not a manifest")

	t.Run("directory, recursively, manifests only", func(t *testing.T) {
		res, err := LoadPaths([]string{dir}, nil)
		if err != nil {
			t.Fatalf("LoadPaths: %v", err)
		}
		if len(res.Objects.Ingresses) != 2 {
			t.Errorf("got %d Ingresses, want 2", len(res.Objects.Ingresses))
		}
	})
	t.Run("file and stdin together", func(t *testing.T) {
		res, err := LoadPaths([]string{filepath.Join(dir, "a.yaml"), "-"}, strings.NewReader(named("from-stdin")))
		if err != nil {
			t.Fatalf("LoadPaths: %v", err)
		}
		if len(res.Objects.Ingresses) != 2 {
			t.Errorf("got %d Ingresses, want 2", len(res.Objects.Ingresses))
		}
	})
	t.Run("errors", func(t *testing.T) {
		if _, err := LoadPaths(nil, nil); err == nil {
			t.Error("no paths: want an error")
		}
		if _, err := LoadPaths([]string{filepath.Join(dir, "missing.yaml")}, nil); err == nil {
			t.Error("missing file: want an error")
		}
		if _, err := LoadPaths([]string{"-"}, nil); err == nil {
			t.Error("stdin requested but unavailable: want an error")
		}
	})
}
