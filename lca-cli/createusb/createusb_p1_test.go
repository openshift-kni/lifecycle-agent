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

package createusb

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateP1Partition(t *testing.T) {
	plan := partitionPlan(ModeInstallOnly, defaultP1ReservationBytes)
	fp := &fakePartitionOps{}
	err := createP1Partition(testLogger(), plan, "/dev/loop1", mediaBuildDeps{partition: fp})
	require.NoError(t, err)
	assert.Equal(t, []string{"create"}, fp.calls)
	assert.Equal(t, plan, fp.lastPlan)
	assert.Empty(t, fp.createdParts, "p1's GPT placeholder has no filesystem")
}

func TestCreateP1PartitionRejectsMissingOrFailedLayout(t *testing.T) {
	fp := &fakePartitionOps{}
	err := createP1Partition(testLogger(), PartitionPlan{}, "/dev/loop1", mediaBuildDeps{partition: fp})
	require.ErrorContains(t, err, "p1 (live-iso) not found")
	assert.Empty(t, fp.calls)

	fp.createErr = fmt.Errorf("sgdisk boom")
	err = createP1Partition(testLogger(), partitionPlan(ModeInstallOnly, defaultP1ReservationBytes),
		"/dev/loop1", mediaBuildDeps{partition: fp})
	require.ErrorContains(t, err, "sgdisk boom")
}
