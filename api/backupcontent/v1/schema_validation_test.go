/*
Copyright 2026.

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

package v1

import (
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

const backupContentCRDPath = "../../../config/crd/bases/lca.openshift.io_backupcontents.yaml"

// loadSchemaValidator reads the generated BackupContent CRD manifest and builds
// the same OpenAPI structural-schema validator the API server applies at
// admission. Running custom resources through it is the "equivalent" of an
// envtest round-trip for the CRD schema constraints.
func loadSchemaValidator(t *testing.T) apiservervalidation.SchemaValidator {
	t.Helper()

	data, err := os.ReadFile(backupContentCRDPath)
	if err != nil {
		t.Fatalf("reading CRD %s: %v", backupContentCRDPath, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshalling CRD %s: %v", backupContentCRDPath, err)
	}

	var v1Schema *apiextensionsv1.JSONSchemaProps
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			v1Schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}
	if v1Schema == nil {
		t.Fatalf("CRD %s has no stored version with a schema", backupContentCRDPath)
	}

	internalSchema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1Schema, internalSchema, nil); err != nil {
		t.Fatalf("converting schema for %s: %v", backupContentCRDPath, err)
	}

	validator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("building schema validator for %s: %v", backupContentCRDPath, err)
	}
	return validator
}

// backupContent builds a BackupContent object map with the given spec overlay.
func backupContent(spec map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "lca.openshift.io/v1",
		"kind":       "BackupContent",
		"metadata":   map[string]interface{}{"name": "sample", "namespace": "openshift-lifecycle-agent"},
		"spec":       spec,
	}
}

// TestResourceSelectorKindRequired verifies the CRD schema constraints: every
// resource-selector entry must carry a kind, while names remains optional (a
// name-less entry is accepted).
func TestResourceSelectorKindRequired(t *testing.T) {
	validator := loadSchemaValidator(t)

	tests := []struct {
		name       string
		spec       map[string]interface{}
		wantReject bool
	}{
		{
			name: "entry without kind is rejected",
			spec: map[string]interface{}{
				"namespacedResources": []interface{}{
					map[string]interface{}{"names": []interface{}{"klusterlet"}},
				},
			},
			wantReject: true,
		},
		{
			name: "entry with empty kind is rejected",
			spec: map[string]interface{}{
				"namespacedResources": []interface{}{
					map[string]interface{}{"kind": ""},
				},
			},
			wantReject: true,
		},
		{
			name: "name-less entry with kind is accepted",
			spec: map[string]interface{}{
				"namespacedResources": []interface{}{
					map[string]interface{}{"kind": "configmaps"},
				},
			},
			wantReject: false,
		},
		{
			name: "entry with kind and names is accepted",
			spec: map[string]interface{}{
				"clusterScopedResources": []interface{}{
					map[string]interface{}{
						"kind":  "clusterroles.rbac.authorization.k8s.io",
						"names": []interface{}{"klusterlet"},
					},
				},
			},
			wantReject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), backupContent(tt.spec), validator)
			if gotReject := len(errs) > 0; gotReject != tt.wantReject {
				t.Errorf("rejected=%v, want %v (errs: %v)", gotReject, tt.wantReject, errs)
			}
		})
	}
}

// TestWaveMinimum verifies the CRD schema constraint that backup and restore
// waves cannot be negative (Minimum=0), while 0 and positive values are accepted.
func TestWaveMinimum(t *testing.T) {
	validator := loadSchemaValidator(t)

	tests := []struct {
		name       string
		spec       map[string]interface{}
		wantReject bool
	}{
		{"negative backupWave is rejected", map[string]interface{}{"backupWave": -1}, true},
		{"negative restoreWave is rejected", map[string]interface{}{"restoreWave": -1}, true},
		{"zero backupWave is accepted", map[string]interface{}{"backupWave": 0}, false},
		{"positive waves are accepted", map[string]interface{}{"backupWave": 2, "restoreWave": 3}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), backupContent(tt.spec), validator)
			if gotReject := len(errs) > 0; gotReject != tt.wantReject {
				t.Errorf("rejected=%v, want %v (errs: %v)", gotReject, tt.wantReject, errs)
			}
		})
	}
}
