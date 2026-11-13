package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/rules"
)

func newExplainCommand(streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "explain [RULE-ID]",
		Short: "Describe a rule, or list all rules",
		Example: "  routeparity explain\n" +
			"  routeparity explain NGX-REGEX-002",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				for _, r := range rules.All() {
					m := r.Meta()
					fmt.Fprintf(streams.Out, "%-18s %-7s %s\n", m.ID, m.Severity, m.Title)
				}
				return nil
			}
			id := strings.ToUpper(args[0])
			r, ok := rules.ByID(id)
			if !ok {
				return fmt.Errorf("no rule with ID %q; run `routeparity explain` to list rules", args[0])
			}
			m := r.Meta()
			fmt.Fprintf(streams.Out, "%s  %s\nseverity: %s\n\n%s\n", m.ID, m.Title, m.Severity, m.Summary)
			if len(m.References) > 0 {
				fmt.Fprintln(streams.Out, "\nReferences:")
				for _, ref := range m.References {
					fmt.Fprintf(streams.Out, "  %s\n", ref)
				}
			}
			return nil
		},
	}
}
