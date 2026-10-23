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

package main

import (
	"os"

	"go.podman.io/storage/pkg/reexec"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/cmd"
)

func main() {
	// containers/storage re-executes this binary for privileged graph-driver
	// operations (create-usb's self-contained build reaches BuildDurabilityStore,
	// which drives containers/storage). reexec.Init must run before any other work:
	// when the process is such a re-exec it dispatches the registered handler and
	// returns true, and we must exit immediately rather than fall through to the CLI.
	// Omitting this makes the storage layer panic on first use.
	if reexec.Init() {
		return
	}

	// Exit non-zero when a command fails so automation never treats a failed
	// (or not-yet-implemented) build as success. cobra has already printed the
	// error to stderr by this point.
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
