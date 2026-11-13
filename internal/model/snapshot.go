package model

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SnapshotSchemaVersion is bumped whenever the snapshot file format changes in
// a way older readers cannot handle.
const SnapshotSchemaVersion = 1

// TargetKind names the kind of routing setup a snapshot was recorded from.
type TargetKind string

// The kinds of routing setup a snapshot can come from.
const (
	TargetIngressNGINX TargetKind = "ingress-nginx"
	TargetGatewayAPI   TargetKind = "gateway-api"
	TargetUnknown      TargetKind = "unknown"
)

// RunMode says how observations were collected, which determines what a MATCH
// is allowed to claim.
type RunMode string

const (
	// ModeLive observations come from a reachable endpoint and contain only
	// the client's view.
	ModeLive RunMode = "live"
	// ModeLab observations come from a local cluster with echo backends and
	// also contain the backend's view.
	ModeLab RunMode = "lab"
)

// TargetMeta describes where a snapshot came from. Every version that could
// change behaviour is recorded, because a result is only meaningful for the
// versions it was measured on.
type TargetMeta struct {
	Name           string            `json:"name"`
	Kind           TargetKind        `json:"kind"`
	Mode           RunMode           `json:"mode"`
	Address        string            `json:"address,omitempty"`
	Implementation string            `json:"implementation,omitempty"`
	Versions       map[string]string `json:"versions,omitempty"`
	ToolVersion    string            `json:"toolVersion"`
	RecordedAt     time.Time         `json:"recordedAt"`
}

// UnverifiedReason is a stable code for why something could not be checked.
type UnverifiedReason string

const (
	// ReasonBackendDependent means the behaviour depends on the real backend,
	// which a lab run replaces with an echo server.
	ReasonBackendDependent UnverifiedReason = "backend-dependent"
	// ReasonClientIdentity means the behaviour depends on the client address,
	// which is not meaningful through a port-forward.
	ReasonClientIdentity UnverifiedReason = "client-identity"
	// ReasonNotConverted means the converter recognised the feature and did
	// not translate it.
	ReasonNotConverted UnverifiedReason = "not-converted"
	// ReasonNoProbe means no probe could be generated that exercises the rule.
	ReasonNoProbe UnverifiedReason = "no-probe"
	// ReasonStatistical means the behaviour is random per request and can only
	// be characterised over many requests.
	ReasonStatistical UnverifiedReason = "statistical"
	// ReasonUnsupported means the tool does not handle the feature yet.
	ReasonUnsupported UnverifiedReason = "unsupported"
)

// Unverified records something a run could not check. A run with any
// Unverified entries must not be reported as equivalent.
type Unverified struct {
	Reason  UnverifiedReason `json:"reason"`
	Feature string           `json:"feature"`
	Object  *ObjectRef       `json:"object,omitempty"`
	Detail  string           `json:"detail,omitempty"`
}

// Snapshot is the recorded behaviour of one target for one set of probes.
// Every comparison the tool makes is between two snapshots.
type Snapshot struct {
	SchemaVersion int           `json:"schemaVersion"`
	Meta          TargetMeta    `json:"meta"`
	Probes        []Probe       `json:"probes"`
	Observations  []Observation `json:"observations"`
	Unverified    []Unverified  `json:"unverified,omitempty"`
}

// Write encodes the snapshot as indented JSON.
func (s *Snapshot) Write(w io.Writer) error {
	s.SchemaVersion = SnapshotSchemaVersion
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(s)
}

// ReadSnapshot decodes a snapshot and refuses files written by a newer schema.
func ReadSnapshot(r io.Reader) (*Snapshot, error) {
	var s Snapshot
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if s.SchemaVersion < 1 || s.SchemaVersion > SnapshotSchemaVersion {
		return nil, fmt.Errorf("snapshot schema version %d is not supported (this build reads up to %d)",
			s.SchemaVersion, SnapshotSchemaVersion)
	}
	return &s, nil
}
