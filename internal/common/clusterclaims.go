package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ClusterClaimPrefix = "lcm.openshift.io/ibu-"

func isClusterClaimCRDInstalled(ctx context.Context, client client.Client) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := client.Get(ctx, types.NamespacedName{
		Name: "clusterclaims.cluster.open-cluster-management.io",
	}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("could not get ClusterClaim crd: %w", err)
	}
	return true, nil
}

func CleanupClusterClaims(ctx context.Context, client client.Client, log *logr.Logger) error {
	if installed, err := isClusterClaimCRDInstalled(ctx, client); err != nil {
		return err
	} else if !installed {
		log.Info("ClusterClaim CRD not found; Skip creating ClusterClaim")
		return nil
	}
	cl := &clusterv1.ClusterClaimList{}
	err := client.List(ctx, cl)
	if err != nil {
		return fmt.Errorf("failed to list ClusterClaims: %w", err)
	}
	for _, cc := range cl.Items {
		if strings.HasPrefix(cc.Name, ClusterClaimPrefix) {
			err := client.Delete(ctx, &cc)
			if err != nil {
				return fmt.Errorf("failed to delete ClusterClaim: %w", err)
			}
		}
	}
	return nil
}

func CreateClusterClaim(ctx context.Context, client client.Client, log *logr.Logger, stage string, status string) error {
	if installed, err := isClusterClaimCRDInstalled(ctx, client); err != nil {
		return err
	} else if !installed {
		log.Info("ClusterClaim CRD not found; Skip creating ClusterClaim")
		return nil
	}
	cc := &clusterv1.ClusterClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: ClusterClaimPrefix + stage,
		},
		Spec: clusterv1.ClusterClaimSpec{
			Value: status,
		},
	}
	err := client.Create(ctx, cc)
	if apierrors.IsAlreadyExists(err) {
		log.Info("ClusterClaim already exists")
	} else if err != nil {
		return fmt.Errorf("could not create ClusterClaim: %w", err)
	}
	return nil
}
