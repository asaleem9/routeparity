package lab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/probe"
	"github.com/asaleem9/routeparity/internal/prober"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "routeparity"

	ingressNGINXNamespace  = "ingress-nginx"
	ingressNGINXDeployment = "ingress-nginx-controller"
	ingressNGINXSelector   = "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"

	controllerTimeout = 5 * time.Minute // includes pulling the controller image
	echoTimeout       = 2 * time.Minute
	routesTimeout     = 90 * time.Second
	hostCheckTimeout  = 5 * time.Second
	// settleRounds bounds how often the baseline is re-recorded while waiting
	// for two consecutive recordings to agree.
	settleRounds = 8
)

// ReasonNotLoaded marks a host that ingress-nginx never started routing.
const ReasonNotLoaded model.UnverifiedReason = "not-loaded"

// Options controls a lab run.
type Options struct {
	// ClusterName is the kind cluster to create or reuse.
	ClusterName string
	// EchoImage is the echo backend image. A local Docker image of that name
	// is copied into the cluster; otherwise the cluster pulls it.
	EchoImage string
	// Keep leaves the cluster running when the run ends.
	Keep bool
	// Probes selects which probes are sent.
	Probes probe.Options
	// Send controls how they are sent.
	Send prober.Options
	// ToolVersion is recorded in the snapshot.
	ToolVersion string
	// Progress receives one line per step. It may be nil.
	Progress func(format string, args ...any)
}

func (o Options) progress(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(format, args...)
	}
}

