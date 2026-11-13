// Package prober sends probes and records what came back.
//
// Requests are written to the socket by hand instead of through net/http's
// client. The client parses and cleans the URL before sending it, so it cannot
// send /a/../b or //a as written, and whether a proxy normalises such paths is
// one of the behaviours being measured.
package prober

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/asaleem9/routeparity/internal/echo"
	"github.com/asaleem9/routeparity/internal/model"
)

// Target is one routing setup to send probes to.
type Target struct {
	// HTTP is the host:port that accepts plain HTTP, or empty.
	HTTP string
	// HTTPS is the host:port that accepts TLS, or empty.
	HTTPS string
}

// Options controls how probes are sent.
type Options struct {
	// Timeout bounds one probe from dial to the end of the response body.
	Timeout time.Duration
	// Concurrency is how many probes are in flight at once.
	Concurrency int
	// RatePerSecond caps how many probes start per second. Zero means no cap.
	RatePerSecond float64
	// UserAgent identifies the tool to whoever reads the target's logs.
	UserAgent string
	// MaxBody is how much of a response body is read and hashed. The rest is
	// discarded and only counted.
	MaxBody int64
}

// Defaults for Options fields left at zero.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultConcurrency = 4
	DefaultRate        = 20
	DefaultMaxBody     = 1 << 20
	DefaultUserAgent   = "routeparity (+https://github.com/asaleem9/routeparity)"
)

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MaxBody <= 0 {
		o.MaxBody = DefaultMaxBody
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	return o
}

// Error classes recorded in ClientView.Error. They are deliberately coarse so
// that the same failure compares equal across targets and runs.
const (
	ErrClassRefused   = "connection-refused"
	ErrClassTimeout   = "timeout"
	ErrClassClosed    = "connection-closed"
	ErrClassTLS       = "tls-handshake"
	ErrClassDNS       = "dns"
	ErrClassMalformed = "malformed-response"
	ErrClassOther     = "error"
)

// Send sends one probe once and returns what was observed. A failure to get a
// response is not an error: it is recorded in the observation, because "this
// target refused the connection" is itself something to compare.
func Send(ctx context.Context, addr string, probe model.Probe, opts Options) model.Observation {
	opts = opts.withDefaults()
	obs := model.Observation{ProbeID: probe.ID, Attempt: 1}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		obs.Client.Error = classify(err)
		return obs
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	if probe.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: hostWithoutPort(probe.Host),
			NextProtos: []string{"http/1.1"},
			// The certificate is recorded and compared between targets, not
			// trusted: lab and staging endpoints routinely present
			// self-signed certificates, and refusing them would hide every
			// other behaviour behind a handshake error.
			InsecureSkipVerify: true, //nolint:gosec // see above
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			obs.Client.Error = classifyTLS(err)
			return obs
		}
		obs.Client.TLS = tlsView(tlsConn.ConnectionState())
		conn = tlsConn
	}

	if _, err := conn.Write(requestBytes(probe, opts.UserAgent)); err != nil {
		obs.Client.Error = classify(err)
		return obs
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: probe.Method})
	if err != nil {
		obs.Client.Error = classifyResponse(err)
		return obs
	}
	defer func() { _ = resp.Body.Close() }()

	obs.Client.Status = resp.StatusCode
	obs.Client.Proto = resp.Proto

	// Read up to MaxBody for hashing and for the echo's JSON; count the rest.
	head, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBody))
	rest, restErr := io.Copy(io.Discard, resp.Body)
	if err == nil {
		err = restErr
	}
	if err != nil {
		// The status and headers arrived, so they are kept; the body did not
		// arrive whole, so it is not hashed.
		obs.Client.Error = classifyResponse(err)
	} else {
		sum := sha256.Sum256(head)
		obs.Client.BodySHA256 = hex.EncodeToString(sum[:])
	}
	obs.Client.BodyLen = int64(len(head)) + rest

	if value := resp.Header.Get(echo.ViewHeader); value != "" {
		obs.Backend = backendView(value, head)
		resp.Header.Del(echo.ViewHeader)
	}
	obs.Client.Headers = flatten(resp.Header)
	return obs
}

// requestBytes renders the probe as an HTTP/1.1 request. The target and the
// header values go out exactly as the probe holds them; Probe.Validate has
// already refused anything that could split the request.
func requestBytes(p model.Probe, userAgent string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", p.Method, p.RawTarget)
	fmt.Fprintf(&b, "Host: %s\r\n", p.Host)

	reserved := map[string]bool{"host": true, "connection": true, "content-length": true}
	has := map[string]bool{}
	for _, h := range p.Headers {
		name := strings.ToLower(h.Name)
		if reserved[name] {
			continue // set by the prober; a second copy would be ambiguous
		}
		has[name] = true
		fmt.Fprintf(&b, "%s: %s\r\n", h.Name, h.Value)
	}
	if !has["user-agent"] {
		fmt.Fprintf(&b, "User-Agent: %s\r\n", userAgent)
	}
	if !has["accept"] {
		b.WriteString("Accept: */*\r\n")
	}
	if p.Body != nil {
		if !has["content-type"] {
			contentType := p.Body.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
		}
		fmt.Fprintf(&b, "Content-Length: %d\r\n", p.Body.Size)
	}
	// One request per connection: a response can then never be attributed to
	// the wrong probe.
	b.WriteString("Connection: close\r\n\r\n")
	if p.Body != nil {
		b.Write(bytes.Repeat([]byte{'a'}, int(p.Body.Size)))
	}
	return b.Bytes()
}

// backendView reads what the echo backend reported. The header carries the
// view without request headers; the body, when it is complete JSON, carries
// the view with them.
func backendView(header string, body []byte) *model.BackendView {
	var full model.BackendView
	if json.Unmarshal(body, &full) == nil && full.Method != "" {
		return &full
	}
	view, err := echo.DecodeViewHeader(header)
	if err != nil {
		return nil
	}
	return view
}

// flatten lists headers sorted by name, keeping the order of repeated values.
// net/http has already folded the names to canonical form.
func flatten(h http.Header) []model.Header {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []model.Header
	for _, name := range names {
		for _, value := range h[name] {
			out = append(out, model.Header{Name: name, Value: value})
		}
	}
	return out
}

func tlsView(state tls.ConnectionState) *model.TLSView {
	view := &model.TLSView{
		Version: tls.VersionName(state.Version),
		ALPN:    state.NegotiatedProtocol,
	}
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		view.PeerSubject = leaf.Subject.String()
		view.PeerSANs = append(view.PeerSANs, leaf.DNSNames...)
		for _, ip := range leaf.IPAddresses {
			view.PeerSANs = append(view.PeerSANs, ip.String())
		}
	}
	return view
}

func hostWithoutPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

// classify maps a dial or write error to an error class.
func classify(err error) string {
	var dnsErr *net.DNSError
	switch {
	case isTimeout(err):
		return ErrClassTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return ErrClassRefused
	case errors.As(err, &dnsErr):
		return ErrClassDNS
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ErrClassClosed
	}
	return ErrClassOther
}

func classifyTLS(err error) string {
	if isTimeout(err) {
		return ErrClassTimeout
	}
	return ErrClassTLS
}

// classifyResponse maps an error from reading the response. Anything that is
// not a timeout or a dropped connection means the bytes that arrived were not
// a well-formed HTTP response.
func classifyResponse(err error) string {
	switch class := classify(err); class {
	case ErrClassTimeout, ErrClassClosed:
		return class
	}
	return ErrClassMalformed
}
