package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/lab"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/probe"
	"github.com/asaleem9/routeparity/internal/source"
)

const (
	// echoRepository is where release builds of the echo backend are published.
	echoRepository = "ghcr.io/asaleem9/routeparity-echo"
	// localEchoImage is what `make echo-image` builds.
	localEchoImage = "routeparity-echo:dev"
)

var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// defaultEchoImage picks the echo image for this build: the published image
// of the same version for a release, and the locally built one otherwise,
// since an unreleased build has no published image to match it.
func defaultEchoImage(version string) string {
	if releaseVersion.MatchString(version) {
		return echoRepository + ":" + version
	}
	return localEchoImage
}

func newLabCommand(streams Streams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lab",
		Short: "Run your Ingresses on a local cluster to measure how they behave",
		Long: "lab reproduces your Ingresses in a kind cluster on this machine, with every Service\n" +
			"replaced by an echo backend that reports the request it received. Nothing is sent to\n" +
			"any cluster of yours.\n\n" +
			"It needs Docker (or Podman or nerdctl) running.",
	}
	cmd.AddCommand(newLabRunCommand(streams))
	cmd.AddCommand(newLabRecordCommand(streams))
	cmd.AddCommand(newLabDownCommand(streams))
	return cmd
}

func newLabRecordCommand(streams Streams) *cobra.Command {
	var (
		world       worldFlags
		send        sendFlags
		clusterName string
		echoImage   string
		keep        bool
		noMutations bool
		samples     int
		output      string
	)
	cmd := &cobra.Command{
		Use:   "record -f FILE... -o SNAPSHOT",
		Short: "Record how ingress-nginx treats your Ingresses, on a local cluster",
		Long: "record starts a kind cluster with ingress-nginx " + lab.IngressNGINXVersion + ", applies your Ingresses with every\n" +
			"Service replaced by an echo backend, sends the probes, and writes a snapshot.\n\n" +
			"The snapshot is real ingress-nginx behaviour for your configuration, including what\n" +
			"each backend received. Compare a Gateway against it with `routeparity verify`.\n\n" +
			"Annotations that need something the lab does not have, such as an external\n" +
			"authentication service, are removed before applying and listed in the snapshot as\n" +
			"not verified. The lab runs the final ingress-nginx release with its default settings;\n" +
			"your controller's version and ConfigMap may differ.",
		Example: "  routeparity lab record -f manifests/ -o ingress-nginx.json\n" +
			"  routeparity lab record -f manifests/ -o ingress-nginx.json --keep   # reuse the cluster next time\n" +
			"  routeparity verify ingress-nginx.json --address gateway.staging:80",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if samples < 1 {
				return errors.New("--samples must be at least 1")
			}
			opts, err := send.options()
			if err != nil {
				return err
			}
			w, err := world.load(streams)
			if err != nil {
				return err
			}
			if echoImage == "" {
				echoImage = defaultEchoImage(Version())
			}

			start := time.Now()
			snap, err := lab.RecordBaseline(cmd.Context(), w, lab.Options{
				ClusterName: clusterName,
				EchoImage:   echoImage,
				Keep:        keep,
				Probes:      probe.Options{Mutations: !noMutations, Samples: samples},
				Send:        opts,
				ToolVersion: Version(),
				Progress: func(format string, args ...any) {
					fmt.Fprintf(streams.Err, "[%3ds] %s\n", int(time.Since(start).Seconds()), fmt.Sprintf(format, args...))
				},
			})
			if err != nil {
				return err
			}
			if err := requireResponses("ingress-nginx in the lab", snap); err != nil {
				return err
			}
			if output == "-" {
				if err := snap.Write(streams.Out); err != nil {
					return err
				}
			} else if err := writeFile(output, snap.Write); err != nil {
				return err
			}

			fmt.Fprintf(streams.Err, "recorded %d observation(s) of ingress-nginx %s\n", len(snap.Observations), lab.IngressNGINXVersion)
			if n := len(snap.Unverified); n > 0 {
				fmt.Fprintf(streams.Err, "%d thing(s) could not be reproduced and are listed in the snapshot as not verified:\n", n)
				for _, u := range snap.Unverified {
					line := fmt.Sprintf("  %s: %s", u.Reason, u.Feature)
					if u.Object != nil {
						line += " (" + u.Object.String() + ")"
					}
					fmt.Fprintln(streams.Err, line)
				}
			}
			return nil
		},
	}
	world.add(cmd)
	// The lab's cluster serves nobody else, so there is no need to hold back.
	send.addWithDefaults(cmd, 8, 0)
	addSamplesFlag(cmd, &samples)
	cmd.Flags().StringVar(&clusterName, "name", lab.DefaultClusterName, "name of the kind cluster to create or reuse")
	cmd.Flags().StringVar(&echoImage, "echo-image", "",
		"echo backend image; a local Docker image of this name is copied into the cluster (default: the image matching this build)")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the cluster running afterwards, which makes the next run much faster")
	cmd.Flags().BoolVar(&noMutations, "no-mutations", false,
		"only request the paths written in the manifests; by default the lab also sends variations of each path")
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write the snapshot to; \"-\" writes to stdout")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func newLabDownCommand(streams Streams) *cobra.Command {
	var clusterName string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete the lab cluster",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			err := lab.DeleteCluster(clusterName, "")
			if errors.Is(err, lab.ErrNoCluster) {
				fmt.Fprintf(streams.Err, "no kind cluster named %s\n", clusterName)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(streams.Err, "deleted kind cluster %s\n", clusterName)
			return nil
		},
	}
	cmd.Flags().StringVar(&clusterName, "name", lab.DefaultClusterName, "name of the kind cluster to delete")
	return cmd
}

