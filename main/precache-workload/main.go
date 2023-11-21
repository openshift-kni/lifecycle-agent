/*
 * Copyright 2023 Red Hat, Inc.
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

package main

import (
	"os"
	"strings"
	"syscall"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/precache/workload"
	log "github.com/sirupsen/logrus"
)

// readPrecacheSpecFile returns the list of images to be precached as specified in the precache spec file
func readPrecacheSpecFile() (precacheSpec []string, err error) {
	precacheSpecFile := os.Getenv(precache.EnvPrecacheSpecFile)
	if precacheSpecFile == "" {
		log.Errorf("Environment variable %s is not set. Exiting...", precache.EnvPrecacheSpecFile)
		os.Exit(1)
	}

	// Check if precacheSpecFile exists
	if _, err := os.Stat(precacheSpecFile); os.IsNotExist(err) {
		log.Errorf("Missing precache spec file.")
		return precacheSpec, err
	}
	log.Info("Precache spec file found.")

	var content []byte
	content, err = os.ReadFile(precacheSpecFile)
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")

	// Filter out empty lines
	for _, line := range lines {
		if line != "" {
			precacheSpec = append(precacheSpec, line)
		}
	}

	return precacheSpec, nil
}

func main() {

	log.Info("Starting to execute pre-cache workload")

	// Load precache spec file which is outside /host filesystem
	precacheSpec, err := readPrecacheSpecFile()
	if err != nil {
		os.Exit(1)
	}
	log.Info("Loaded precache spec file.")

	// Change root directory to /host
	if err := syscall.Chroot(utils.Host); err != nil {
		log.Errorf("Failed to chroot to %s, err: %s", utils.Host, err)
		os.Exit(1)
	}
	log.Infof("chroot %s successful", utils.Host)

	// Pre-check: Verify podman is running
	if !workload.CheckPodman() {
		log.Errorf("Failed to execute podman command, terminating pre-caching job!")
		os.Exit(1)
	}
	log.Info("podman is running, proceeding to pre-cache images!")

	// Pre-cache images
	status, err := workload.PullImages(precacheSpec)
	if err != nil {
		log.Errorf("Encountered error while pre-caching images, error: %v", err)
		os.Exit(1)
	}
	log.Info("Completed executing pre-caching, no errors encountered!")

	// Check pre-caching execution status
	if status.Failed != 0 {
		log.Info("Failed to pre-cache the following images:")
		for _, image := range status.FailedPullList {
			log.Info(image)
		}
		log.Info("Flagging pre-caching job as failed.")
		os.Exit(1)
	}

	log.Info("Pre-cached images successfully.")
}
