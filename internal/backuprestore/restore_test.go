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

package backuprestore

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/stretchr/testify/assert"
)

// TestStartRestoreMissingBackup verifies that a missing backup directory is
// only tolerated when no backup content was configured; when content was
// configured but no backup is on disk the restore must fail hard.
func TestStartRestoreMissingBackup(t *testing.T) {
	tmpDir := t.TempDir()
	origHostPath := hostPath
	hostPath = tmpDir
	defer func() { hostPath = origHostPath }()

	h := &BRHandler{Log: logr.Discard()}

	t.Run("no content configured and no backup on disk skips restore", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), nil)
		assert.NoError(t, err)
		assert.NotNil(t, rt)
		assert.Empty(t, rt.FailedRestores)
	})

	t.Run("content configured but no backup on disk fails hard", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), []ibuv1.ConfigMapRef{{Name: "backup", Namespace: "ns"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.NotNil(t, rt)
	})
}
