/*
 * Copyright 2026 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package workload

import (
	"testing"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"
)

func TestMaxPullThreads(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{
			name:  "unset",
			value: "",
			want:  precache.DefaultMaxConcurrentPulls,
		},
		{
			name:  "non numeric",
			value: "not-a-number",
			want:  precache.DefaultMaxConcurrentPulls,
		},
		{
			name:  "zero",
			value: "0",
			want:  precache.DefaultMaxConcurrentPulls,
		},
		{
			name:  "negative",
			value: "-1",
			want:  precache.DefaultMaxConcurrentPulls,
		},
		{
			name:  "positive",
			value: "4",
			want:  4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maxPullThreads(tt.value); got != tt.want {
				t.Fatalf("maxPullThreads(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}
