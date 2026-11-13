package cli

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/probe"
	"github.com/asaleem9/routeparity/internal/prober"
	"github.com/asaleem9/routeparity/internal/source"
)

// worldFlags select the manifests and the Ingress class to work on.
type worldFlags struct {
	filenames        []string
	ingressClass     string
	includeClassless bool
}

func (f *worldFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringSliceVarP(&f.filenames, "filename", "f", nil,
		"manifest file or directory to read; \"-\" reads stdin (repeatable)")
	cmd.Flags().StringVar(&f.ingressClass, "ingress-class", model.DefaultIngressClass,
		"use Ingresses of this class")
	cmd.Flags().BoolVar(&f.includeClassless, "include-classless", false,
		"also use Ingresses that name no class")
	_ = cmd.MarkFlagRequired("filename")
}

// load reads the manifests and builds the World. Loader warnings go to
// stderr; having no Ingress of the class is an error, because every command
// would otherwise succeed while doing nothing.
func (f *worldFlags) load(streams Streams) (*model.World, error) {
	loaded, err := source.LoadPaths(f.filenames, streams.In)
	if err != nil {
		return nil, err
	}
	for _, warning := range loaded.Warnings {
		fmt.Fprintf(streams.Err, "warning: %s\n", warning)
	}
	world := model.NewWorld(loaded.Objects, model.WorldOptions{
		IngressClass:     f.ingressClass,
		IncludeClassless: f.includeClassless,
	})
	if len(world.Ingresses) == 0 {
		return nil, fmt.Errorf("no Ingress of class %q found in the input%s",
			world.IngressClass, describeSkipped(world.Skipped))
	}
	return world, nil
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

// probeFlags choose which probes are generated.
type probeFlags struct {
	mutations bool
}

func (f *probeFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.mutations, "mutations", false,
		"also send variations of each path (changed case, extra segments, dot segments, doubled slashes). "+
			"They find the most differences, and a WAF in front of a live endpoint may object to them")
}

func (f *probeFlags) generate(w *model.World) probe.Set {
	return probe.Generate(w, probe.Options{Mutations: f.mutations})
}

// sendFlags control how probes are sent.
type sendFlags struct {
	timeout     time.Duration
	concurrency int
	rate        float64
}

func (f *sendFlags) add(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&f.timeout, "timeout", prober.DefaultTimeout, "time allowed for one probe")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", prober.DefaultConcurrency, "probes in flight at once")
	cmd.Flags().Float64Var(&f.rate, "rate", prober.DefaultRate, "probes started per second; 0 removes the limit")
}

func (f *sendFlags) options() (prober.Options, error) {
	if f.timeout <= 0 {
		return prober.Options{}, errors.New("--timeout must be positive")
	}
	if f.concurrency < 1 {
		return prober.Options{}, errors.New("--concurrency must be at least 1")
	}
	if f.rate < 0 {
		return prober.Options{}, errors.New("--rate must not be negative")
	}
	return prober.Options{
		Timeout:       f.timeout,
		Concurrency:   f.concurrency,
		RatePerSecond: f.rate,
		UserAgent:     "routeparity/" + Version() + " (+https://github.com/asaleem9/routeparity)",
	}, nil
}

// parseAddress accepts host:port, or a URL whose scheme supplies the port,
// and returns host:port. Empty input gives an empty address.
func parseAddress(flag, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	addr, defaultPort := value, ""
	switch {
	case strings.HasPrefix(addr, "http://"):
		addr, defaultPort = strings.TrimPrefix(addr, "http://"), "80"
	case strings.HasPrefix(addr, "https://"):
		addr, defaultPort = strings.TrimPrefix(addr, "https://"), "443"
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		if strings.Trim(addr[i:], "/") != "" {
			return "", fmt.Errorf("--%s %q: give an address, not a URL with a path; probes supply the paths", flag, value)
		}
		addr = addr[:i]
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && host != "" && port != "" {
		return addr, nil
	}
	if defaultPort != "" && addr != "" && !strings.Contains(addr, ":") {
		return net.JoinHostPort(addr, defaultPort), nil
	}
	return "", fmt.Errorf("--%s %q: want HOST:PORT", flag, value)
}

// target builds a prober target from an HTTP and an HTTPS address flag, at
// least one of which must be set.
func target(httpFlag, httpValue, httpsFlag, httpsValue string) (prober.Target, error) {
	httpAddr, err := parseAddress(httpFlag, httpValue)
	if err != nil {
		return prober.Target{}, err
	}
	httpsAddr, err := parseAddress(httpsFlag, httpsValue)
	if err != nil {
		return prober.Target{}, err
	}
	if httpAddr == "" && httpsAddr == "" {
		return prober.Target{}, fmt.Errorf("--%s or --%s is required", httpFlag, httpsFlag)
	}
	return prober.Target{HTTP: httpAddr, HTTPS: httpsAddr}, nil
}

// describeAddresses renders a target's addresses for snapshot metadata.
func describeAddresses(t prober.Target) string {
	var parts []string
	if t.HTTP != "" {
		parts = append(parts, t.HTTP)
	}
	if t.HTTPS != "" {
		parts = append(parts, t.HTTPS+" (TLS)")
	}
	return strings.Join(parts, ", ")
}

// requireResponses refuses a snapshot in which nothing answered. Comparing
// two such snapshots would report agreement between two failures.
func requireResponses(name string, snap *model.Snapshot) error {
	if len(snap.Observations) == 0 {
		return fmt.Errorf("no probe could be sent to %s", name)
	}
	classes := map[string]int{}
	for _, obs := range snap.Observations {
		if obs.Client.Error == "" {
			return nil
		}
		classes[obs.Client.Error]++
	}
	var parts []string
	for class, n := range classes {
		parts = append(parts, fmt.Sprintf("%s ×%d", class, n))
	}
	sort.Strings(parts)
	return fmt.Errorf("no probe got a response from %s (%s); check the address", name, strings.Join(parts, ", "))
}
