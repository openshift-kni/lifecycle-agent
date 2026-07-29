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

package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestString(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		gitCommit string
		buildTime string
		expected  string
	}{
		{
			name:      "default values",
			version:   "unknown",
			gitCommit: "unknown",
			buildTime: "unknown",
			expected:  "unknown (commit: unknown, built: unknown)",
		},
		{
			name:      "fully populated",
			version:   "4.19.0",
			gitCommit: "abc1234",
			buildTime: "2026-07-27T12:00:00Z",
			expected:  "4.19.0 (commit: abc1234, built: 2026-07-27T12:00:00Z)",
		},
		{
			name:      "partial values",
			version:   "4.19.0",
			gitCommit: "unknown",
			buildTime: "unknown",
			expected:  "4.19.0 (commit: unknown, built: unknown)",
		},
		{
			name:      "empty strings",
			version:   "",
			gitCommit: "",
			buildTime: "",
			expected:  " (commit: , built: )",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origVersion, origCommit, origBuild := Version, GitCommit, BuildTime
			t.Cleanup(func() {
				Version, GitCommit, BuildTime = origVersion, origCommit, origBuild
			})

			Version = tc.version
			GitCommit = tc.gitCommit
			BuildTime = tc.buildTime

			assert.Equal(t, tc.expected, String())
		})
	}
}
