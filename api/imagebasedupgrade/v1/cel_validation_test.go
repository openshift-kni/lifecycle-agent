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

package v1

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

const (
	ibuCRDPath                = "../../../config/crd/bases/lca.openshift.io_imagebasedupgrades.yaml"
	mutualExclusionMsg        = "oadpContent and backupContent are mutually exclusive"
	backupContentImmutableMsg = "can not change spec.backupContent while ibu is in progress"
)

// loadRootStructural reads a generated CRD manifest, converts its stored-version
// openAPIV3Schema into an internal structural schema, and returns it. This is the
// same schema the API server builds its CEL validator from, so evaluating rules
// against it is the "equivalent" of an envtest round-trip that the story's test
// plan calls for.
func loadRootStructural(t *testing.T, crdPath string) *structuralschema.Structural {
	t.Helper()

	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading CRD %s: %v", crdPath, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshalling CRD %s: %v", crdPath, err)
	}

	var v1Schema *apiextensionsv1.JSONSchemaProps
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			v1Schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}
	if v1Schema == nil {
		t.Fatalf("CRD %s has no stored version with a schema", crdPath)
	}

	internalSchema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1Schema, internalSchema, nil); err != nil {
		t.Fatalf("converting schema for %s: %v", crdPath, err)
	}

	structural, err := structuralschema.NewStructural(internalSchema)
	if err != nil {
		t.Fatalf("building structural schema for %s: %v", crdPath, err)
	}
	return structural
}

// validateCEL runs the CRD's x-kubernetes-validations (CEL) rules against obj
// (and oldObj for transition rules), mirroring what the API server does at
// admission. A nil oldObj models a create; a non-nil oldObj models an update.
func validateCEL(root *structuralschema.Structural, obj, oldObj map[string]interface{}) field.ErrorList {
	validator := celschema.NewValidator(root, true, celconfig.PerCallLimit)
	errs, _ := validator.Validate(context.Background(), field.NewPath("root"), root, obj, oldObj, celconfig.RuntimeCELCostBudget)
	return errs
}

// hasMessage reports whether any error in errs contains substr.
func hasMessage(errs field.ErrorList, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}

// ibu builds a minimal valid IBU object map with metadata.name=upgrade (required
// by the singleton rule) and the given stage/spec overlay applied under spec.
func ibu(stage string, specOverlay map[string]interface{}) map[string]interface{} {
	spec := map[string]interface{}{"stage": stage}
	for k, v := range specOverlay {
		spec[k] = v
	}
	return map[string]interface{}{
		"apiVersion": "lca.openshift.io/v1",
		"kind":       "ImageBasedUpgrade",
		"metadata":   map[string]interface{}{"name": "upgrade"},
		"spec":       spec,
	}
}

func oadpRef(name string) []interface{} {
	return []interface{}{map[string]interface{}{"name": name, "namespace": "openshift-adp"}}
}

func backupRef(name string) []interface{} {
	return []interface{}{map[string]interface{}{"name": name}}
}

// TestMutualExclusionRejected verifies the spec-level CEL rule: an IBU that sets
// both spec.oadpContent and spec.backupContent (both non-empty) is rejected, while
// setting exactly one, or neither (the Idle IBU InitIBU creates), is accepted.
func TestMutualExclusionRejected(t *testing.T) {
	root := loadRootStructural(t, ibuCRDPath)

	tests := []struct {
		name       string
		spec       map[string]interface{}
		wantReject bool
	}{
		{
			name:       "both set is rejected",
			spec:       map[string]interface{}{"oadpContent": oadpRef("cm"), "backupContent": backupRef("bc")},
			wantReject: true,
		},
		{
			name:       "only oadpContent is accepted",
			spec:       map[string]interface{}{"oadpContent": oadpRef("cm")},
			wantReject: false,
		},
		{
			name:       "only backupContent is accepted",
			spec:       map[string]interface{}{"backupContent": backupRef("bc")},
			wantReject: false,
		},
		{
			name:       "neither set is accepted (Idle IBU from InitIBU)",
			spec:       map[string]interface{}{},
			wantReject: false,
		},
		{
			name:       "both present but empty lists is accepted",
			spec:       map[string]interface{}{"oadpContent": []interface{}{}, "backupContent": []interface{}{}},
			wantReject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateCEL(root, ibu("Idle", tt.spec), nil)
			got := hasMessage(errs, mutualExclusionMsg)
			if got != tt.wantReject {
				t.Errorf("mutual-exclusion rejected=%v, want %v (errs: %v)", got, tt.wantReject, errs)
			}
		})
	}
}

