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

package utils

import (
	"os/exec"
	"syscall"

	log "github.com/sirupsen/logrus"
)

const Path string = "/var/ibu"
const Host string = "/host"
const PrepGetSeedImage string = "prepGetSeedImage.sh"
const PrepPullImages string = "prepPullImages.sh"
const PrepSetupStateroot string = "prepSetupStateroot.sh"
const PrepCleanup string = "prepCleanup.sh"

// ExecuteCmd execute shell commands
func ExecuteCmd(cmd string) {

	logger := log.StandardLogger()
	lw := logger.Writer()

	log.Infof("Running: bash -c %s", cmd)
	execCmd := exec.Command("bash", "-c", cmd)

	execCmd.Stdout = lw
	execCmd.Stderr = lw

	err := execCmd.Run()

	lw.Close()

	if err != nil {
		log.Error(err)
	}
}

// ExecuteChrootCmd execute shell commands in a chroot environment
func ExecuteChrootCmd(root, cmd string) {

	logger := log.StandardLogger()
	lw := logger.Writer()

	log.Infof("Running chroot: bash -c %s", cmd)
	execCmd := exec.Command("/usr/bin/env", "--", "bash", "-c", cmd)

	execCmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	execCmd.Dir = "/"
	execCmd.Stdout = lw
	execCmd.Stderr = lw

	err := execCmd.Run()

	lw.Close()

	if err != nil {
		log.Error(err)
	}
}
