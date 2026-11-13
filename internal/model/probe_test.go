package model

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestSealFillsDefaultsAndAssignsID(t *testing.T) {
	p := Probe{Host: "example.com", RawTarget: "/a"}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if p.Method != "GET" || p.Scheme != "http" || p.Repeat != 1 || p.Safety != SafetySafe {
		t.Errorf("defaults = %s %s repeat=%d safety=%s, want GET http repeat=1 safety=safe", p.Method, p.Scheme, p.Repeat, p.Safety)
	}
	if !strings.HasPrefix(p.ID, "p-") || len(p.ID) != 18 {
		t.Errorf("ID = %q, want p- followed by 16 hex digits", p.ID)
	}
}

func TestSealKeepsTheRawTargetVerbatim(t *testing.T) {
	// Normalisation is a behaviour under test, so nothing may clean the target.
	for _, target := range []string{"/a/../b", "//a", "/a/./b", "/%2e%2e/a", "/a%2Fb", "/a?x=1&y=%20"} {
		p := Probe{Host: "example.com", RawTarget: target}
		if err := p.Seal(); err != nil {
			t.Errorf("Seal(%q): %v", target, err)
			continue
		}
		if p.RawTarget != target {
			t.Errorf("RawTarget = %q, want %q unchanged", p.RawTarget, target)
		}
	}
}

func TestDefaultSafety(t *testing.T) {
	tests := []struct {
		name  string
		probe Probe
		want  Safety
	}{
		{"derived GET", Probe{Method: "GET", Origin: Origin{Source: OriginDerived}}, SafetySafe},
		{"mutated GET", Probe{Method: "GET", Origin: Origin{Source: OriginMutation}}, SafetyIntrusive},
		{"OPTIONS preflight", Probe{Method: "OPTIONS"}, SafetySafe},
		{"POST", Probe{Method: "POST"}, SafetyUnsafe},
		{"GET with a body", Probe{Method: "GET", Body: &BodySpec{Size: 1}}, SafetyUnsafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.probe
			p.Host, p.RawTarget = "example.com", "/"
			if err := p.Seal(); err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if p.Safety != tt.want {
				t.Errorf("Safety = %q, want %q", p.Safety, tt.want)
			}
		})
	}
}

