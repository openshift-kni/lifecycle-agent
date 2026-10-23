/*
Copyright 2026.

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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltExecutableInitializesReexec builds the real lca-cli binary and invokes it,
// exercising the shipped entrypoint (main) rather than the package test harness. It is
// the regression guard for the self-contained build panic: containers/storage re-executes
// this binary for its graph-driver operations, so main must call reexec.Init() before the
// CLI runs. A normal invocation (reexec.Init() returns false) must start cleanly and not
// panic; a missing reexec.Init() breaks the storage layer the create-usb self-contained
// path depends on.
//
// Building the binary is relatively expensive, so the test is skipped under -short.
func TestBuiltExecutableInitializesReexec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping executable build/invocation test in -short mode")
	}

	bin := filepath.Join(t.TempDir(), "lca-cli")
	// Build with the same tags the project uses so the containers/storage graph driver
	// (which registers the reexec handlers) is compiled in exactly as it ships.
	build := exec.Command("go", "build",
		"-tags", "containers_image_openpgp exclude_graphdriver_btrfs",
		"-o", bin, "main.go")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building lca-cli: %v\n%s", err, out)
	}

	// --version is a side-effect-free invocation: reexec.Init() returns false (argv[0] is
	// the normal program name), so the binary must proceed to the CLI, print the version,
	// and exit zero without panicking.
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("running built lca-cli --version: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "panic:") {
		t.Fatalf("built lca-cli panicked on startup:\n%s", out)
	}
	if !strings.Contains(string(out), "lca-cli") {
		t.Fatalf("built lca-cli --version output did not identify the tool:\n%s", out)
	}
}
