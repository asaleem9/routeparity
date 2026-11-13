package lab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// fieldManager names this tool as the owner of what it applies.
const fieldManager = "routeparity"

// kube bundles the clients the lab uses.
type kube struct {
	config *rest.Config
	client kubernetes.Interface
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func newKube(config *rest.Config) (*kube, error) {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(client.Discovery()))
	return &kube{config: config, client: client, dyn: dyn, mapper: mapper}, nil
}

// decodeObjects parses a multi-document YAML or JSON stream.
func decodeObjects(data []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := dec.Decode(obj)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(obj.Object) > 0 {
			out = append(out, obj)
		}
	}
}

// toUnstructured converts typed objects for the dynamic client.
func toUnstructured(objs ...runtime.Object) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, &unstructured.Unstructured{Object: content})
	}
	return out, nil
}

// apply creates or updates objects with server-side apply, in order.
func (k *kube) apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if meta.IsNoMatchError(err) {
			// The kind may have been registered after discovery was cached.
			k.mapper.Reset()
			mapping, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
		}
		var resource dynamic.ResourceInterface = k.dyn.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			namespace := obj.GetNamespace()
			if namespace == "" {
				namespace = metav1.NamespaceDefault
			}
			resource = k.dyn.Resource(mapping.Resource).Namespace(namespace)
		}
		if _, err := resource.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// waitForDeployment blocks until the Deployment has all its replicas
// available. On timeout the error describes the pods, since "timed out" alone
// never says why.
func (k *kube) waitForDeployment(ctx context.Context, namespace, name string, timeout time.Duration) error {
	var last *appsv1.Deployment
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		d, err := k.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// Not created yet, or the API server hiccuped: either way the
			// answer is to ask again until the timeout.
			return false, nil //nolint:nilerr // returning the error would stop the poll
		}
		last = d
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		return d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want, nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := "the Deployment was never created"
	if last != nil {
		detail = k.describePods(ctx, namespace, metav1.FormatLabelSelector(last.Spec.Selector))
	}
	return fmt.Errorf("deployment %s/%s was not available after %s: %s", namespace, name, timeout, detail)
}

// describePods summarises why pods are not running.
func (k *kube) describePods(ctx context.Context, namespace, selector string) string {
	// The caller's context has usually just expired; use a fresh, short one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	pods, err := k.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "could not list its pods: " + err.Error()
	}
	if len(pods.Items) == 0 {
		return "it has no pods"
	}
	var parts []string
	for _, pod := range pods.Items {
		state := string(pod.Status.Phase)
		for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			switch {
			case cs.State.Waiting != nil:
				state = fmt.Sprintf("%s: container %s is waiting (%s %s)", state, cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
				state = fmt.Sprintf("%s: container %s exited with %d (%s)", state, cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
			}
		}
		parts = append(parts, fmt.Sprintf("pod %s is %s", pod.Name, strings.TrimSpace(state)))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// runningPod returns the name of a running, ready pod matching the selector.
func (k *kube) runningPod(ctx context.Context, namespace, selector string) (string, error) {
	pods, err := k.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pod.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no ready pod matches %q in namespace %s", selector, namespace)
}

// forward opens port-forwards from 127.0.0.1 to a pod's ports and returns
// the local port for each remote port, in order. The forwards close when stop
// is called.
func (k *kube) forward(ctx context.Context, namespace, pod string, remotePorts ...int) (local []int, stop func(), err error) {
	transport, upgrader, err := spdy.RoundTripperFor(k.config)
	if err != nil {
		return nil, nil, err
	}
	url := k.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)

	specs := make([]string, len(remotePorts))
	for i, port := range remotePorts {
		specs[i] = fmt.Sprintf("0:%d", port) // 0 lets the kernel pick a free local port
	}
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, specs, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, nil, err
	}
	failed := make(chan error, 1)
	go func() { failed <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-failed:
		return nil, nil, fmt.Errorf("port-forward to pod %s/%s: %w", namespace, pod, err)
	case <-ctx.Done():
		close(stopCh)
		return nil, nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil {
		close(stopCh)
		return nil, nil, err
	}
	byRemote := map[uint16]uint16{}
	for _, p := range ports {
		byRemote[p.Remote] = p.Local
	}
	for _, remote := range remotePorts {
		local = append(local, int(byRemote[uint16(remote)]))
	}
	return local, func() { close(stopCh) }, nil
}
