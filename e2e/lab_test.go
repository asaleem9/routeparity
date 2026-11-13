//go:build e2e

// Package e2e runs the lab against a real kind cluster. It needs Docker and
// the echo image, and takes a minute or two:
//
//	make e2e
//
// ROUTEPARITY_E2E_CLUSTER names the cluster (default routeparity-e2e) and
// ROUTEPARITY_E2E_KEEP=1 leaves it running, which makes a second run fast.
package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asaleem9/routeparity/internal/lab"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/probe"
	"github.com/asaleem9/routeparity/internal/prober"
	"github.com/asaleem9/routeparity/internal/source"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func loadWorld(t *testing.T, manifests string) *model.World {
	t.Helper()
	res, err := source.Load(strings.NewReader(manifests), "test")
	if err != nil {
		t.Fatal(err)
	}
	return model.NewWorld(res.Objects, model.WorldOptions{})
}

func record(t *testing.T, w *model.World, keep bool) *model.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	snap, err := lab.RecordBaseline(ctx, w, lab.Options{
		ClusterName: env("ROUTEPARITY_E2E_CLUSTER", "routeparity-e2e"),
		EchoImage:   env("ROUTEPARITY_E2E_ECHO_IMAGE", "routeparity-echo:dev"),
		Keep:        keep,
		Probes:      probe.Options{Mutations: true},
		Send:        prober.Options{Concurrency: 8},
		ToolVersion: "e2e",
		Progress:    func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("RecordBaseline: %v", err)
	}
	return snap
}

// answer is what ingress-nginx did with one request.
type answer struct {
	status   int
	location string
	// backend is the Service that received the request, or "" when none did.
	backend string
	// received is the request-target the backend saw.
	received string
}

func answers(t *testing.T, snap *model.Snapshot) map[string]answer {
	t.Helper()
	probes := map[string]model.Probe{}
	for _, p := range snap.Probes {
		probes[p.ID] = p
	}
	out := map[string]answer{}
	for _, obs := range snap.Observations {
		p := probes[obs.ProbeID]
		if obs.Client.Error != "" {
			t.Errorf("%s://%s%s got no response: %s", p.Scheme, p.Host, p.RawTarget, obs.Client.Error)
			continue
		}
		a := answer{status: obs.Client.Status}
		if loc := model.HeaderValues(obs.Client.Headers, "Location"); len(loc) > 0 {
			a.location = loc[0]
		}
		if obs.Backend != nil {
			a.backend, a.received = obs.Backend.Service, obs.Backend.RawTarget
		}
		out[p.Scheme+"://"+p.Host+p.RawTarget] = a
	}
	return out
}

