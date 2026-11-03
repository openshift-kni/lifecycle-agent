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
	"reflect"
	"testing"
)

// conditionTypeCase pairs a ConditionTypes field with its expected string value.
type conditionTypeCase struct {
	name string
	got  ConditionType
	want string
}

// conditionTypesValueCases returns the expected string value for every
// ConditionTypes field. It is shared by the value-lock test and the
// completeness test so both stay in sync with a single source of truth.
func conditionTypesValueCases() []conditionTypeCase {
	return []conditionTypeCase{
		{"Idle", ConditionTypes.Idle, "Idle"},
		{"PrepInProgress", ConditionTypes.PrepInProgress, "PrepInProgress"},
		{"PrepCompleted", ConditionTypes.PrepCompleted, "PrepCompleted"},
		{"UpgradeInProgress", ConditionTypes.UpgradeInProgress, "UpgradeInProgress"},
		{"UpgradeCompleted", ConditionTypes.UpgradeCompleted, "UpgradeCompleted"},
		{"RollbackInProgress", ConditionTypes.RollbackInProgress, "RollbackInProgress"},
		{"RollbackCompleted", ConditionTypes.RollbackCompleted, "RollbackCompleted"},
		{"SeedGenInProgress", ConditionTypes.SeedGenInProgress, "SeedGenInProgress"},
		{"SeedGenCompleted", ConditionTypes.SeedGenCompleted, "SeedGenCompleted"},
		{"ConfigInProgress", ConditionTypes.ConfigInProgress, "ConfigInProgress"},
		{"ConfigCompleted", ConditionTypes.ConfigCompleted, "ConfigCompleted"},
	}
}

// TestConditionTypesValues locks the exact string value of every ConditionTypes
// field. Downstream consumers (e.g. TALM) string-match these values in IBU
// status conditions, so an accidental change here would silently break them.
func TestConditionTypesValues(t *testing.T) {
	for _, tt := range conditionTypesValueCases() {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Errorf("ConditionTypes.%s = %q, want %q", tt.name, string(tt.got), tt.want)
			}
		})
	}
}

// TestConditionTypesCompleteness guards against a new ConditionTypes field being
// added without a corresponding value-lock case. It reflects over the struct and
// fails if any exported field is missing from conditionTypesValueCases, so the
// value-lock coverage can never silently fall behind the struct definition.
func TestConditionTypesCompleteness(t *testing.T) {
	covered := make(map[string]bool)
	for _, tt := range conditionTypesValueCases() {
		covered[tt.name] = true
	}

	st := reflect.TypeOf(ConditionTypes)
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if !covered[name] {
			t.Errorf("ConditionTypes.%s has no value-lock case; add it to conditionTypesValueCases", name)
		}
	}
}
