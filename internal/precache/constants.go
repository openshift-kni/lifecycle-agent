/*
 * Copyright 2023 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
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

package precache

import (
	"fmt"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	corev1 "k8s.io/api/core/v1"
)

// LCA Resources
const (
	LcaPrecacheServiceAccount string = "lifecycle-agent-controller-manager"
	LcaPrecacheJobName        string = "lca-precache-job"
	LcaPrecacheConfigMapName  string = "lca-precache-cm"
	LcaPrecacheFinalizer             = "lca.precache.io/finalizer"
)

// Image paths
const (
	PrecachingSpecFilepath string = "/tmp/"
	PrecachingSpecFilename string = "images.txt"
)

// StatusFile is the filename for persisting the precaching progress tracker
const StatusFile = utils.IBUWorkspacePath + "/precache_status.json"

// Environment variable names
const (
	EnvLcaPrecacheImage   string = "PRECACHE_WORKLOAD_IMG"
	EnvPrecacheSpecFile   string = "PRECACHE_SPEC_FILE"
	EnvMaxPullThreads     string = "MAX_PULL_THREADS"
	EnvPrecacheBestEffort string = "PRECACHE_BEST_EFFORT"
)

// Precaching job specs
const (
	BackoffLimit    int32 = 0 // Allow 0 automatic retry for failed pre-caching job
	DefaultMode     int32 = 420
	RunAsUser       int64 = 0 // Run as root user
	Privileged      bool  = true
	HostDirPathType       = corev1.HostPathDirectory
)

// Resource Limits for Job
const (
	RequestResourceCPU    string = "10m"
	RequestResourceMemory string = "512Mi"
)

// Nice priority range
const (
	MinNicePriority int = -20
	MaxNicePriority int = 19
)

// IoNice class values
const (
	IoNiceClassNone       int = 0
	IoNiceClassRealTime   int = 1
	IoNiceClassBestEffort int = 2
	IoNiceClassIdle       int = 3
)

// IoNice priority range
const (
	MinIoNicePriority int = 0
	MaxIoNicePriority int = 7
)

// Default precaching config values
const (
	DefaultMaxConcurrentPulls int = 10
	DefaultNicePriority       int = 0
	DefaultIoNiceClass            = IoNiceClassBestEffort
	DefaultIoNicePriority     int = 4
)

// Precache status
const (
	Active    string = "Active"
	Failed    string = "Failed"
	Succeeded string = "Succeeded"
)

var ErrFailed = fmt.Errorf("precaching failed")
