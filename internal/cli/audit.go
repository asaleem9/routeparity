package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/audit"
	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/report"
)

type auditFlags struct {
	world     worldFlags
	emitter   string
	noConvert bool
	output    string
	failOn    string
}

func newAuditCommand(streams Streams) *cobra.Command {
	var flags auditFlags
	cmd := &cobra.Command{
		Use:   "audit -f FILE...",
		Short: "Predict behaviour differences from manifests, without sending traffic",
		Long: "audit reads Ingress manifests and reports ingress-nginx behaviour that a Gateway API\n" +
			"setup will not reproduce unless it is configured explicitly.\n\n" +
			"It sends no requests. Every finding is a prediction, and behaviour that depends on\n" +
			"controller settings or on the Gateway implementation is not covered.",
		Example: "  routeparity audit -f ingress.yaml\n" +
			"  kubectl get ingress,service -A -o yaml | routeparity audit -f -\n" +
			"  routeparity audit -f manifests/ -o json --fail-on high",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAudit(cmd, streams, flags)
		},
	}
	flags.world.add(cmd)
	cmd.Flags().StringVar(&flags.emitter, "emitter", "standard",
		"ingress2gateway emitter whose warnings to include (standard, envoy-gateway)")
	cmd.Flags().BoolVar(&flags.noConvert, "no-convert", false,
		"do not run ingress2gateway to collect its warnings")
	cmd.Flags().StringVarP(&flags.output, "output", "o", "text", "output format: text or json")
	cmd.Flags().StringVar(&flags.failOn, "fail-on", "medium",
		"exit with status 1 when a finding has at least this severity: high, medium, low, info or none")
	return cmd
}

func runAudit(cmd *cobra.Command, streams Streams, flags auditFlags) error {
	threshold, err := parseFailOn(flags.failOn)
	if err != nil {
		return err
	}
	if flags.output != "text" && flags.output != "json" {
		return fmt.Errorf("unknown output format %q: want text or json", flags.output)
	}

	world, err := flags.world.load(streams)
	if err != nil {
		return err
	}

	opts := audit.Options{Emitter: flags.emitter}
	if !flags.noConvert {
		opts.Converter = convert.InProcess{}
	}
	rep, err := audit.Run(cmd.Context(), world, opts)
	if err != nil {
		return err
	}

	switch flags.output {
	case "json":
		err = report.AuditJSON(streams.Out, rep)
	default:
		err = report.AuditText(streams.Out, rep, report.StyleFor(streams.Out))
	}
	if err != nil {
		return err
	}

	if worst, ok := rep.Worst(); ok && threshold >= 0 && worst.Rank() >= threshold {
		return &exitError{code: ExitFindings}
	}
	return nil
}

// parseFailOn returns the minimum severity rank that fails the run, or -1
// when findings never fail it.
func parseFailOn(s string) (int, error) {
	switch strings.ToLower(s) {
	case "none":
		return -1, nil
	case "info", "low", "medium", "high":
		return model.Severity(strings.ToLower(s)).Rank(), nil
	}
	return 0, fmt.Errorf("unknown --fail-on value %q: want high, medium, low, info or none", s)
}
