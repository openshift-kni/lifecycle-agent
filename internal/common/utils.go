/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"

	"github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetConfigMap retrieves the configmap from cluster
func GetConfigMap(ctx context.Context, c client.Client, configMap v1alpha1.ConfigMapRef) (*corev1.ConfigMap, error) {

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      configMap.Name,
		Namespace: configMap.Namespace,
	}, cm); err != nil {
		return nil, err
	}

	return cm, nil
}

// GetConfigMaps retrieves a collection of configmaps from cluster
func GetConfigMaps(ctx context.Context, c client.Client, configMaps []v1alpha1.ConfigMapRef) ([]corev1.ConfigMap, error) {
	var cms []corev1.ConfigMap

	for _, cm := range configMaps {
		existingCm, err := GetConfigMap(ctx, c, cm)
		if err != nil {
			return nil, err
		}
		cms = append(cms, *existingCm)
	}

	return cms, nil
}

// PathOutsideChroot returns filepath with host fs
func PathOutsideChroot(filename string) string {
	return filepath.Join(utils.Host, filename)
}

func GetStaterootPath(osname string) string {
	return fmt.Sprintf("/ostree/deploy/%s", osname)
}
