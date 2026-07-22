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

package ops

import (
	"testing"
)

func TestParseSizeToBytes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{
			name:     "Parse gigabytes",
			input:    "40G",
			expected: 40 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "Parse gigabytes with B suffix",
			input:    "40GB",
			expected: 40 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "Parse megabytes",
			input:    "1024M",
			expected: 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "Parse kilobytes",
			input:    "2048K",
			expected: 2048 * 1024,
			wantErr:  false,
		},
		{
			name:     "Parse bytes",
			input:    "1024",
			expected: 1024,
			wantErr:  false,
		},
		{
			name:     "Parse decimal gigabytes",
			input:    "1.5G",
			expected: int64(1.5 * 1024 * 1024 * 1024),
			wantErr:  false,
		},
		{
			name:     "Parse with lowercase",
			input:    "40g",
			expected: 40 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "Invalid format",
			input:    "abc",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "Invalid number",
			input:    "abcG",
			expected: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseSizeToBytes(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSizeToBytes() expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("parseSizeToBytes() unexpected error: %v", err)
				return
			}
			if result != tt.expected {
				t.Errorf("parseSizeToBytes() = %d, expected %d", result, tt.expected)
			}
		})
	}
}
