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

package workload

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// MaxRetries is the max number of retries for pulling an image before marking it as failed
const MaxRetries int = 5

// Podman auth-file related constants
const (
	EnvAuthFile     string = "PULL_SECRET_PATH"
	DefaultAuthFile string = "/var/lib/kubelet/config.json"
)

var (
	logExec = &log.Logger{
		Level: log.ErrorLevel, // reducing log level and only report if the exec calls fail
	}
	Executor = ops.NewRegularExecutor(logExec, false)
)

// CheckPodman verifies that podman is running by checking the version of podman
func CheckPodman() bool {
	if _, err := Executor.ExecuteWithLiveLogger("podman", []string{"version"}...); err != nil {
		return false
	}
	return true
}

// podmanImgPull pulls the specified image via podman CLI
func podmanImgPull(image, authFile string) error {
	args := []string{"pull", image}
	if authFile != "" {
		args = append(args, []string{"--authfile", authFile}...)
	}
	if _, err := Executor.Execute("podman", args...); err != nil {
		return fmt.Errorf("failed podman pull with args %s: %w", args, err)
	}
	return nil
}

func podmanImgExists(image string) bool {
	args := []string{"image exists", image}
	_, err := Executor.Execute("podman", args...)
	if err != nil {
		log.Errorf("failed podman image check with args %s: %v", args, err)
	}
	return err == nil
}

// pullImage attempts to pull an image via podman CLI
func pullImage(image, authFile string, progress *precache.Progress) error {

	var err error
	for i := 0; i < MaxRetries; i++ {
		err = podmanImgPull(image, authFile)
		if err == nil {
			log.Infof("Successfully pulled image: %s", image)
			break
		} else {
			message := fmt.Sprintf("%v", err)
			log.Infof("Attempt %d/%d: Failed to pull %s: %s", i+1, MaxRetries, image, message)
			if strings.Contains(message, "manifest unknown") {
				break
			}
		}
	}
	// update precache progress tracker
	progress.Update(err == nil, image)

	// persist progress to file
	progress.Persist(precache.StatusFile)

	return err
}

// getAuthFile returns the auth file for podman
func GetAuthFile() (string, error) {
	// Configure Podman auth file
	authFile := os.Getenv(EnvAuthFile)
	if authFile == "" {
		authFile = DefaultAuthFile
	}

	// Check if authFile exists
	if _, err := os.Stat(authFile); os.IsNotExist(err) {
		return "", fmt.Errorf("failed to get authfile for podman: %w", err)
	}
	log.Info("Auth file for podman found.")

	return authFile, nil
}

// PullImages pulls a list of images using podman
func PullImages(precacheSpec []string, authFile string) *precache.Progress {

	// Initialize progress tracking
	progress := &precache.Progress{
		Total:  len(precacheSpec),
		Pulled: 0,
		Failed: 0,
	}

	log.Infof("Will attempt to pull %d images", len(precacheSpec))

	// Create wait group and pull images
	var wg sync.WaitGroup
	numThreads, err := strconv.Atoi(os.Getenv(precache.EnvMaxPullThreads))
	if err != nil {
		numThreads = precache.DefaultMaxConcurrentPulls
	}
	threads := make(chan struct{}, numThreads)
	log.Infof("Configured precaching job to concurrently pull %d images.", numThreads)

	// Start pulling images
	for _, image := range precacheSpec {
		threads <- struct{}{}
		wg.Add(1)
		go func(image string) {
			defer func() {
				<-threads
				wg.Done()
			}()
			err := pullImage(image, authFile, progress)

			if err != nil {
				log.Errorf("Failed to pull image: %s, error: %v", image, err)
			}
		}(image)
	}

	log.Info("Waiting for precaching threads to finish...")
	// Wait for all threads to complete
	wg.Wait()
	log.Info("All the precaching threads have finished.")

	// Log final progress
	progress.Log()

	// Store final precache progress report to file
	progress.Persist(precache.StatusFile)

	return progress
}

func ValidatePrecache(status *precache.Progress, bestEffort bool) error {
	// Check pre-caching execution status
	if status.Failed != 0 {
		var imagesFound []string
		log.Info("Failed to pre-cache the following images:")
		for _, image := range status.FailedPullList {
			if strings.Contains(image, "@sha:") && podmanImgExists(image) {
				log.Infof("%s, but found locally after downloading other images", image)
				imagesFound = append(imagesFound, image)
			} else {
				log.Info(image)
			}
		}
		if len(imagesFound) == status.Failed {
			return nil
		}
		if bestEffort {
			log.Info("Failed to precache, running in best-effort mode, skip error")
			return nil
		}
		return fmt.Errorf("failed to pre-cache one or more images")
	}
	return nil
}

func Precache(precacheSpec []string, authFile string, bestEffort bool) error {
	// Pre-cache images
	status := PullImages(precacheSpec, authFile)
	log.Info("Completed executing pre-caching")

	if err := ValidatePrecache(status, bestEffort); err != nil {
		return fmt.Errorf("failed to pre-cache one or more images")
	}

	log.Info("Pre-cached images successfully.")
	return nil
}
