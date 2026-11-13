package prober

import (
	"context"
	"time"

	"github.com/asaleem9/routeparity/internal/model"
)

// Record sends the probes to a target and returns the snapshot. The
// unverified list is carried into the snapshot alongside anything the run
// itself could not send. now is the recording time, passed in so that callers
// and tests control it.
func Record(ctx context.Context, target Target, meta model.TargetMeta, probes []model.Probe,
	unverified []model.Unverified, opts Options, now time.Time) (*model.Snapshot, error) {
	res, err := Run(ctx, target, probes, opts)
	if err != nil {
		return nil, err
	}
	meta.RecordedAt = now.UTC()
	if meta.Mode == "" {
		meta.Mode = ModeOf(res.Observations)
	}
	snap := &model.Snapshot{
		SchemaVersion: model.SnapshotSchemaVersion,
		Meta:          meta,
		Probes:        probes,
		Observations:  res.Observations,
	}
	snap.Unverified = append(snap.Unverified, unverified...)
	snap.Unverified = append(snap.Unverified, res.Unverified...)
	return snap, nil
}

// ModeOf reports how observations were collected. They count as lab
// observations when a backend reported its view on at least one of them,
// which only the echo backend does.
func ModeOf(observations []model.Observation) model.RunMode {
	for _, obs := range observations {
		if obs.Backend != nil {
			return model.ModeLab
		}
	}
	return model.ModeLive
}
