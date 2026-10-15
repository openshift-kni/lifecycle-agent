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

// saveImgSeams snapshots the podman pull/exists seams and restores them when the
// test ends.
func saveImgSeams(t *testing.T) {
	t.Helper()
	origPull, origExists := imgPull, imgExists
	t.Cleanup(func() {
		imgPull = origPull
		imgExists = origExists
	})
}

// TestPullImageDisconnected proves the disconnected path never pulls: an image present
// locally succeeds and a missing one fails, and podman pull is never invoked either way.
func TestPullImageDisconnected(t *testing.T) {
	cases := []struct {
		name    string
		exists  bool
		wantErr bool
	}{
		{name: "present locally succeeds", exists: true, wantErr: false},
		{name: "missing locally fails", exists: false, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveImgSeams(t)
			imgExists = func(string) bool { return tc.exists }
			imgPull = func(string, string) error {
				t.Fatal("podman pull must not be called in disconnected mode")
				return nil
			}

			progress := &precache.Progress{Total: 1}
			err := pullImage("quay.io/img@sha256:abc", "", true, progress)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for a missing image in disconnected mode")
				}
				if progress.Failed != 1 || progress.Pulled != 0 {
					t.Fatalf("expected Failed=1 Pulled=0, got Failed=%d Pulled=%d", progress.Failed, progress.Pulled)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if progress.Pulled != 1 || progress.Failed != 0 {
					t.Fatalf("expected Pulled=1 Failed=0, got Pulled=%d Failed=%d", progress.Pulled, progress.Failed)
				}
			}
		})
	}
}

// TestPullImageConnectedStillPulls proves the connected path is unchanged: it pulls via
// podman and does not consult local presence as a substitute for pulling.
func TestPullImageConnectedStillPulls(t *testing.T) {
	saveImgSeams(t)
	pulled := false
	imgPull = func(image, _ string) error {
		pulled = true
		return nil
	}
	imgExists = func(string) bool {
		t.Fatal("podman image exists must not gate the connected pull path")
		return false
	}

	progress := &precache.Progress{Total: 1}
	if err := pullImage("quay.io/img@sha256:abc", "", false, progress); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pulled {
		t.Fatal("expected podman pull to be called in connected mode")
	}
	if progress.Pulled != 1 {
		t.Fatalf("expected Pulled=1, got %d", progress.Pulled)
	}
}

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
