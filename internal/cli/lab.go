package cli

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/lab"
	"github.com/asaleem9/routeparity/internal/probe"
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
