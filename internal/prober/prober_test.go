package prober

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/echo"
	"github.com/asaleem9/routeparity/internal/model"
)

func probe(t *testing.T, p model.Probe) model.Probe {
	t.Helper()
	if p.Host == "" {
		p.Host = "shop.example.com"
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return p
}

// wireServer accepts connections, records the request bytes exactly as they
// arrive, and answers each with a canned response. It is deliberately not an
// http.Server: the test is about what goes on the wire.
type wireServer struct {
	ln       net.Listener
	response string

	mu       sync.Mutex
	requests []string
}

func newWireServer(t *testing.T, response string) *wireServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &wireServer{ln: ln, response: response}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *wireServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	var req strings.Builder
	contentLength := 0
	for {
		line, err := r.ReadString('\n')
		req.WriteString(line)
		if err != nil {
			return
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			for _, c := range strings.TrimSpace(v) {
				contentLength = contentLength*10 + int(c-'0')
			}
		}
		if line == "\r\n" {
			break
		}
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return
	}
	req.Write(body)

	s.mu.Lock()
	s.requests = append(s.requests, req.String())
	s.mu.Unlock()
	_, _ = io.WriteString(conn, s.response)
}

func (s *wireServer) addr() string { return s.ln.Addr().String() }

func (s *wireServer) lastRequest(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("server received no request")
	}
	return s.requests[len(s.requests)-1]
}

const okResponse = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: text/plain\r\n\r\nok"

func TestSendWritesTheTargetByteForByte(t *testing.T) {
	srv := newWireServer(t, okResponse)
	// net/http's client would rewrite or refuse every one of these.
	targets := []string{
		"/a/../b",
		"//double//slash",
		"/./current",
		"/%2e%2e/encoded-dots",
		"/a%2Fb",
		"/UPPER/lower?Q=1&x=%20",
		"/trailing/",
		"*",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			obs := Send(context.Background(), srv.addr(), probe(t, model.Probe{RawTarget: target}), Options{})
			if obs.Client.Error != "" || obs.Client.Status != 200 {
				t.Fatalf("observation = %+v", obs.Client)
			}
			wantLine := "GET " + target + " HTTP/1.1\r\n"
			if got := srv.lastRequest(t); !strings.HasPrefix(got, wantLine) {
				t.Errorf("request line = %q, want %q", strings.SplitN(got, "\r\n", 2)[0], strings.TrimSpace(wantLine))
			}
		})
	}
}

func TestSendWritesAWellFormedRequest(t *testing.T) {
	srv := newWireServer(t, okResponse)
	p := probe(t, model.Probe{
		Method:    "POST",
		RawTarget: "/upload",
		Headers: []model.Header{
			{Name: "X-Canary", Value: "always"},
			{Name: "Host", Value: "evil.example.com"}, // must not produce a second Host header
			{Name: "Content-Length", Value: "999"},    // must not contradict the real length
		},
		Body: &model.BodySpec{Size: 5},
	})
	obs := Send(context.Background(), srv.addr(), p, Options{UserAgent: "test-agent"})
	if obs.Client.Error != "" {
		t.Fatalf("error = %q", obs.Client.Error)
	}

	want := "POST /upload HTTP/1.1\r\n" +
		"Host: shop.example.com\r\n" +
		"X-Canary: always\r\n" +
		"User-Agent: test-agent\r\n" +
		"Accept: */*\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"aaaaa"
	if diff := cmp.Diff(want, srv.lastRequest(t)); diff != "" {
		t.Errorf("request on the wire (-want +got):\n%s", diff)
	}
}