// TestFiveBehaviours reproduces, on real ingress-nginx, each behaviour from
// "Before You Migrate: Five Surprising Ingress-NGINX Behaviors You Need to
// Know" using the example manifests in this repository. These are the facts
// the audit rules assert; if ingress-nginx did not behave this way the rules
// would be wrong.
func TestFiveBehaviours(t *testing.T) {
	data, err := os.ReadFile("../examples/five-behaviours/ingress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	snap := record(t, loadWorld(t, string(data)), true)
	got := answers(t, snap)

	routedTo := func(t *testing.T, request, service, received string) {
		t.Helper()
		a, ok := got[request]
		switch {
		case !ok:
			t.Errorf("%s: no observation", request)
		case a.status != 200 || a.backend != service || a.received != received:
			t.Errorf("%s: status %d, backend %q received %q; want 200 from %q receiving %q",
				request, a.status, a.backend, a.received, service, received)
		}
	}
	redirected := func(t *testing.T, request string, status int, location string) {
		t.Helper()
		a, ok := got[request]
		switch {
		case !ok:
			t.Errorf("%s: no observation", request)
		case a.status != status || a.location != location || a.backend != "":
			t.Errorf("%s: status %d to %q (backend %q); want %d to %q with no backend",
				request, a.status, a.location, a.backend, status, location)
		}
	}
	notRouted := func(t *testing.T, request string) {
		t.Helper()
		a, ok := got[request]
		switch {
		case !ok:
			t.Errorf("%s: no observation", request)
		case a.status != 404 || a.backend != "":
			t.Errorf("%s: status %d, backend %q; want 404 with no backend", request, a.status, a.backend)
		}
	}

	t.Run("1 regex paths match case-insensitively and as a prefix", func(t *testing.T) {
		// The Ingress path is /[A-Z]{3}.
		routedTo(t, "http://regex-match.example.com/AAA", "httpbin", "/AAA")
		routedTo(t, "http://regex-match.example.com/aaa", "httpbin", "/aaa")
		routedTo(t, "http://regex-match.example.com/AAA/rp-extra", "httpbin", "/AAA/rp-extra")
	})

	t.Run("2 regex mode applies to every path on the host", func(t *testing.T) {
		// /Header is an Exact path in an Ingress with no annotations. It
		// shares a host with the Ingress above.
		routedTo(t, "http://regex-match.example.com/Header", "httpbin", "/Header")
		routedTo(t, "http://regex-match.example.com/hEADER", "httpbin", "/hEADER")
		routedTo(t, "http://regex-match.example.com/Header/rp-extra", "httpbin", "/Header/rp-extra")
	})

	t.Run("3 rewrite-target turns on regex matching", func(t *testing.T) {
		// /IP is an Exact path whose Ingress sets rewrite-target: /uuid.
		routedTo(t, "http://rewrite-target.example.com/IP", "httpbin", "/uuid")
		routedTo(t, "http://rewrite-target.example.com/ip", "httpbin", "/uuid")
		routedTo(t, "http://rewrite-target.example.com/IP/rp-extra", "httpbin", "/uuid")
	})

	t.Run("4 a path ending in a slash redirects the slashless request", func(t *testing.T) {
		routedTo(t, "http://trailing-slash.example.com/my-path/", "httpbin", "/my-path/")
		redirected(t, "http://trailing-slash.example.com/my-path", 301, "http://trailing-slash.example.com/my-path/")
		// On a host that is not in regex mode, Exact stays case-sensitive.
		notRouted(t, "http://trailing-slash.example.com/MY-PATH/")
	})

	t.Run("5 paths are normalised before matching, and forwarded as sent", func(t *testing.T) {
		routedTo(t, "http://trailing-slash.example.com/rp-x/../my-path/", "httpbin", "/rp-x/../my-path/")
		routedTo(t, "http://trailing-slash.example.com//my-path/", "httpbin", "//my-path/")
		routedTo(t, "http://trailing-slash.example.com/./my-path/", "httpbin", "/./my-path/")
		routedTo(t, "http://trailing-slash.example.com/%6Dy-path/", "httpbin", "/%6Dy-path/")
	})

	t.Run("a host with TLS redirects plain HTTP and routes HTTPS", func(t *testing.T) {
		redirected(t, "http://path-normalization.example.com/uuid", 308, "https://path-normalization.example.com/uuid")
		routedTo(t, "https://path-normalization.example.com/uuid", "httpbin", "/uuid")
		// Variations go over HTTPS, where the path is actually matched.
		routedTo(t, "https://path-normalization.example.com/rp-x/../uuid", "httpbin", "/rp-x/../uuid")
		notRouted(t, "https://path-normalization.example.com/UUID")
	})

	t.Run("an unknown host is not routed", func(t *testing.T) {
		notRouted(t, "http://unrouted.routeparity.invalid/rp-unrouted-control")
	})

	t.Run("what the lab cannot reproduce is listed", func(t *testing.T) {
		features := map[string]bool{}
		for _, u := range snap.Unverified {
			features[strings.TrimPrefix(u.Feature, model.NGINXAnnotationPrefix)] = true
		}
		// The admin Ingress is applied without these, so its paths route.
		for _, want := range []string{"auth-url", "limit-rps"} {
			if !features[want] {
				t.Errorf("%s is not listed as unverified; listed: %v", want, features)
			}
		}
		routedTo(t, "http://admin.example.com/", "admin", "/")
		for _, u := range snap.Unverified {
			if u.Reason == lab.ReasonNotLoaded {
				t.Errorf("the lab reports that something never loaded: %+v", u)
			}
		}
	})

	t.Run("the snapshot says what it was measured on", func(t *testing.T) {
		m := snap.Meta
		if m.Mode != model.ModeLab || m.Kind != model.TargetIngressNGINX {
			t.Errorf("mode %q, kind %q", m.Mode, m.Kind)
		}
		if m.Versions["ingress-nginx"] != lab.IngressNGINXVersion || m.Versions["kubernetes"] != lab.KubernetesVersion {
			t.Errorf("versions = %v", m.Versions)
		}
	})
}

// TestRegexModeComesFromTheOtherIngress runs in the cluster the first test
// left behind, with the regex Ingress removed. It shows two things at once:
// the leak in behaviour 2 really is caused by the other Ingress, and a reused
// cluster does not keep serving the previous run's Ingresses.
func TestRegexModeComesFromTheOtherIngress(t *testing.T) {
	const bystanderOnly = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex-match-other
  namespace: demo
spec:
  ingressClassName: nginx
  rules:
  - host: regex-match.example.com
    http:
      paths:
      - path: "/Header"
        pathType: Exact
        backend:
          service: {name: httpbin, port: {number: 8000}}
`
	keep := os.Getenv("ROUTEPARITY_E2E_KEEP") == "1"
	got := answers(t, record(t, loadWorld(t, bystanderOnly), keep))

	if a := got["http://regex-match.example.com/Header"]; a.status != 200 || a.backend != "httpbin" {
		t.Errorf("/Header: status %d, backend %q; want it routed", a.status, a.backend)
	}
	// Without the Ingress that set use-regex, the same Exact path no longer
	// matches in a different case.
	if a := got["http://regex-match.example.com/hEADER"]; a.status != 404 || a.backend != "" {
		t.Errorf("/hEADER: status %d, backend %q; want 404 now that nothing puts the host in regex mode", a.status, a.backend)
	}
	// /AAA belonged to the Ingress from the previous run. It was not asked
	// for this time; that it is absent from the probes is expected, and that
	// the host is out of regex mode shows the old Ingress is gone.
	if _, asked := got["http://regex-match.example.com/AAA"]; asked {
		t.Error("a probe for the previous run's Ingress was generated")
	}
}
