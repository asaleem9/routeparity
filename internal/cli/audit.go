package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/audit"
	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/report"
	"github.com/asaleem9/routeparity/internal/source"
)

type auditFlags struct {
	filenames        []string
	ingressClass     string
	includeClassless bool
	emitter          string
	noConvert        bool
	output           string
	failOn           string
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
	cmd.Flags().StringSliceVarP(&flags.filenames, "filename", "f", nil,
		"manifest file or directory to read; \"-\" reads stdin (repeatable)")
	cmd.Flags().StringVar(&flags.ingressClass, "ingress-class", model.DefaultIngressClass,
		"audit Ingresses of this class")
	cmd.Flags().BoolVar(&flags.includeClassless, "include-classless", false,
		"also audit Ingresses that name no class")
	cmd.Flags().StringVar(&flags.emitter, "emitter", "standard",
		"ingress2gateway emitter whose warnings to include (standard, envoy-gateway)")
	cmd.Flags().BoolVar(&flags.noConvert, "no-convert", false,
		"do not run ingress2gateway to collect its warnings")
	cmd.Flags().StringVarP(&flags.output, "output", "o", "text", "output format: text or json")
	cmd.Flags().StringVar(&flags.failOn, "fail-on", "medium",
		"exit with status 1 when a finding has at least this severity: high, medium, low, info or none")
	_ = cmd.MarkFlagRequired("filename")
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

	loaded, err := source.LoadPaths(flags.filenames, streams.In)
	if err != nil {
		return err
	}
	for _, warning := range loaded.Warnings {
		fmt.Fprintf(streams.Err, "warning: %s\n", warning)
	}

	world := model.NewWorld(loaded.Objects, model.WorldOptions{
		IngressClass:     flags.ingressClass,
		IncludeClassless: flags.includeClassless,
	})
	if len(world.Ingresses) == 0 {
		return fmt.Errorf("no Ingress of class %q found in the input%s",
			world.IngressClass, describeSkipped(world.Skipped))
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

// describeSkipped summarises which classes the input did contain, so that a
// wrong --ingress-class is easy to spot.
func describeSkipped(skipped []model.SkippedIngress) string {
	if len(skipped) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, s := range skipped {
		counts[s.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason, n := range counts {
		reasons = append(reasons, fmt.Sprintf("%d whose %s", n, reason))
	}
	sort.Strings(reasons)
	return fmt.Sprintf(" (%d Ingresses were skipped: %s)", len(skipped), strings.Join(reasons, "; "))
}
