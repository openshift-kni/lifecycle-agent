package common

import (
	"context"
	"fmt"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CleanupLogf is an optional logger hook used by cleanup helpers in this file.
// If nil, logging is skipped.
type CleanupLogf func(msg string)

func logInfo(logf CleanupLogf, msg string) {
	if logf != nil {
		logf(msg)
	}
}

// CleanupACMResources deletes ACM-related CRDs and namespaces and waits until they're gone.
// It is safe to call multiple times.
func CleanupACMResources(ctx context.Context, c client.Client, logf CleanupLogf) error {
	// Ensure that dependent resources are deleted
	deleteOpts := []client.DeleteOption{
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	}

	interval := 10 * time.Second
	maxRetries := 90 // ~15 minutes

	// Trigger deletion for any remaining ACM CRDs
	acmCrds := currentAcmCrds(ctx, c, logf)
	if len(acmCrds) > 0 {
		logInfo(logf, "Deleting ACM CRDs")

		for _, crdName := range acmCrds {
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: crdName,
				},
			}
			logInfo(logf, fmt.Sprintf("Deleting CRD %s", crdName))
			if err := c.Delete(ctx, crd, deleteOpts...); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete CRD %s: %w", crdName, err)
			}
		}

		// Verify ACM CRDs have been deleted
		current := 0
		logInfo(logf, "Waiting until ACM CRDs are deleted")
		for len(currentAcmCrds(ctx, c, logf)) > 0 {
			if current < maxRetries {
				time.Sleep(interval)
				current += 1
			} else {
				return fmt.Errorf("timed out waiting for ACM CRD deletion")
			}
		}
	} else {
		logInfo(logf, "No ACM CRDs found")
	}

	// Trigger deletion for any remaining ACM namespaces
	acmNamespaces := currentAcmNamespaces(ctx, c, logf)
	if len(acmNamespaces) > 0 {
		logInfo(logf, "Deleting ACM namespaces")
		for _, nsName := range acmNamespaces {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
				},
			}
			logInfo(logf, fmt.Sprintf("Deleting namespace %s", nsName))
			if err := c.Delete(ctx, ns, deleteOpts...); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete namespace %s: %w", nsName, err)
			}
		}

		// Verify ACM namespaces have been deleted
		current := 0
		logInfo(logf, "Waiting until ACM namespaces are deleted")
		for len(currentAcmNamespaces(ctx, c, logf)) > 0 {
			if current < maxRetries {
				time.Sleep(interval)
				current += 1
			} else {
				return fmt.Errorf("timed out waiting for ACM namespace deletion")
			}
		}
	} else {
		logInfo(logf, "No ACM namespaces found")
	}

	return nil
}

// CleanupClusterResources cleans up ACM and other cluster resources that can block seed creation / pivot flows.
// It is safe to call multiple times.
func CleanupClusterResources(ctx context.Context, c client.Client, logf CleanupLogf) error {
	// ACM CRDs + namespaces first (namespace deletion can be blocked by CRDs/resources).
	if err := CleanupACMResources(ctx, c, logf); err != nil {
		return err
	}

	deleteOpts := []client.DeleteOption{
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	}

	// Delete remaining cluster resources leftover from ACM (or install)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "assisted-installer",
		},
	}
	if err := c.Delete(ctx, ns, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete assisted-installer namespace: %w", err)
	}

	roles := []string{
		"klusterlet",
		"klusterlet-bootstrap-kubeconfig",
		"open-cluster-management:klusterlet-admin-aggregate-clusterrole",
	}
	for _, role := range roles {
		roleStruct := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: role,
			},
		}
		if err := c.Delete(ctx, roleStruct, deleteOpts...); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete clusterrole %s: %w", role, err)
		}
	}

	roleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "klusterlet",
		},
	}
	if err := c.Delete(ctx, roleBinding, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete klusterlet clusterrolebinding: %w", err)
	}

	// If observability is enabled, there may be a copy of the accessor secret in openshift-monitoring namespace
	observabilitySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "openshift-monitoring",
			Name:      "observability-alertmanager-accessor",
		},
	}
	if err := c.Delete(ctx, observabilitySecret, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete observability secret: %w", err)
	}

	return nil
}

// currentAcmNamespaces returns a list of existing ACM namespaces on the cluster.
// In case of list errors, it logs and returns an empty list (to match existing controller behavior).
func currentAcmNamespaces(ctx context.Context, c client.Client, logf CleanupLogf) (acmNsList []string) {
	namespaces := &corev1.NamespaceList{}
	if err := c.List(ctx, namespaces); err != nil {
		if client.IgnoreNotFound(err) != nil {
			logInfo(logf, fmt.Sprintf("Error when checking namespaces: %s", err.Error()))
		}
		return
	}

	re := regexp.MustCompile(`^open-cluster-management-agent`)
	for _, ns := range namespaces.Items {
		if re.MatchString(ns.ObjectMeta.Name) {
			acmNsList = append(acmNsList, ns.ObjectMeta.Name)
		}
	}
	return
}

// currentAcmCrds returns a list of existing ACM CRDs on the cluster.
// In case of list errors, it logs and returns an empty list (to match existing controller behavior).
func currentAcmCrds(ctx context.Context, c client.Client, logf CleanupLogf) (acmCrdList []string) {
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := c.List(ctx, crds); err != nil {
		if client.IgnoreNotFound(err) != nil {
			logInfo(logf, fmt.Sprintf("Error when checking CRDs: %s", err.Error()))
		}
		return
	}

	re := regexp.MustCompile(`\.open-cluster-management\.io$`)
	for _, crd := range crds.Items {
		if re.MatchString(crd.ObjectMeta.Name) {
			acmCrdList = append(acmCrdList, crd.ObjectMeta.Name)
		}
	}
	return
}