func TestValidateRejectsRequestsThatWouldSplit(t *testing.T) {
	valid := Probe{Method: "GET", Scheme: "http", Host: "example.com", RawTarget: "/"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid probe rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Probe)
	}{
		{"empty host", func(p *Probe) { p.Host = "" }},
		{"host with a path", func(p *Probe) { p.Host = "example.com/x" }},
		{"empty target", func(p *Probe) { p.RawTarget = "" }},
		{"relative target", func(p *Probe) { p.RawTarget = "a/b" }},
		{"target with a space", func(p *Probe) { p.RawTarget = "/a b" }},
		{"target with CRLF", func(p *Probe) { p.RawTarget = "/a\r\nX-Injected: 1" }},
		{"method with a space", func(p *Probe) { p.Method = "GET /x" }},
		{"unknown scheme", func(p *Probe) { p.Scheme = "ftp" }},
		{"header name with a colon", func(p *Probe) { p.Headers = []Header{{Name: "X:Y", Value: "1"}} }},
		{"header value with a line break", func(p *Probe) { p.Headers = []Header{{Name: "X", Value: "1\r\nY: 2"}} }},
		{"negative body", func(p *Probe) { p.Body = &BodySpec{Size: -1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid
			tt.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Error("Validate accepted the probe")
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	base := Probe{Method: "GET", Scheme: "http", Host: "example.com", RawTarget: "/a"}

	t.Run("ignores why the probe exists", func(t *testing.T) {
		other := base
		other.Origin = Origin{Source: OriginMutation, RuleID: "NGX-REGEX-001"}
		other.Repeat = 50
		other.Safety = SafetyIntrusive
		if base.Fingerprint() != other.Fingerprint() {
			t.Error("fingerprint changed with origin, repeat or safety")
		}
	})
	t.Run("ignores host and header name case", func(t *testing.T) {
		a, b := base, base
		a.Headers = []Header{{Name: "X-Canary", Value: "always"}}
		b.Headers = []Header{{Name: "x-canary", Value: "always"}}
		b.Host = "EXAMPLE.com"
		if a.Fingerprint() != b.Fingerprint() {
			t.Error("fingerprint is sensitive to host or header name case")
		}
	})
	t.Run("distinguishes what goes on the wire", func(t *testing.T) {
		variants := map[string]func(*Probe){
			"method":       func(p *Probe) { p.Method = "HEAD" },
			"scheme":       func(p *Probe) { p.Scheme = "https" },
			"host":         func(p *Probe) { p.Host = "other.example.com" },
			"target case":  func(p *Probe) { p.RawTarget = "/A" },
			"target slash": func(p *Probe) { p.RawTarget = "/a/" },
			"header":       func(p *Probe) { p.Headers = []Header{{Name: "X", Value: "1"}} },
			"header value": func(p *Probe) { p.Headers = []Header{{Name: "X", Value: "One"}} },
			"body":         func(p *Probe) { p.Body = &BodySpec{Size: 10} },
		}
		seen := map[string]string{base.Fingerprint(): "base"}
		for name, mutate := range variants {
			p := base
			mutate(&p)
			fp := p.Fingerprint()
			if prev, dup := seen[fp]; dup {
				t.Errorf("%s has the same fingerprint as %s", name, prev)
			}
			seen[fp] = name
		}
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a, b := base, base
		a.Host, a.RawTarget = "example.com", "/ab"
		b.Host, b.RawTarget = "example.com/a", "b"
		if a.Fingerprint() == b.Fingerprint() {
			t.Error("moving a byte between fields left the fingerprint unchanged")
		}
	})
}

func TestSnapshotRoundTrip(t *testing.T) {
	probe := Probe{Host: "example.com", RawTarget: "/a/../b", Origin: Origin{Source: OriginMutation, RuleID: "NGX-NORM-001"}}
	if err := probe.Seal(); err != nil {
		t.Fatal(err)
	}
	want := &Snapshot{
		Meta: TargetMeta{
			Name: "old", Kind: TargetIngressNGINX, Mode: ModeLab,
			Versions:    map[string]string{"ingress-nginx": "v1.15.1"},
			ToolVersion: "test", RecordedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		},
		Probes: []Probe{probe},
		Observations: []Observation{{
			ProbeID: probe.ID, Attempt: 1,
			Client:  ClientView{Status: 200, Proto: "HTTP/1.1", Headers: []Header{{Name: "Content-Type", Value: "text/plain"}}, BodyLen: 2},
			Backend: &BackendView{Namespace: "demo", Service: "web", Method: "GET", RawTarget: "/b", Host: "example.com"},
		}},
		Unverified: []Unverified{{Reason: ReasonBackendDependent, Feature: "auth-url"}},
	}

	var buf bytes.Buffer
	if err := want.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ReadSnapshot(&buf)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("snapshot changed in a round trip (-want +got):\n%s", diff)
	}
}

func TestReadSnapshotRefusesWhatItCannotInterpret(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"a newer schema", `{"schemaVersion": 99, "meta": {}, "probes": [], "observations": []}`},
		{"no schema version", `{"meta": {}, "probes": [], "observations": []}`},
		{"an unknown field", `{"schemaVersion": 1, "meta": {}, "probes": [], "observations": [], "surprise": true}`},
		{"not JSON", `schemaVersion: 1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadSnapshot(strings.NewReader(tt.json)); err == nil {
				t.Error("ReadSnapshot accepted the input")
			}
		})
	}
}

func TestHeaderValues(t *testing.T) {
	headers := []Header{{Name: "Set-Cookie", Value: "a=1"}, {Name: "Location", Value: "/x"}, {Name: "set-cookie", Value: "b=2"}}
	if diff := cmp.Diff([]string{"a=1", "b=2"}, HeaderValues(headers, "SET-COOKIE")); diff != "" {
		t.Errorf("HeaderValues (-want +got):\n%s", diff)
	}
	if got := HeaderValues(headers, "Missing"); got != nil {
		t.Errorf("HeaderValues for a missing header = %v, want nil", got)
	}
}
