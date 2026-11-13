// Package cli wires the commands together.
package cli

import (
	"errors"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Exit codes. They are part of the tool's interface: CI pipelines branch on them.
const (
	// ExitOK means the run completed and found nothing at or above the
	// failure threshold.
	ExitOK = 0
	// ExitFindings means the run completed and found a divergence, or an
	// audit finding at or above the failure threshold.
	ExitFindings = 1
	// ExitUnverified means nothing diverged but something could not be
	// verified and strict mode was requested.
	ExitUnverified = 2
	// ExitError means the tool could not complete the run.
	ExitError = 3
)

// exitError carries an exit code out of a command without printing anything:
// the command has already reported its result.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// version is set at build time with -ldflags "-X ...cli.version=v1.2.3".
var version = ""

// Version reports the build version, falling back to the module version that
// `go install` records.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Streams are the command's standard streams, replaceable in tests.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// NewRootCommand builds the command tree.
func NewRootCommand(streams Streams) *cobra.Command {
	root := &cobra.Command{
		Use:   "routeparity",
		Short: "Check that a Gateway API setup behaves like the ingress-nginx setup it replaces",
		Long: "routeparity compares how two Kubernetes routing setups treat the same requests.\n\n" +
			"It exists for migrations from ingress-nginx to Gateway API, where manifests can\n" +
			"convert cleanly and still route differently.",
		Version:       Version(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.SetVersionTemplate("routeparity {{.Version}}\n")

	root.AddCommand(newAuditCommand(streams))
	root.AddCommand(newProbesCommand(streams))
	root.AddCommand(newRecordCommand(streams))
	root.AddCommand(newDiffCommand(streams))
	root.AddCommand(newLiveCommand(streams))
	root.AddCommand(newVerifyCommand(streams))
	root.AddCommand(newExplainCommand(streams))
	return root
}

// Run executes the command line and returns the process exit code.
func Run(args []string, streams Streams) int {
	root := NewRootCommand(streams)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	fmt.Fprintf(streams.Err, "routeparity: %v\n", err)
	return ExitError
}
