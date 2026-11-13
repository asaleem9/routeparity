package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/prober"
	"github.com/asaleem9/routeparity/internal/report"
)

func newProbesCommand(streams Streams) *cobra.Command {
	var (
		world  worldFlags
		probes probeFlags
		output string
	)
	cmd := &cobra.Command{
		Use:   "probes -f FILE...",
		Short: "List the requests that would be sent, without sending them",
		Example: "  routeparity probes -f ingress.yaml\n" +
			"  routeparity probes -f manifests/ --mutations -o json",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if output != "text" && output != "json" {
				return fmt.Errorf("unknown output format %q: want text or json", output)
			}
			w, err := world.load(streams)
			if err != nil {
				return err
			}
			set, err := probes.generate(w)
			if err != nil {
				return err
			}
			if output == "json" {
				enc := json.NewEncoder(streams.Out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				return enc.Encode(set)
			}
			return report.ProbesText(streams.Out, set.Probes, set.Unverified)
		},
	}
	world.add(cmd)
	probes.add(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

func newRecordCommand(streams Streams) *cobra.Command {
	var (
		world        worldFlags
		probes       probeFlags
		send         sendFlags
		address      string
		httpsAddress string
		name         string
		kind         string
		output       string
	)
	cmd := &cobra.Command{
		Use:   "record -f FILE... --address HOST:PORT",
		Short: "Send the probes to one endpoint and save what it did",
		Long: "record sends the probes generated from your manifests to one endpoint and writes a\n" +
			"snapshot of its behaviour. Compare two snapshots with `routeparity diff`.\n\n" +
			"A snapshot of the old setup stays useful after the old setup is gone.",
		Example: "  routeparity record -f manifests/ --address 203.0.113.10:80 --name old -o old.json\n" +
			"  routeparity record -f manifests/ --address 203.0.113.20:80 --name new -o new.json\n" +
			"  routeparity diff old.json new.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targetKind, err := parseKind(kind)
			if err != nil {
				return err
			}
			opts, err := send.options()
			if err != nil {
				return err
			}
			tgt, err := target("address", address, "https-address", httpsAddress)
			if err != nil {
				return err
			}
			w, err := world.load(streams)
			if err != nil {
				return err
			}
			set, err := probes.generate(w)
			if err != nil {
				return err
			}
			meta := model.TargetMeta{Name: name, Kind: targetKind, Address: describeAddresses(tgt), ToolVersion: Version()}
			snap, err := prober.Record(cmd.Context(), tgt, meta, set.Probes, set.Unverified, opts, time.Now())
			if err != nil {
				return err
			}
			if err := requireResponses(name, snap); err != nil {
				return err
			}
			if output == "" || output == "-" {
				if err := snap.Write(streams.Out); err != nil {
					return err
				}
			} else if err := writeFile(output, snap.Write); err != nil {
				return err
			}
			fmt.Fprintf(streams.Err, "recorded %d observation(s) from %s\n", len(snap.Observations), meta.Address)
			return nil
		},
	}
	world.add(cmd)
	probes.add(cmd)
	send.add(cmd)
	cmd.Flags().StringVar(&address, "address", "", "HOST:PORT that accepts plain HTTP")
	cmd.Flags().StringVar(&httpsAddress, "https-address", "", "HOST:PORT that accepts TLS")
	cmd.Flags().StringVar(&name, "name", "target", "name for this endpoint in reports")
	cmd.Flags().StringVar(&kind, "kind", string(model.TargetUnknown), "what the endpoint is: ingress-nginx, gateway-api or unknown")
	cmd.Flags().StringVarP(&output, "output", "o", "-", "file to write the snapshot to; \"-\" writes to stdout")
	return cmd
}

func parseKind(s string) (model.TargetKind, error) {
	switch kind := model.TargetKind(s); kind {
	case model.TargetIngressNGINX, model.TargetGatewayAPI, model.TargetUnknown:
		return kind, nil
	}
	return "", fmt.Errorf("unknown --kind %q: want ingress-nginx, gateway-api or unknown", s)
}

// compareFlags control how a comparison is judged and printed.
type compareFlags struct {
	compareBody    bool
	ignoreHeaders  []string
	compareHeaders []string
	strict         bool
	output         string
	junitFile      string
	markdownFile   string
}

func (f *compareFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.compareBody, "compare-body", false,
		"treat differing response bodies as differences; leave off when backends return per-request content")
	cmd.Flags().StringSliceVar(&f.ignoreHeaders, "ignore-header", nil,
		"response header whose differences are never counted; they are still listed as suppressed (repeatable)")
	cmd.Flags().StringSliceVar(&f.compareHeaders, "compare-header", nil,
		"extra response header whose differences count (repeatable)")
	cmd.Flags().BoolVar(&f.strict, "strict", false,
		"exit with status 2 when nothing differs but something could not be verified")
	cmd.Flags().StringVarP(&f.output, "output", "o", "text", "output format: text, json, junit or markdown")
	cmd.Flags().StringVar(&f.junitFile, "junit", "", "also write a JUnit XML report to this file")
	cmd.Flags().StringVar(&f.markdownFile, "markdown", "", "also write a Markdown report to this file, for example $GITHUB_STEP_SUMMARY")
}