// RecordBaseline reproduces a World on ingress-nginx in a local cluster, with
// echo backends, and records how it behaves.
func RecordBaseline(ctx context.Context, w *model.World, opts Options) (snap *model.Snapshot, err error) {
	if opts.ClusterName == "" {
		opts.ClusterName = DefaultClusterName
	}
	plan, err := BuildPlan(w, opts.EchoImage)
	if err != nil {
		return nil, err
	}
	if len(plan.EchoDeployments) == 0 {
		return nil, errors.New("the Ingresses route to no Service, so there is nothing to reproduce")
	}

	opts.progress("starting kind cluster %s (Kubernetes %s)", opts.ClusterName, KubernetesVersion)
	cluster, err := EnsureCluster(opts.ClusterName)
	if err != nil {
		return nil, err
	}
	if !cluster.Created {
		opts.progress("reusing the existing cluster")
	}
	defer func() {
		if opts.Keep {
			opts.progress("cluster %s kept; remove it with `routeparity lab down`", cluster.Name)
			return
		}
		opts.progress("deleting cluster %s", cluster.Name)
		if delErr := cluster.Delete(); delErr != nil && err == nil {
			err = delErr
		}
	}()

	k, err := newKube(cluster.Config)
	if err != nil {
		return nil, err
	}

	loaded, err := cluster.LoadLocalImage(ctx, opts.EchoImage)
	if err != nil {
		return nil, err
	}
	switch {
	case loaded:
		opts.progress("loaded the local image %s into the cluster", opts.EchoImage)
	case !strings.Contains(opts.EchoImage, "/"):
		// A name without a registry can only come from the local daemon.
		return nil, fmt.Errorf("the echo image %q is not in the local Docker daemon; build it with `make echo-image` or pass --echo-image", opts.EchoImage)
	}

	opts.progress("installing ingress-nginx %s", IngressNGINXVersion)
	controller, err := decodeObjects(ingressNGINXManifest)
	if err != nil {
		return nil, fmt.Errorf("embedded ingress-nginx manifest: %w", err)
	}
	if err := k.apply(ctx, controller); err != nil {
		return nil, err
	}
	// The admission webhook races with the job that issues its certificate
	// and would reject Ingresses until that finishes. Routing does not
	// depend on it.
	err = k.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, "ingress-nginx-admission", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("remove the ingress-nginx admission webhook: %w", err)
	}

	if err := removeStaleIngresses(ctx, k); err != nil {
		return nil, err
	}
	support, err := labelled(plan.Support...)
	if err != nil {
		return nil, err
	}
	if err := k.apply(ctx, support); err != nil {
		return nil, err
	}

	if err := k.waitForDeployment(ctx, ingressNGINXNamespace, ingressNGINXDeployment, controllerTimeout); err != nil {
		return nil, err
	}
	for _, d := range plan.EchoDeployments {
		if err := k.waitForDeployment(ctx, d.Namespace, d.Name, echoTimeout); err != nil {
			return nil, err
		}
	}

	opts.progress("applying %d Ingress(es) with %d Service(s) replaced by the echo backend", len(plan.Ingresses), countServices(plan))
	ingresses := make([]runtime.Object, len(plan.Ingresses))
	for i, ing := range plan.Ingresses {
		ingresses[i] = ing
	}
	ingressObjs, err := labelled(ingresses...)
	if err != nil {
		return nil, err
	}
	// One at a time and in order: ingress-nginx gives precedence to the
	// oldest Ingress, and creation order is the only way to reproduce that.
	if err := k.apply(ctx, ingressObjs); err != nil {
		return nil, err
	}

	pod, err := k.runningPod(ctx, ingressNGINXNamespace, ingressNGINXSelector)
	if err != nil {
		return nil, err
	}
	ports, stop, err := k.forward(ctx, ingressNGINXNamespace, pod, 80, 443)
	if err != nil {
		return nil, err
	}
	defer stop()
	target := prober.Target{
		HTTP:  fmt.Sprintf("127.0.0.1:%d", ports[0]),
		HTTPS: fmt.Sprintf("127.0.0.1:%d", ports[1]),
	}

	set := probe.Generate(w, opts.Probes)
	unverified := append(append([]model.Unverified{}, plan.Unverified...), set.Unverified...)

	opts.progress("waiting for ingress-nginx to load the configuration")
	if err := waitForConfiguration(ctx, k, plan, target, opts.Send); err != nil {
		return nil, err
	}
	// The configuration is current, so a host that is not routed now never
	// will be; one short pass is enough to find those.
	for _, host := range waitForRoutes(ctx, target, set.Probes, opts.Send, hostCheckTimeout) {
		unverified = append(unverified, model.Unverified{
			Reason:  ReasonNotLoaded,
			Feature: host,
			Detail:  "ingress-nginx never started routing this host; it may have rejected the Ingress",
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts.progress("sending %d probe(s)", len(set.Probes))
	meta := model.TargetMeta{
		Name:           "ingress-nginx",
		Kind:           model.TargetIngressNGINX,
		Mode:           model.ModeLab,
		Address:        "kind cluster " + cluster.Name,
		Implementation: "ingress-nginx",
		Versions: map[string]string{
			"ingress-nginx": IngressNGINXVersion,
			"kubernetes":    KubernetesVersion,
			"echo":          opts.EchoImage,
		},
		ToolVersion: opts.ToolVersion,
	}
	snap, settled, err := recordSettled(ctx, target, meta, set.Probes, unverified, opts.Send)
	if err != nil {
		return nil, err
	}
	if !settled {
		snap.Unverified = append(snap.Unverified, model.Unverified{
			Reason:  ReasonNotLoaded,
			Feature: "stable baseline",
			Detail: fmt.Sprintf("ingress-nginx still answered differently between two consecutive recordings after %d attempts; "+
				"the snapshot is the last recording", settleRounds),
		})
	}
	return snap, nil
}

// waitForConfiguration applies a sentinel Ingress and waits until
// ingress-nginx routes it.
//
// ingress-nginx reloads a few seconds after Ingresses change, not at once.
// Until then it serves the previous configuration, consistently, so neither
// "the routes answer" nor "two recordings agree" shows that the change has
// taken effect: in a reused cluster both hold for the configuration of the
// run before. The sentinel's host is new in every run, so the only
// configuration that can route it is one built after this run's changes.
func waitForConfiguration(ctx context.Context, k *kube, plan *Plan, target prober.Target, send prober.Options) error {
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sentinel, host := plan.Sentinel(hex.EncodeToString(nonce))
	if sentinel == nil {
		return errors.New("there is no echo Service to route the sentinel Ingress to")
	}
	objs, err := labelled(sentinel)
	if err != nil {
		return err
	}
	if err := k.apply(ctx, objs); err != nil {
		return err
	}

	probe := model.Probe{Host: host, RawTarget: "/", Origin: model.Origin{Source: model.OriginControl, Mutation: "sentinel"}}
	if err := probe.Seal(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, routesTimeout)
	defer cancel()
	var last string
	for {
		obs := prober.Send(ctx, target.HTTP, probe, send)
		switch {
		case obs.Backend != nil:
			return nil
		case obs.Client.Error != "":
			last = obs.Client.Error
		default:
			last = fmt.Sprintf("status %d", obs.Client.Status)
		}
		if sleep(ctx, 500*time.Millisecond) != nil {
			return fmt.Errorf("ingress-nginx did not load the configuration within %s (last answer for the sentinel host: %s)", routesTimeout, last)
		}
	}
}

// recordSettled records the target until two consecutive recordings agree.
// waitForConfiguration has already established that this run's Ingresses are
// in effect. This is a second check, against whatever else can make a target
// answer differently from one moment to the next, such as a Secret that the
// controller picks up after the Ingress that uses it.
func recordSettled(ctx context.Context, target prober.Target, meta model.TargetMeta, probes []model.Probe,
	unverified []model.Unverified, send prober.Options) (*model.Snapshot, bool, error) {
	var previous *model.Snapshot
	for round := 0; round < settleRounds; round++ {
		if round > 0 {
			if err := sleep(ctx, time.Second); err != nil {
				return nil, false, err
			}
		}
		snap, err := prober.Record(ctx, target, meta, probes, unverified, send, time.Now())
		if err != nil {
			return nil, false, err
		}
		if previous != nil {
			res := diff.Compare(previous, snap, diff.Options{})
			if res.Summary.Diff == 0 && res.Summary.Error == 0 {
				return snap, true, nil
			}
		}
		previous = snap
	}
	return previous, false, nil
}

// waitForRoutes polls until every host has a configured path that the target
// answers differently from a request for an unknown host, which is the sign
// that the host's routes are loaded. It returns the hosts that never got there.
func waitForRoutes(ctx context.Context, target prober.Target, probes []model.Probe, send prober.Options, timeout time.Duration) []string {
	var control *model.Probe
	pending := map[string]model.Probe{}
	var order []string
	for i, p := range probes {
		switch {
		case p.Origin.Source == model.OriginControl:
			control = &probes[i]
		case p.Origin.Source == model.OriginDerived && p.Scheme == "http":
			if _, ok := pending[p.Host]; !ok {
				pending[p.Host] = p
				order = append(order, p.Host)
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for len(pending) > 0 {
		unroutedStatus, unroutedBody, haveControl := 0, "", false
		if control != nil {
			if obs := prober.Send(ctx, target.HTTP, *control, send); obs.Client.Error == "" && obs.Backend == nil {
				unroutedStatus, unroutedBody, haveControl = obs.Client.Status, obs.Client.BodySHA256, true
			}
		}
		for host, p := range pending {
			obs := prober.Send(ctx, target.HTTP, p, send)
			if obs.Client.Error != "" {
				continue
			}
			unrouted := obs.Backend == nil && (obs.Client.Status == 404 || obs.Client.Status == 503)
			if haveControl {
				unrouted = obs.Backend == nil && obs.Client.Status == unroutedStatus && obs.Client.BodySHA256 == unroutedBody
			}
			if !unrouted {
				delete(pending, host)
			}
		}
		if len(pending) == 0 || sleep(ctx, time.Second) != nil {
			break
		}
	}

	var notLoaded []string
	for _, host := range order {
		if _, still := pending[host]; still {
			notLoaded = append(notLoaded, host)
		}
	}
	return notLoaded
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// labelled converts objects for applying and marks them as the lab's, so
// that a later run in a reused cluster can find what an earlier one left.
func labelled(objs ...runtime.Object) ([]*unstructured.Unstructured, error) {
	out, err := toUnstructured(objs...)
	if err != nil {
		return nil, err
	}
	for _, obj := range out {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[managedByLabel] = managedByValue
		obj.SetLabels(labels)
		// Server-side apply rejects these when they are present but empty.
		unstructured.RemoveNestedField(obj.Object, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(obj.Object, "status")
	}
	return out, nil
}

// removeStaleIngresses deletes the Ingresses a previous run left in a reused
// cluster. Left in place they would keep routing, and the recording would
// describe two configurations at once.
func removeStaleIngresses(ctx context.Context, k *kube) error {
	selector := managedByLabel + "=" + managedByValue
	list, err := k.client.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list Ingresses from a previous run: %w", err)
	}
	for _, ing := range list.Items {
		err := k.client.NetworkingV1().Ingresses(ing.Namespace).Delete(ctx, ing.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Ingress %s/%s from a previous run: %w", ing.Namespace, ing.Name, err)
		}
	}
	return nil
}

func countServices(plan *Plan) int {
	n := 0
	for _, obj := range plan.Support {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Service" {
			n++
		}
	}
	return n
}
