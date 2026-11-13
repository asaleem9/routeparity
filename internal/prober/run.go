package prober

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/asaleem9/routeparity/internal/model"
)

// Result is the outcome of sending a set of probes to one target.
type Result struct {
	// Observations has one entry per attempt of each probe that was sent, in
	// the order of the probes given, whatever order they completed in.
	Observations []model.Observation
	// Unverified lists what could not be sent at all.
	Unverified []model.Unverified
}

// Run sends every probe to the target. It returns an error only when the run
// was cancelled; a probe that gets no response is recorded as an observation.
func Run(ctx context.Context, target Target, probes []model.Probe, opts Options) (Result, error) {
	opts = opts.withDefaults()

	type job struct {
		probe   model.Probe
		attempt int
		addr    string
		slot    int
	}
	var jobs []job
	skippedHTTPS, skippedHTTP := 0, 0
	for _, p := range probes {
		addr := target.HTTP
		if p.Scheme == "https" {
			addr = target.HTTPS
		}
		if addr == "" {
			if p.Scheme == "https" {
				skippedHTTPS++
			} else {
				skippedHTTP++
			}
			continue
		}
		for attempt := 1; attempt <= p.Repeat; attempt++ {
			jobs = append(jobs, job{probe: p, attempt: attempt, addr: addr, slot: len(jobs)})
		}
	}

	var res Result
	if skippedHTTPS > 0 {
		res.Unverified = append(res.Unverified, model.Unverified{
			Reason:  model.ReasonUnsupported,
			Feature: "https",
			Detail:  fmt.Sprintf("%d HTTPS probe(s) were not sent because the target has no HTTPS address", skippedHTTPS),
		})
	}
	if skippedHTTP > 0 {
		res.Unverified = append(res.Unverified, model.Unverified{
			Reason:  model.ReasonUnsupported,
			Feature: "http",
			Detail:  fmt.Sprintf("%d HTTP probe(s) were not sent because the target has no HTTP address", skippedHTTP),
		})
	}

	var limiter *rate.Limiter
	if opts.RatePerSecond > 0 {
		limiter = rate.NewLimiter(rate.Limit(opts.RatePerSecond), 1)
	}

	res.Observations = make([]model.Observation, len(jobs))
	var group errgroup.Group
	group.SetLimit(opts.Concurrency)
	for _, j := range jobs {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				break // cancelled; reported below
			}
		}
		if ctx.Err() != nil {
			break
		}
		group.Go(func() error {
			obs := Send(ctx, j.addr, j.probe, opts)
			obs.Attempt = j.attempt
			res.Observations[j.slot] = obs
			return nil
		})
	}
	_ = group.Wait() // the goroutines never return an error
	// A cancelled run has holes where probes were never sent, and sent probes
	// that failed only because of the cancellation. Neither may be mistaken
	// for a complete result.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}
