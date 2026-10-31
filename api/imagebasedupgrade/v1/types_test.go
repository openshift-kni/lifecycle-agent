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

import "testing"

// TestConditionTypesValues locks the exact string value of every ConditionTypes
// field. Downstream consumers (e.g. TALM) string-match these values in IBU
// status conditions, so an accidental change here would silently break them.
func TestConditionTypesValues(t *testing.T) {
	tests := []struct {
		name string
		got  ConditionType
		want string
	}{
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Errorf("ConditionTypes.%s = %q, want %q", tt.name, string(tt.got), tt.want)
			}
		})
	}
}