func TestSendRecordsTheResponse(t *testing.T) {
	srv := newWireServer(t, "HTTP/1.1 301 Moved Permanently\r\n"+
		"Location: http://shop.example.com/app/\r\n"+
		"Set-Cookie: a=1\r\n"+
		"Set-Cookie: b=2\r\n"+
		"Content-Length: 5\r\n\r\nhello")
	obs := Send(context.Background(), srv.addr(), probe(t, model.Probe{RawTarget: "/app"}), Options{})

	want := model.ClientView{
		Status:  301,
		Proto:   "HTTP/1.1",
		BodyLen: 5,
		// sha256("hello")
		BodySHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		Headers: []model.Header{
			{Name: "Content-Length", Value: "5"},
			{Name: "Location", Value: "http://shop.example.com/app/"},
			{Name: "Set-Cookie", Value: "a=1"},
			{Name: "Set-Cookie", Value: "b=2"},
		},
	}
	if diff := cmp.Diff(want, obs.Client); diff != "" {
		t.Errorf("client view (-want +got):\n%s", diff)
	}
	if obs.Backend != nil {
		t.Errorf("backend view = %+v for a response that carries none", obs.Backend)
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	obs := Send(context.Background(), srv.Listener.Addr().String(), probe(t, model.Probe{RawTarget: "/start"}), Options{})
	if obs.Client.Status != http.StatusFound || hits != 1 {
		t.Errorf("status = %d after %d request(s); want the 302 itself from a single request", obs.Client.Status, hits)
	}
}

func TestSendHashesOnlyUpToMaxBodyButCountsEverything(t *testing.T) {
	body := strings.Repeat("x", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer srv.Close()
	obs := Send(context.Background(), srv.Listener.Addr().String(), probe(t, model.Probe{RawTarget: "/big"}), Options{MaxBody: 10})
	if obs.Client.BodyLen != 1000 {
		t.Errorf("BodyLen = %d, want 1000", obs.Client.BodyLen)
	}
	firstTen := sha256.Sum256([]byte(body[:10]))
	if want := hex.EncodeToString(firstTen[:]); obs.Client.BodySHA256 != want {
		t.Errorf("BodySHA256 = %s, want the hash of the first 10 bytes, %s", obs.Client.BodySHA256, want)
	}
	full := Send(context.Background(), srv.Listener.Addr().String(), probe(t, model.Probe{RawTarget: "/big"}), Options{})
	if full.Client.BodySHA256 == obs.Client.BodySHA256 {
		t.Error("a truncated hash equals the full hash")
	}
}

func TestSendReadsTheBackendViewFromTheEcho(t *testing.T) {
	srv := httptest.NewServer(echo.Handler(echo.Identity{Namespace: "demo", Service: "web", Port: "80"}, "web-1"))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	t.Run("GET carries the full view", func(t *testing.T) {
		obs := Send(context.Background(), addr, probe(t, model.Probe{
			RawTarget: "/a/../b?x=1",
			Headers:   []model.Header{{Name: "X-Trace", Value: "t1"}},
		}), Options{})
		if obs.Backend == nil {
			t.Fatal("no backend view")
		}
		if obs.Backend.Service != "web" || obs.Backend.RawTarget != "/a/../b?x=1" || obs.Backend.Host != "shop.example.com" {
			t.Errorf("backend view = %+v", obs.Backend)
		}
		if got := model.HeaderValues(obs.Backend.Headers, "X-Trace"); len(got) != 1 || got[0] != "t1" {
			t.Errorf("backend saw X-Trace = %v, want [t1]", got)
		}
		if len(model.HeaderValues(obs.Client.Headers, echo.ViewHeader)) != 0 {
			t.Error("the echo's own header is listed among the client's response headers; it would show up as a difference between targets")
		}
	})
	t.Run("HEAD falls back to the header", func(t *testing.T) {
		obs := Send(context.Background(), addr, probe(t, model.Probe{Method: "HEAD", RawTarget: "/x"}), Options{})
		if obs.Backend == nil || obs.Backend.Method != "HEAD" || obs.Backend.RawTarget != "/x" {
			t.Errorf("backend view = %+v", obs.Backend)
		}
		if obs.Client.BodyLen != 0 {
			t.Errorf("BodyLen = %d for a HEAD response", obs.Client.BodyLen)
		}
	})
}

func TestSendOverTLS(t *testing.T) {
	var sni string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secure") }))
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = hello.ServerName
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	obs := Send(context.Background(), srv.Listener.Addr().String(),
		probe(t, model.Probe{Scheme: "https", Host: "secure.example.com", RawTarget: "/"}), Options{})
	if obs.Client.Error != "" || obs.Client.Status != 200 {
		t.Fatalf("observation = %+v; a self-signed certificate must not stop the probe", obs.Client)
	}
	if sni != "secure.example.com" {
		t.Errorf("SNI = %q, want the probe's host: which certificate and route a proxy picks depends on it", sni)
	}
	if obs.Client.TLS == nil || !strings.HasPrefix(obs.Client.TLS.Version, "TLS 1.") || obs.Client.TLS.PeerSubject == "" {
		t.Errorf("TLS view = %+v, want the version and the certificate subject recorded", obs.Client.TLS)
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	closed := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}()

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }() // hold the connection open and say nothing
		}
	}()

	// A real HTTP server answers a TLS ClientHello with a plain-text 400,
	// which is what a proxy's HTTP port does too.
	plainHTTP := httptest.NewServer(http.NotFoundHandler())
	defer plainHTTP.Close()

	tests := []struct {
		name string
		addr string
		p    model.Probe
		want string
	}{
		{"nothing listening", closed, model.Probe{RawTarget: "/"}, ErrClassRefused},
		{"no answer", silent.Addr().String(), model.Probe{RawTarget: "/"}, ErrClassTimeout},
		{"not HTTP", newWireServer(t, "SSH-2.0-OpenSSH\r\n").addr(), model.Probe{RawTarget: "/"}, ErrClassMalformed},
		{"closed without answering", newWireServer(t, "").addr(), model.Probe{RawTarget: "/"}, ErrClassClosed},
		{"plain HTTP on the TLS port", plainHTTP.Listener.Addr().String(), model.Probe{Scheme: "https", RawTarget: "/"}, ErrClassTLS},
		{"TLS port that never answers", silent.Addr().String(), model.Probe{Scheme: "https", RawTarget: "/"}, ErrClassTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := Send(context.Background(), tt.addr, probe(t, tt.p), Options{Timeout: 300 * time.Millisecond})
			if obs.Client.Error != tt.want {
				t.Errorf("error class = %q, want %q", obs.Client.Error, tt.want)
			}
			if obs.Client.Status != 0 {
				t.Errorf("status = %d alongside an error before any response", obs.Client.Status)
			}
		})
	}
}