func newLabRunCommand(streams Streams) *cobra.Command {
	var (
		world          worldFlags
		send           sendFlags
		compare        compareFlags
		clusterName    string
		echoImage      string
		keep           bool
		noMutations    bool
		samples        int
		gatewayFiles   []string
		emitter        string
		implementation string
		saveBaseline   string
		saveCandidate  string
	)
	cmd := &cobra.Command{
		Use:   "run -f FILE... [--gateway FILE...]",
		Short: "Compare ingress-nginx with a Gateway implementation, on a local cluster",
		Long: "run starts a kind cluster with ingress-nginx and a Gateway API implementation side by\n" +
			"side, puts the same echo backends behind both, sends each probe through each, and\n" +
			"reports where they differ: in what the client received and in what the backend received.\n" +
			"Each difference names the behaviour that caused it and, where one exists, a change that removes it.\n\n" +
			"The Gateway configuration comes from --gateway, or from Gateway API objects among the\n" +
			"-f manifests. With neither, your Ingresses are converted with ingress2gateway and the\n" +
			"result is what gets tested.\n\n" +
			"A difference here is measured, not predicted. The lab runs the final ingress-nginx\n" +
			"release with default settings; your controller's version and ConfigMap may differ.\n\n" +
			"Exit status is the same as for `routeparity diff`.",
		Example: "  routeparity lab run -f ingress/                          # test what ingress2gateway produces\n" +
			"  routeparity lab run -f ingress/ --gateway gateway/       # test the routes you wrote\n" +
			"  routeparity lab run -f ingress/ --emitter envoy-gateway --markdown summary.md",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := compare.validate(); err != nil {
				return err
			}
			if samples < 1 {
				return errors.New("--samples must be at least 1")
			}
			impl, ok := lab.ImplementationByName(implementation)
			if !ok {
				return fmt.Errorf("unknown --implementation %q: the lab can run %s", implementation, strings.Join(lab.ImplementationNames(), ", "))
			}
			opts, err := send.options()
			if err != nil {
				return err
			}
			w, err := world.load(streams)
			if err != nil {
				return err
			}
			candidate, err := resolveCandidate(cmd.Context(), streams, w, gatewayFiles, emitter)
			if err != nil {
				return err
			}
			if echoImage == "" {
				echoImage = defaultEchoImage(Version())
			}

			start := time.Now()
			base, cand, err := lab.Run(cmd.Context(), w, candidate, impl, lab.Options{
				ClusterName: clusterName,
				EchoImage:   echoImage,
				Keep:        keep,
				Probes:      probe.Options{Mutations: !noMutations, Samples: samples},
				Send:        opts,
				ToolVersion: Version(),
				Progress: func(format string, args ...any) {
					fmt.Fprintf(streams.Err, "[%3ds] %s\n", int(time.Since(start).Seconds()), fmt.Sprintf(format, args...))
				},
			})
			if err != nil {
				return err
			}
			if err := requireResponses("ingress-nginx in the lab", base); err != nil {
				return err
			}
			if err := requireResponses(impl.Name+" in the lab", cand); err != nil {
				return err
			}
			if saveBaseline != "" {
				if err := writeFile(saveBaseline, base.Write); err != nil {
					return err
				}
			}
			if saveCandidate != "" {
				if err := writeFile(saveCandidate, cand.Write); err != nil {
					return err
				}
			}
			fmt.Fprintln(streams.Err)
			return compare.finish(streams, base, cand, w)
		},
	}
	world.add(cmd)
	send.addWithDefaults(cmd, 8, 0)
	compare.add(cmd)
	addSamplesFlag(cmd, &samples)
	cmd.Flags().StringSliceVar(&gatewayFiles, "gateway", nil,
		"file or directory with the Gateway API configuration to test (repeatable); without it the Ingresses are converted")
	cmd.Flags().StringVar(&emitter, "emitter", "standard",
		"ingress2gateway emitter to convert with when no Gateway configuration is given (standard, envoy-gateway)")
	cmd.Flags().StringVar(&implementation, "implementation", lab.EnvoyGateway.Name,
		"Gateway API implementation to run: "+strings.Join(lab.ImplementationNames(), ", "))
	cmd.Flags().StringVar(&clusterName, "name", lab.DefaultClusterName, "name of the kind cluster to create or reuse")
	cmd.Flags().StringVar(&echoImage, "echo-image", "",
		"echo backend image; a local Docker image of this name is copied into the cluster (default: the image matching this build)")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the cluster running afterwards, which makes the next run much faster")
	cmd.Flags().BoolVar(&noMutations, "no-mutations", false,
		"only request the paths written in the manifests; by default the lab also sends variations of each path")
	cmd.Flags().StringVar(&saveBaseline, "save-baseline", "", "also write the ingress-nginx snapshot to this file")
	cmd.Flags().StringVar(&saveCandidate, "save-candidate", "", "also write the Gateway snapshot to this file")
	return cmd
}

