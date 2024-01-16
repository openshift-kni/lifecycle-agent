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
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"

	"os"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;delete

// PHandler handles the precaching job
type PHandler struct {
	client.Client
	Log logr.Logger
}

// Config defines the configuration options for a pre-caching job.
type Config struct {
	ImageList          []string
	NumConcurrentPulls int

	// To run pre-caching job with an adjusted niceness, which affects process scheduling.
	// Niceness values range from -20 (most favorable to the process) to 19 (least favorable to the process).
	NicePriority int

	// To configure the I/O-scheduling class and priority of a process.
	IoNiceClass    int // 0: none, 1: realtime, 2: best-effort, 3: idle
	IoNicePriority int // priority (0..7) in the specified scheduling class, only for the realtime and best-effort classes

	// Allow for environment variables to be passed in
	EnvVars []corev1.EnvVar
}

// NewConfig creates a new Config instance with the provided imageList and optional configuration parameters.
// It initializes the Config with default values and updates specific fields using key-value pairs in args.
// Supported configuration options in args:
//   - "NumConcurrentPulls" (int): Number of concurrent pulls for pre-caching.
//   - "NicePriority" (int): Nice priority for pre-caching.
//   - "IoNiceClass" (int): I/O nice class for pre-caching.
//   - "IoNicePriority" (int): I/O nice priority for pre-caching.
//
// Example usage:
//
//	config := NewConfig(imageList, "NumConcurrentPulls", 10, "NicePriority", 5)
func NewConfig(imageList []string, envVars []corev1.EnvVar, args ...any) *Config {
	instance := &Config{
		ImageList:          imageList,
		NumConcurrentPulls: DefaultMaxConcurrentPulls,
		NicePriority:       DefaultNicePriority,
		IoNiceClass:        DefaultIoNiceClass,
		IoNicePriority:     DefaultIoNicePriority,
		EnvVars:            envVars,
	}

	for i := 0; i < len(args); i += 2 {
		fieldName, value := args[i].(string), args[i+1]

		switch fieldName {
		case "NumConcurrentPulls":
			if NumConcurrentPulls, ok := value.(int); ok {
				instance.NumConcurrentPulls = NumConcurrentPulls
			}
		case "NicePriority":
			if NicePriority, ok := value.(int); ok {
				instance.NicePriority = NicePriority
			}
		case "IoNiceClass":
			if IoNiceClass, ok := value.(int); ok {
				instance.IoNiceClass = IoNiceClass
			}
		case "IoNicePriority":
			if IoNicePriority, ok := value.(int); ok {
				instance.IoNicePriority = IoNicePriority
			}
		}
	}

	return instance
}

// Status represents the status and progress information for the precaching job
type Status struct {
	Status   string
	Message  string
	Progress Progress
}

// CreateJob creates a new precache job.
func (h *PHandler) CreateJob(ctx context.Context, config *Config) error {

	if err := validateJobConfig(ctx, h.Client, config.ImageList); err != nil {
		return err
	}

	// Generate ConfigMap for list of images to be pre-cached
	cm := renderConfigMap(config.ImageList)
	err := h.Client.Create(ctx, cm)
	if err != nil {
		return err
	}

	job, err := renderJob(config, h.Log)
	if err != nil {
		h.Log.Info("Failed to render precaching job manifest.")
		return err
	}
	err = h.Client.Create(ctx, job)
	if err != nil {
		h.Log.Info("Failed to create K8s job.")
		return err
	}

	// Log job details
	h.Log.Info("Precaching", "CreatedJob", job.Name)

	return nil
}

// QueryJobStatus retrieves the status of the precache job.
func (h *PHandler) QueryJobStatus(ctx context.Context) (*Status, error) {

	job, err := getJob(ctx, h.Client, LcaPrecacheJobName, common.LcaNamespace)
	if err != nil {
		h.Log.Info("Unable to get job for status", "jobName", LcaPrecacheJobName)
		return nil, err
	}

	if job == nil {
		h.Log.Info("Precaching job does not exist", "jobName", LcaPrecacheJobName)
		return nil, nil
	}

	status := &Status{Message: ""}
	// Extract job status: active, successful, failed
	if job.Status.Active > 0 {
		status.Status = Active
		h.Log.Info("Precaching job in-progress", "name:", LcaPrecacheJobName)
	} else if job.Status.Succeeded > 0 {
		status.Status = Succeeded
		h.Log.Info("Precaching job succeeded", "name:", LcaPrecacheJobName)
	} else if job.Status.Failed > 0 {
		status.Status = Failed
		h.Log.Info("Precaching job failed", "name:", LcaPrecacheJobName)
	}

	// Get precaching progress summary from StatusFile
	_, err = os.Stat(common.PathOutsideChroot(StatusFile))
	if err == nil {
		// in progress
		var data []byte
		data, err = os.ReadFile(common.PathOutsideChroot(StatusFile))
		if err == nil {
			strProgress := strings.TrimSpace(string(data))
			err = json.Unmarshal([]byte(strProgress), &status.Progress)
			if err != nil {
				h.Log.Error(err, "Failed to parse progress", "StatusFile", StatusFile)
			} else {
				status.Message = fmt.Sprintf("total: %d (pulled: %d, skipped: %d, failed: %d)",
					status.Progress.Total, status.Progress.Pulled, status.Progress.Skipped, status.Progress.Failed)
			}
		} else {
			h.Log.Info("Unable to read precaching progress file", "StatusFile", StatusFile)
		}
	}
	return status, nil
}

// Cleanup deletes the ConfigMap and Job precaching resources
func (h *PHandler) Cleanup(ctx context.Context) error {
	// Delete Job
	if err := deleteJob(ctx, h.Client, LcaPrecacheJobName, common.LcaNamespace); err != nil {
		h.Log.Info("Failed to delete precaching job", "name", LcaPrecacheJobName)
		return err
	}
	// Delete ConfigMap
	if err := deleteConfigMap(ctx, h.Client, LcaPrecacheConfigMapName, common.LcaNamespace); err != nil {
		h.Log.Info("Failed to delete precaching configmap", "name", LcaPrecacheConfigMapName)
		return err
	}

	// Delete precaching progress tracker file
	statusFile := common.PathOutsideChroot(StatusFile)
	if _, err := os.Stat(statusFile); err == nil {
		// Progress tracker file exists, attempt to delete it
		if err := os.Remove(statusFile); err != nil {
			h.Log.Error(err, "Failed to delete precaching progress tracker", "file", StatusFile)
		}
	}

	return nil
}
