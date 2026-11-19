package lab

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// claimImplementation pins the Gateway implementation and CRD bundle for a
// kept cluster. Gateway API bundles differ in version and channel, and their
// safe-upgrade admission policy correctly prevents some replacements. More
// importantly, mixing bundles would invalidate the snapshot's version claim.
func (s *session) claimImplementation(ctx context.Context, impl Implementation) error {
	cms := s.k.client.CoreV1().ConfigMaps(portsConfigMapNamespace)
	cm, err := cms.Get(ctx, portsConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read lab implementation record: %w", err)
	}
	identity := impl.Name + " " + impl.Version + " (Gateway API " + impl.GatewayAPIVersion + ")"
	if prior := cm.Data["gateway-implementation"]; prior != "" {
		if prior == identity {
			return nil
		}
		return fmt.Errorf("kind cluster %q belongs to %s, not %s; use a different --name or delete it with `routeparity lab down --name %s`", s.cluster.Name, prior, identity, s.cluster.Name)
	}
	// Clusters kept by older builds have no implementation record. Do not
	// infer the CRD version from the controller's image tag.
	for _, name := range ImplementationNames() {
		known, _ := ImplementationByName(name)
		_, err := s.k.client.AppsV1().Deployments(known.controller.Namespace).Get(ctx, known.controller.Name, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("kind cluster %q has an unversioned Gateway installation from an older lab build; use a different --name or delete it with `routeparity lab down --name %s`", s.cluster.Name, s.cluster.Name)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("inspect existing Gateway installation: %w", err)
		}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["gateway-implementation"] = identity
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("record lab implementation: %w", err)
	}
	return nil
}