// resolveCandidate decides which Gateway configuration a lab run tests: the
// files given with --gateway, else Gateway API objects found among the -f
// manifests, else the converter's output for the Ingresses.
func resolveCandidate(ctx context.Context, streams Streams, w *model.World, gatewayFiles []string, emitter string) (*lab.Candidate, error) {
	if len(gatewayFiles) > 0 {
		loaded, err := source.LoadPaths(gatewayFiles, streams.In)
		if err != nil {
			return nil, err
		}
		for _, warning := range loaded.Warnings {
			fmt.Fprintf(streams.Err, "warning: %s\n", warning)
		}
		candidate := &lab.Candidate{
			Gateways:   loaded.Objects.Gateways,
			HTTPRoutes: loaded.Objects.HTTPRoutes,
			Other:      loaded.Objects.GatewayExtras,
			Source:     "the --gateway manifests",
		}
		if candidate.Empty() {
			return nil, fmt.Errorf("the --gateway manifests contain no Gateway and no HTTPRoute")
		}
		return candidate, nil
	}

	if len(w.Gateways) > 0 || len(w.HTTPRoutes) > 0 {
		return &lab.Candidate{
			Gateways:   w.Gateways,
			HTTPRoutes: w.HTTPRoutes,
			Other:      w.GatewayExtras,
			Source:     "the Gateway API objects in the manifests",
		}, nil
	}

	manifests, err := convert.Manifests(w)
	if err != nil {
		return nil, err
	}
	out, err := convert.InProcess{}.Convert(ctx, bytes.NewReader(manifests), convert.Options{IngressClass: w.IngressClass, Emitter: emitter})
	if err != nil {
		return nil, fmt.Errorf("the Ingresses could not be converted, so there is no Gateway configuration to test; "+
			"write one and pass it with --gateway: %w", err)
	}
	candidate := &lab.Candidate{
		Gateways:   out.Gateways,
		HTTPRoutes: out.HTTPRoutes,
		Other:      out.Other,
		Source:     fmt.Sprintf("%s (%s emitter)", out.Converter, emitter),
	}
	// What the converter says it did not translate is, by its own account,
	// missing from the configuration under test.
	for _, n := range out.Notices {
		if n.Level != "WARNING" && n.Level != "ERROR" {
			continue
		}
		u := model.Unverified{Reason: model.ReasonNotConverted, Feature: n.Message, Detail: "reported by ingress2gateway"}
		if len(n.Objects) > 0 {
			ref := n.Objects[0]
			u.Object = &ref
		}
		candidate.Unverified = append(candidate.Unverified, u)
	}
	if candidate.Empty() {
		return nil, errors.New("ingress2gateway produced no Gateway and no HTTPRoute for these Ingresses")
	}
	return candidate, nil
}
