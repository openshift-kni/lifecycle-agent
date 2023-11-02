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

// Constants for file and directory names
const (
	Path               string = "/var/ibu"
	Host               string = "/host"
	PrepGetSeedImage   string = "prepGetSeedImage.sh"
	PrepPullImages     string = "prepPullImages.sh"
	PrepSetupStateroot string = "prepSetupStateroot.sh"
	PrepCleanup        string = "prepCleanup.sh"
)

// CommandLine interface
type CommandLine interface {
	ExecuteCmd(string)
	ExecuteChrootCmd(string, string)
}

// OSCmd executes commands on OS
type OSCmd struct{}

// ExecuteCmd execute shell commands
func (OSCmd) ExecuteCmd(cmd string) {

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
func (OSCmd) ExecuteChrootCmd(root, cmd string) {

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

// FakeCmd fakes commands and record history
type FakeCmd struct {
	Commands       chan string
	ChrootCommands chan string
}

// ExecuteCmd keep track of the command instead of running it
func (f *FakeCmd) ExecuteCmd(cmd string) {
	f.Commands <- cmd
}

// ExecuteChrootCmd keep track of the command instead of running it
func (f *FakeCmd) ExecuteChrootCmd(root, cmd string) {
	f.ChrootCommands <- cmd
}
