// Package echo is the backend that stands in for real Services in a lab run.
// It answers every request with a description of what it received, so that a
// run can compare what each proxy sent to the backend and not only what the
// client got back.
package echo

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/asaleem9/routeparity/internal/model"
)

// ViewHeader carries the backend view, base64url-encoded JSON without the
// request headers, on every response. It is how the view survives a HEAD
// request, which has no body.
const ViewHeader = "Routeparity-Backend"

// Identity says which Service a listener stands in for. It comes from the
// port the request arrived on, not from the request, so it stays correct when
// a proxy rewrites the Host header or the path.
type Identity struct {
	Namespace string
	Service   string
	Port      string
}

// Handler answers every request, on any path and with any method, with the
// model.BackendView of that request as JSON.
//
// It must be served directly by an http.Server. Wrapping it in an
// http.ServeMux would defeat its purpose: the mux redirects requests whose
// path contains dot segments or repeated slashes, and whether a proxy forwards
// such paths unchanged is one of the things a lab run measures.
func Handler(id Identity, pod string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyLen, _ := io.Copy(io.Discard, r.Body)

		view := model.BackendView{
			Namespace: id.Namespace,
			Service:   id.Service,
			Port:      id.Port,
			Pod:       pod,
			Method:    r.Method,
			// RequestURI is the request-target exactly as it was received.
			RawTarget: r.RequestURI,
			Host:      r.Host,
			BodyLen:   bodyLen,
		}

		// Without the headers the view is small enough to travel in a header.
		compact, err := json.Marshal(view)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set(ViewHeader, base64.RawURLEncoding.EncodeToString(compact))

		view.Headers = flatten(r.Header)
		full, err := json.Marshal(view)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(append(full, '\n'))
		}
	})
}

// flatten lists request headers sorted by name. net/http has already
// canonicalised the names and lost the order they arrived in, so sorting
// loses nothing further and makes the output stable.
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

// DecodeViewHeader reads the backend view from a response's ViewHeader value.
func DecodeViewHeader(value string) (*model.BackendView, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	var view model.BackendView
	if err := json.Unmarshal(data, &view); err != nil {
		return nil, err
	}
	return &view, nil
}