// TestBackupContentImmutableWhileInProgress verifies the root-object CEL rule:
// changing spec.backupContent while the IBU is in progress (not Idle) is rejected,
// but changing it while Idle is accepted.
func TestBackupContentImmutableWhileInProgress(t *testing.T) {
	root := loadRootStructural(t, ibuCRDPath)

	inProgressStatus := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "Idle", "status": "False"},
		},
	}
	idleStatus := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "Idle", "status": "True"},
		},
	}

	withStatus := func(obj, status map[string]interface{}) map[string]interface{} {
		obj["status"] = status
		return obj
	}

	t.Run("change while in progress is rejected", func(t *testing.T) {
		oldObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("old")}), inProgressStatus)
		newObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("new")}), inProgressStatus)
		errs := validateCEL(root, newObj, oldObj)
		if !hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("expected rejection changing backupContent while in progress, got errs: %v", errs)
		}
	})

	t.Run("change while Idle is accepted", func(t *testing.T) {
		oldObj := withStatus(ibu("Idle", map[string]interface{}{"backupContent": backupRef("old")}), idleStatus)
		newObj := withStatus(ibu("Idle", map[string]interface{}{"backupContent": backupRef("new")}), idleStatus)
		errs := validateCEL(root, newObj, oldObj)
		if hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("did not expect rejection changing backupContent while Idle, got errs: %v", errs)
		}
	})

	// Isolates the oldSelf.status Idle=True clause of the immutability rule. The
	// new stage is Prep (not Idle), so the self.spec.stage=='Idle' clause cannot
	// satisfy the rule on its own; only the old-status-Idle clause can permit the
	// backupContent change. The previous "change while Idle" case leaves both
	// clauses true, so it never exercises this one in isolation.
	t.Run("change with old status Idle and new stage Prep is accepted", func(t *testing.T) {
		oldObj := withStatus(ibu("Idle", map[string]interface{}{"backupContent": backupRef("old")}), idleStatus)
		newObj := withStatus(ibu("Prep", map[string]interface{}{"backupContent": backupRef("new")}), idleStatus)
		errs := validateCEL(root, newObj, oldObj)
		if hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("did not expect rejection changing backupContent when old status is Idle, got errs: %v", errs)
		}
	})

	t.Run("unchanged while in progress is accepted", func(t *testing.T) {
		oldObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("same")}), inProgressStatus)
		newObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("same")}), inProgressStatus)
		errs := validateCEL(root, newObj, oldObj)
		if hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("did not expect rejection leaving backupContent unchanged, got errs: %v", errs)
		}
	})

	t.Run("adding backupContent while in progress is rejected", func(t *testing.T) {
		oldObj := withStatus(ibu("Upgrade", map[string]interface{}{}), inProgressStatus)
		newObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("new")}), inProgressStatus)
		errs := validateCEL(root, newObj, oldObj)
		if !hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("expected rejection adding backupContent while in progress, got errs: %v", errs)
		}
	})

	t.Run("removing backupContent while in progress is rejected", func(t *testing.T) {
		oldObj := withStatus(ibu("Upgrade", map[string]interface{}{"backupContent": backupRef("old")}), inProgressStatus)
		newObj := withStatus(ibu("Upgrade", map[string]interface{}{}), inProgressStatus)
		errs := validateCEL(root, newObj, oldObj)
		if !hasMessage(errs, backupContentImmutableMsg) {
			t.Errorf("expected rejection removing backupContent while in progress, got errs: %v", errs)
		}
	})
}
