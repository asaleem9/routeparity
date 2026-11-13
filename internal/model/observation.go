package model

import "strings"

// ClientView is what the sender of a probe saw.
type ClientView struct {
	// Status is the HTTP status code, or zero when no response arrived.
	Status int    `json:"status"`
	Proto  string `json:"proto,omitempty"`
	// Headers are the response headers sorted by name. Repeated headers
	// keep the order they arrived in.
	Headers    []Header `json:"headers,omitempty"`
	BodyLen    int64    `json:"bodyLen"`
	BodySHA256 string   `json:"bodySHA256,omitempty"`
	TLS        *TLSView `json:"tls,omitempty"`
	// Error is set when the exchange failed before a complete response was
	// read, for example "connection refused". It is a short class, not a raw
	// error string, so that it compares equal across targets.
	Error string `json:"error,omitempty"`
}

// TLSView is the part of a TLS handshake that a migration can change.
type TLSView struct {
	Version     string   `json:"version"`
	ALPN        string   `json:"alpn,omitempty"`
	PeerSubject string   `json:"peerSubject,omitempty"`
	PeerSANs    []string `json:"peerSANs,omitempty"`
}

// BackendView is what the backend received. It is only available when the
// backend is an echo server that reports it, which is the case in lab runs.
type BackendView struct {
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Port      string `json:"port,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Method    string `json:"method"`
	// RawTarget is the request-target the backend received, after any rewrite.
	RawTarget string   `json:"rawTarget"`
	Host      string   `json:"host"`
	Headers   []Header `json:"headers,omitempty"`
	BodyLen   int64    `json:"bodyLen"`
}

// Observation is the outcome of sending one probe once to one target.
type Observation struct {
	ProbeID string `json:"probeID"`
	// Attempt counts from one and distinguishes repeats of the same probe.
	Attempt int          `json:"attempt"`
	Client  ClientView   `json:"client"`
	Backend *BackendView `json:"backend,omitempty"`
}

// HeaderValues returns every value of the named header, matching the name
// case-insensitively.
func HeaderValues(headers []Header, name string) []string {
	var out []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}
