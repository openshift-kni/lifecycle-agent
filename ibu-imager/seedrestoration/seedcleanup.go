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

package seedrestoration

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/sirupsen/logrus"
)

var foldersToRemove = []string{
	common.BackupDir,
	common.BackupChecksDir,
	common.OvnNodeCerts,
	common.MultusCerts,
	common.SeedDataDir,
}

// SeedRestoration handles cleanup operations after creating a seed image, removing temporary files
// and executing additional cleanup steps as needed.
type SeedRestoration struct {
	log                  *logrus.Logger
	ops                  ops.Ops
	backupDir            string
	containerRegistry    string
	authFile             string
	recertContainerImage string
	recertSkipValidation bool
}

func NewSeedRestoration(log *logrus.Logger, ops ops.Ops, backupDir,
	containerRegistry, authFile, recertContainerImage string, recertSkipValidation bool) *SeedRestoration {

	return &SeedRestoration{
		log:                  log,
		ops:                  ops,
		backupDir:            backupDir,
		containerRegistry:    containerRegistry,
		authFile:             authFile,
		recertContainerImage: recertContainerImage,
		recertSkipValidation: recertSkipValidation,
	}
}

// CleanupSeedCluster comprises the Imager workflow for cleanup operations after creating a seed image
// out of an SNO cluster.
func (s *SeedRestoration) CleanupSeedCluster() error {
	s.log.Info("Cleaning up seed cluster")

	// Collect all cleanup errors to only fail fatally at the end,
	// but still cleanup as much as possible.
	var errors []error

	if _, err := s.ops.RunInHostNamespace("podman", []string{"rmi", s.containerRegistry}...); err != nil {
		s.log.Errorf("failed to remove seed image: %v", err)
		errors = append(errors, err)
	}

	if err := s.cleanupServiceUnits(); err != nil {
		s.log.Errorf("Error cleaning up systemd service files: %v", err)
		errors = append(errors, err)
	}

	if err := s.cleanupScriptFiles(); err != nil {
		s.log.Errorf("Error cleaning up script files: %v", err)
		errors = append(errors, err)
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping restoring crypto via recert tool")
	} else {
		if err := s.ops.RestoreOriginalSeedCrypto(s.recertContainerImage, s.authFile); err != nil {
			s.log.Errorf("Error restoring certificates: %v", err)
			errors = append(errors, err)
		}
	}

	for _, folder := range foldersToRemove {
		s.log.Infof("Removing %s folder", folder)
		if err := os.RemoveAll(folder); err != nil {
			s.log.Errorf("Error removing %s: %v", folder, err)
			errors = append(errors, err)
		}
	}

	s.log.Info("Restoring cluster services (i.e. kubelet.service unit)")
	if _, err := s.ops.SystemctlAction("enable", "kubelet.service", "--now"); err != nil {
		s.log.Errorf("Error enabling kubelet.service unit: %v", err)
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("encountered %d errors during cleanup", len(errors))
	}

	return nil
}

func (s *SeedRestoration) cleanupServiceUnits() error {
	dir := filepath.Join(common.InstallationConfigurationFilesDir, "services")
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		serviceName := info.Name()

		s.log.Infof("Disabling service unit %s", serviceName)
		if _, err := s.ops.SystemctlAction("disable", serviceName, "--now"); err != nil {
			s.log.Errorf("Error disabling %s unit: %v", serviceName, err)
		}

		s.log.Infof("Removing service unit %s", serviceName)
		if err := os.Remove(filepath.Join("/etc/systemd/system/", serviceName)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("error removing %s file: %w", serviceName, err)
		}

		return nil
	})

	return err
}

func (s *SeedRestoration) cleanupScriptFiles() error {
	dir := filepath.Join(common.InstallationConfigurationFilesDir, "scripts")
	return utils.HandleFilesWithCallback(dir, func(path string) error {
		scriptName := filepath.Base(path)

		s.log.Infof("Removing script file %s", scriptName)
		if err := os.Remove(filepath.Join("/var/usrlocal/bin/", scriptName)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("error removing %s file: %w", scriptName, err)
		}

		return nil
	})
}
