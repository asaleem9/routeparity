package echo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/model"
)

var identity = Identity{Namespace: "demo", Service: "web", Port: "8000"}

// response is a fully read HTTP response.
type response struct {
	status int
	header http.Header
	body   []byte
}

// rawRequest writes a request line byte for byte, which net/http's client
// cannot do because it cleans the URL first.
func rawRequest(t *testing.T, addr, requestLine string, headers ...string) response {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var req strings.Builder
	req.WriteString(requestLine + "\r\n")
	for _, h := range headers {
		req.WriteString(h + "\r\n")
	}
	req.WriteString("Connection: close\r\n\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response to %q: %v", requestLine, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of response to %q: %v", requestLine, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

func decodeBody(t *testing.T, resp response) model.BackendView {
	t.Helper()
	var view model.BackendView
	if err := json.Unmarshal(resp.body, &view); err != nil {
		t.Fatalf("response body is not a backend view: %v\n%s", err, resp.body)
	}
	return view
}

func TestHandlerReportsTheTargetExactlyAsReceived(t *testing.T) {
	srv := httptest.NewServer(Handler(identity, "web-abc"))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	// Every one of these would be cleaned or redirected by an http.ServeMux.
	targets := []string{
		"/",
		"/a/../b",
		"//a//b",
		"/a/./b/",
		"/%2e%2e/a",
		"/a%2Fb",
		"/A/b?x=1&y=%20z",
		"/health",
		"/status/500",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			resp := rawRequest(t, addr, "GET "+target+" HTTP/1.1", "Host: shop.example.com")
			if resp.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: the echo must answer every path itself", resp.status)
			}
			view := decodeBody(t, resp)
			if view.RawTarget != target {
				t.Errorf("RawTarget = %q, want %q unchanged", view.RawTarget, target)
			}
		})
	}
}

func TestHandlerReportsIdentityFromTheListenerNotTheRequest(t *testing.T) {
	srv := httptest.NewServer(Handler(identity, "web-abc"))
	defer srv.Close()

	// A proxy may rewrite the Host header; identity must not follow it.
	resp := rawRequest(t, srv.Listener.Addr().String(), "POST /orders HTTP/1.1",
		"Host: rewritten.internal", "X-Forwarded-For: 10.0.0.1", "X-Forwarded-For: 10.0.0.2",
		"Content-Length: 5", "", "hello")
	view := decodeBody(t, resp)

	want := model.BackendView{
		Namespace: "demo", Service: "web", Port: "8000", Pod: "web-abc",
		Method: "POST", RawTarget: "/orders", Host: "rewritten.internal", BodyLen: 5,
	}
	gotHeaders := view.Headers
	view.Headers = nil
	if diff := cmp.Diff(want, view); diff != "" {
		t.Errorf("view (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"10.0.0.1", "10.0.0.2"}, model.HeaderValues(gotHeaders, "x-forwarded-for")); diff != "" {
		t.Errorf("repeated request header (-want +got):\n%s", diff)
	}
}

func TestViewSurvivesAHeadRequest(t *testing.T) {
	srv := httptest.NewServer(Handler(identity, "web-abc"))
	defer srv.Close()

	resp := rawRequest(t, srv.Listener.Addr().String(), "HEAD /a/../b HTTP/1.1", "Host: shop.example.com")
	if len(resp.body) != 0 {
		t.Errorf("HEAD response has a %d-byte body", len(resp.body))
	}
	view, err := DecodeViewHeader(resp.header.Get(ViewHeader))
	if err != nil {
		t.Fatalf("decode %s header: %v", ViewHeader, err)
	}
	if view.Method != "HEAD" || view.RawTarget != "/a/../b" || view.Service != "web" {
		t.Errorf("view from header = %+v", view)
	}
	if len(view.Headers) != 0 {
		t.Error("the header copy of the view includes request headers; it should stay small")
	}
}

func TestDecodeViewHeaderRejectsGarbage(t *testing.T) {
	for _, value := range []string{"", "not base64!", "bm90IGpzb24"} {
		if _, err := DecodeViewHeader(value); err == nil {
			t.Errorf("DecodeViewHeader(%q) succeeded", value)
		}
	}
}

func TestEveryMethodIsAnswered(t *testing.T) {
	srv := httptest.NewServer(Handler(identity, ""))
	defer srv.Close()
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		resp := rawRequest(t, srv.Listener.Addr().String(), fmt.Sprintf("%s /x HTTP/1.1", method), "Host: h")
		if view := decodeBody(t, resp); view.Method != method {
			t.Errorf("%s: view reports method %q", method, view.Method)
		}
	}
}
