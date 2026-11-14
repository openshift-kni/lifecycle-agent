package precache

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/openshift-kni/lifecycle-agent/utils"
)

// TestProgressConcurrentUpdateAndPersist mirrors the precache workload, where each pull goroutine
// calls Update and then Persist. Run with -race to detect unsynchronized access.
func TestProgressConcurrentUpdateAndPersist(t *testing.T) {
	const pulls = 50
	file := filepath.Join(t.TempDir(), "progress.json")
	progress := &Progress{Total: pulls}

	var wg sync.WaitGroup
	for i := range pulls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			progress.Update(i%5 != 0, fmt.Sprintf("quay.io/image:%d", i))
			progress.Persist(file)
		}()
	}
	wg.Wait()

	persisted := &Progress{}
	assert.NoError(t, utils.ReadYamlOrJSONFile(file, persisted))
	assert.Equal(t, pulls, persisted.Total)
	assert.Equal(t, 40, persisted.Pulled)
	assert.Equal(t, 10, persisted.Failed)
	assert.Len(t, persisted.FailedPullList, 10)
}

// TestProgressMethodsWaitForLock fails deterministically (without -race) if Persist or Log reads
// the progress without holding the lock.
func TestProgressMethodsWaitForLock(t *testing.T) {
	testCases := []struct {
		name       string
		call       func(p *Progress, file string)
		writesFile bool
	}{
		{name: "Persist", call: (*Progress).Persist, writesFile: true},
		{name: "Log", call: func(p *Progress, _ string) { p.Log() }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "progress.json")
			progress := &Progress{Total: 2, Pulled: 1, Failed: 1, FailedPullList: []string{"quay.io/image:1"}}

			progress.mux.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(progress, file)
			}()

			select {
			case <-done:
				t.Fatalf("%s returned while the progress lock was held", tc.name)
			case <-time.After(50 * time.Millisecond):
			}
			assert.NoFileExists(t, file)

			progress.mux.Unlock()
			<-done
			if tc.writesFile {
				assert.FileExists(t, file)
			}
		})
	}
}

func TestProgressPersistMissingDirWritesNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing", "progress.json")
	(&Progress{Total: 1}).Persist(file)
	assert.NoFileExists(t, file)
}
