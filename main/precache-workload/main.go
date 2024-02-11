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
	"fmt"
	"os"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/precache/workload"
)

// Exit codes
const (
	Failure int = 1
)

// terminateOnError Logs a "terminating job" + error message and terminates the pre-caching job with the given exit code
func terminateOnError(err error) {
	log.Errorf("terminating pre-caching job due to error: %v", err)
	os.Exit(Failure)
}

// readPrecacheSpecFile returns the list of images to be precached as specified in the precache spec file
func readPrecacheSpecFile() (precacheSpec []string, err error) {
	precacheSpecFile := os.Getenv(precache.EnvPrecacheSpecFile)
	if precacheSpecFile == "" {
		return precacheSpec, fmt.Errorf("environment variable %s is not set", precache.EnvPrecacheSpecFile)
	}

	// Check if precacheSpecFile exists
	if _, err := os.Stat(precacheSpecFile); os.IsNotExist(err) {
		return precacheSpec, fmt.Errorf("missing precache spec file")
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

	bestEffort := false
	str := os.Getenv(precache.EnvPrecacheBestEffort)
	if str == "TRUE" {
		bestEffort = true
		log.Info("pre-caching set to 'best-effort'")
	}

	// Load precache spec file which is outside /host filesystem
	precacheSpec, err := readPrecacheSpecFile()
	if err != nil {
		terminateOnError(err)
	}

	log.Info("Loaded precache spec file.")

	// Change root directory to /host
	if err := syscall.Chroot(common.Host); err != nil {
		terminateOnError(fmt.Errorf("failed to chroot to %s, err: %w", common.Host, err))
	}
	log.Infof("chroot %s successful", common.Host)

	// Pre-check: Verify podman is running
	if !workload.CheckPodman() {
		terminateOnError(fmt.Errorf("failed to execute podman command"))
	}
	log.Info("podman is running, proceeding to pre-cache images!")
	// Get auth file for Podman
	authFile, err := workload.GetAuthFile()
	if err != nil {
		terminateOnError(err)
	}
	if err := workload.Precache(precacheSpec, authFile, bestEffort); err != nil {
		terminateOnError(err)
	}
}