func TestRunKeepsProbeOrderAndRepeats(t *testing.T) {
	srv := httptest.NewServer(echo.Handler(echo.Identity{Namespace: "demo", Service: "web", Port: "80"}, ""))
	defer srv.Close()

	var probes []model.Probe
	for _, target := range []string{"/c", "/a", "/b"} {
		probes = append(probes, probe(t, model.Probe{RawTarget: target}))
	}
	probes[1].Repeat = 3

	res, err := Run(context.Background(), Target{HTTP: srv.Listener.Addr().String()}, probes, Options{Concurrency: 8, RatePerSecond: 0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	type row struct {
		Target  string
		Attempt int
	}
	var got []row
	for _, obs := range res.Observations {
		if obs.Backend == nil {
			t.Fatalf("observation without a backend view: %+v", obs)
		}
		got = append(got, row{obs.Backend.RawTarget, obs.Attempt})
	}
	want := []row{{"/c", 1}, {"/a", 1}, {"/a", 2}, {"/a", 3}, {"/b", 1}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("observations (-want +got):\n%s", diff)
	}
	if len(res.Unverified) != 0 {
		t.Errorf("unexpected unverified entries: %+v", res.Unverified)
	}
}

func TestRunReportsProbesItCouldNotSend(t *testing.T) {
	srv := newWireServer(t, okResponse)
	probes := []model.Probe{
		probe(t, model.Probe{RawTarget: "/plain"}),
		probe(t, model.Probe{Scheme: "https", RawTarget: "/secure"}),
		probe(t, model.Probe{Scheme: "https", RawTarget: "/secure2"}),
	}
	res, err := Run(context.Background(), Target{HTTP: srv.addr()}, probes, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Observations) != 1 || res.Observations[0].ProbeID != probes[0].ID {
		t.Errorf("observations = %+v, want only the HTTP probe", res.Observations)
	}
	if len(res.Unverified) != 1 || res.Unverified[0].Feature != "https" || !strings.Contains(res.Unverified[0].Detail, "2 HTTPS probe") {
		t.Errorf("unverified = %+v, want one entry saying two HTTPS probes were not sent", res.Unverified)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	srv := newWireServer(t, okResponse)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Target{HTTP: srv.addr()}, []model.Probe{probe(t, model.Probe{RawTarget: "/"})}, Options{})
	if err == nil {
		t.Error("Run returned no error for a cancelled context; a partial result must not look complete")
	}
}
