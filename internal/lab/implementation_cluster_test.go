package lab

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKeptClusterPinsImplementationAndVersions(t *testing.T) {
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: portsConfigMapNamespace, Name: portsConfigMap},
		Data:       map[string]string{"nginx-http": "12345"},
	})
	s := &session{k: &kube{client: client}, cluster: &Cluster{Name: "routeparity-test"}}
	ctx := context.Background()
	if err := s.claimImplementation(ctx, NGINXGatewayFabric); err != nil {
		t.Fatal(err)
	}
	if err := s.claimImplementation(ctx, NGINXGatewayFabric); err != nil {
		t.Fatalf("same version cannot reuse cluster: %v", err)
	}
	upgraded, changedAPI := NGINXGatewayFabric, NGINXGatewayFabric
	upgraded.Version = "v99"
	changedAPI.GatewayAPIVersion = "v99"
	for _, impl := range []Implementation{EnvoyGateway, upgraded, changedAPI} {
		if err := s.claimImplementation(ctx, impl); err == nil || !strings.Contains(err.Error(), "different --name") {
			t.Errorf("mixed implementation/version accepted or unclear error: %v", err)
		}
	}
	cm, err := client.CoreV1().ConfigMaps(portsConfigMapNamespace).Get(ctx, portsConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["nginx-http"] != "12345" {
		t.Fatal("implementation record overwrote the host ports")
	}
}

func TestLegacyGatewayInstallationCannotBeReinterpreted(t *testing.T) {
	client := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: portsConfigMapNamespace, Name: portsConfigMap}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: EnvoyGateway.controller.Namespace, Name: EnvoyGateway.controller.Name}},
	)
	s := &session{k: &kube{client: client}, cluster: &Cluster{Name: "legacy"}}
	if err := s.claimImplementation(context.Background(), EnvoyGateway); err == nil || !strings.Contains(err.Error(), "older lab build") {
		t.Errorf("legacy cluster was silently measured with new version metadata: %v", err)
	}
}