func (f *compareFlags) validate() error {
	switch f.output {
	case "text", "json", "junit", "markdown":
	default:
		return fmt.Errorf("unknown output format %q: want text, json, junit or markdown", f.output)
	}
	ignored := map[string]bool{}
	for _, name := range f.ignoreHeaders {
		ignored[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range f.compareHeaders {
		if ignored[strings.ToLower(strings.TrimSpace(name))] {
			return fmt.Errorf("header %q is given to both --ignore-header and --compare-header", name)
		}
	}
	return nil
}

// writeFile creates path and fills it with write, reporting a failed close,
// since a report that was not fully written is worse than none.
func writeFile(path string, write func(io.Writer) error) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// finish prints the comparison, writes any extra reports, and turns the
// result into an exit status.
func (f *compareFlags) finish(streams Streams, base, cand *model.Snapshot) error {
	res := diff.Compare(base, cand, diff.Options{
		CompareBody:    f.compareBody,
		IgnoreHeaders:  f.ignoreHeaders,
		CompareHeaders: f.compareHeaders,
	})
	junit := func(w io.Writer) error { return report.DiffJUnit(w, res, f.strict) }
	markdown := func(w io.Writer) error { return report.DiffMarkdown(w, res) }

	var err error
	switch f.output {
	case "json":
		err = report.DiffJSON(streams.Out, res)
	case "junit":
		err = junit(streams.Out)
	case "markdown":
		err = markdown(streams.Out)
	default:
		err = report.DiffText(streams.Out, res, report.StyleFor(streams.Out))
	}
	if err != nil {
		return err
	}
	if f.junitFile != "" {
		if err := writeFile(f.junitFile, junit); err != nil {
			return err
		}
	}
	if f.markdownFile != "" {
		if err := writeFile(f.markdownFile, markdown); err != nil {
			return err
		}
	}
	switch {
	case res.Summary.Diff > 0:
		return &exitError{code: ExitFindings}
	case f.strict && res.Incomplete():
		return &exitError{code: ExitUnverified}
	}
	return nil
}

func newDiffCommand(streams Streams) *cobra.Command {
	var compare compareFlags
	cmd := &cobra.Command{
		Use:   "diff BASELINE CANDIDATE",
		Short: "Compare two snapshots",
		Long: "diff compares the candidate snapshot against the baseline, probe by probe.\n\n" +
			"Exit status: 0 when nothing differs, 1 when something differs, 2 with --strict when\n" +
			"nothing differs but something could not be verified, 3 when the comparison could not run.",
		Example: "  routeparity diff old.json new.json\n" +
			"  routeparity diff old.json new.json --strict -o json",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := compare.validate(); err != nil {
				return err
			}
			base, err := readSnapshot(args[0], streams.In)
			if err != nil {
				return err
			}
			cand, err := readSnapshot(args[1], streams.In)
			if err != nil {
				return err
			}
			return compare.finish(streams, base, cand)
		},
	}
	compare.add(cmd)
	return cmd
}

func readSnapshot(path string, stdin io.Reader) (*model.Snapshot, error) {
	if path == "-" {
		snap, err := model.ReadSnapshot(stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		return snap, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	snap, err := model.ReadSnapshot(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return snap, nil
}

func newLiveCommand(streams Streams) *cobra.Command {
	var (
		world          worldFlags
		probes         probeFlags
		send           sendFlags
		compare        compareFlags
		baseline       string
		baselineHTTPS  string
		candidate      string
		candidateHTTPS string
	)
	cmd := &cobra.Command{
		Use:   "live -f FILE... --baseline HOST:PORT --candidate HOST:PORT",
		Short: "Send the same requests to two endpoints and compare the responses",
		Long: "live sends the probes generated from your manifests to two endpoints, the setup you\n" +
			"have and the one replacing it, and compares what each returned.\n\n" +
			"Without --mutations it only requests paths that appear in your manifests, with safe\n" +
			"methods, at a limited rate. It compares what the client receives; what each proxy\n" +
			"sent to the backend is not visible from outside.\n\n" +
			"Exit status is the same as for `routeparity diff`.",
		Example: "  routeparity live -f manifests/ --baseline 203.0.113.10:80 --candidate 203.0.113.20:80\n" +
			"  routeparity live -f manifests/ --baseline old.example.net:80 --candidate new.example.net:80 --mutations --strict",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := compare.validate(); err != nil {
				return err
			}
			opts, err := send.options()
			if err != nil {
				return err
			}
			baseTarget, err := target("baseline", baseline, "baseline-https", baselineHTTPS)
			if err != nil {
				return err
			}
			candTarget, err := target("candidate", candidate, "candidate-https", candidateHTTPS)
			if err != nil {
				return err
			}
			w, err := world.load(streams)
			if err != nil {
				return err
			}
			set, err := probes.generate(w)
			if err != nil {
				return err
			}

			record := func(name string, kind model.TargetKind, tgt prober.Target) (*model.Snapshot, error) {
				meta := model.TargetMeta{Name: name, Kind: kind, Address: describeAddresses(tgt), ToolVersion: Version()}
				snap, err := prober.Record(cmd.Context(), tgt, meta, set.Probes, set.Unverified, opts, time.Now())
				if err != nil {
					return nil, err
				}
				if err := requireResponses(name, snap); err != nil {
					return nil, err
				}
				return snap, nil
			}
			base, err := record("baseline", model.TargetIngressNGINX, baseTarget)
			if err != nil {
				return err
			}
			cand, err := record("candidate", model.TargetGatewayAPI, candTarget)
			if err != nil {
				return err
			}
			return compare.finish(streams, base, cand)
		},
	}
	world.add(cmd)
	probes.add(cmd)
	send.add(cmd)
	compare.add(cmd)
	cmd.Flags().StringVar(&baseline, "baseline", "", "HOST:PORT of the existing setup, plain HTTP")
	cmd.Flags().StringVar(&baselineHTTPS, "baseline-https", "", "HOST:PORT of the existing setup, TLS")
	cmd.Flags().StringVar(&candidate, "candidate", "", "HOST:PORT of the new setup, plain HTTP")
	cmd.Flags().StringVar(&candidateHTTPS, "candidate-https", "", "HOST:PORT of the new setup, TLS")
	return cmd
}

func newVerifyCommand(streams Streams) *cobra.Command {
	var (
		send         sendFlags
		compare      compareFlags
		address      string
		httpsAddress string
		name         string
		kind         string
		samples      int
		save         string
	)
	cmd := &cobra.Command{
		Use:   "verify BASELINE --address HOST:PORT",
		Short: "Check an endpoint against a saved snapshot",
		Long: "verify sends the requests stored in a snapshot to an endpoint and compares the answers\n" +
			"with the ones the snapshot recorded.\n\n" +
			"The snapshot carries its own requests, so no manifests are needed and the baseline does\n" +
			"not have to exist any more. Use it after a migration, after a controller upgrade, or in\n" +
			"CI to catch a change in routing behaviour.\n\n" +
			"Exit status is the same as for `routeparity diff`.",
		Example: "  routeparity verify old.json --address 203.0.113.20:80\n" +
			"  routeparity verify old.json --address gateway.staging:80 --samples 3 --strict --junit report.xml",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := compare.validate(); err != nil {
				return err
			}
			if samples < 1 {
				return errors.New("--samples must be at least 1")
			}
			targetKind, err := parseKind(kind)
			if err != nil {
				return err
			}
			opts, err := send.options()
			if err != nil {
				return err
			}
			tgt, err := target("address", address, "https-address", httpsAddress)
			if err != nil {
				return err
			}
			base, err := readSnapshot(args[0], streams.In)
			if err != nil {
				return err
			}
			if len(base.Probes) == 0 {
				return fmt.Errorf("%s contains no probes", args[0])
			}

			// The candidate is asked exactly what the baseline was asked. The
			// probes are copied so that raising the sample count does not
			// alter the baseline.
			probes := append([]model.Probe(nil), base.Probes...)
			for i := range probes {
				if probes[i].Repeat < samples {
					probes[i].Repeat = samples
				}
			}
			meta := model.TargetMeta{Name: name, Kind: targetKind, Address: describeAddresses(tgt), ToolVersion: Version()}
			cand, err := prober.Record(cmd.Context(), tgt, meta, probes, nil, opts, time.Now())
			if err != nil {
				return err
			}
			if err := requireResponses(name, cand); err != nil {
				return err
			}
			if save != "" {
				if err := writeFile(save, cand.Write); err != nil {
					return err
				}
			}
			return compare.finish(streams, base, cand)
		},
	}
	send.add(cmd)
	compare.add(cmd)
	addSamplesFlag(cmd, &samples)
	cmd.Flags().StringVar(&address, "address", "", "HOST:PORT that accepts plain HTTP")
	cmd.Flags().StringVar(&httpsAddress, "https-address", "", "HOST:PORT that accepts TLS")
	cmd.Flags().StringVar(&name, "name", "candidate", "name for this endpoint in reports")
	cmd.Flags().StringVar(&kind, "kind", string(model.TargetUnknown), "what the endpoint is: ingress-nginx, gateway-api or unknown")
	cmd.Flags().StringVar(&save, "save", "", "also write the endpoint's snapshot to this file")
	return cmd
}
