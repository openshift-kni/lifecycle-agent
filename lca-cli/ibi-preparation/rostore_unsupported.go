//go:build !linux

package ibi_preparation

import (
	"fmt"
	"runtime"
)

// defaultExchangeStoreDirs is the non-Linux stub for the atomic store swap. renameat2(RENAME_EXCHANGE)
// is a Linux-only syscall and the IBI prepare flow only ever runs on Linux (RHCOS); this
// implementation exists solely so the package builds on other platforms (e.g. macOS developer
// machines). It swaps nothing and fails closed, matching the Linux helper's contract that a caller
// which cannot perform the atomic exchange must not fall back to a non-atomic move.
func defaultExchangeStoreDirs(a, b string) error {
	return fmt.Errorf("renameat2(RENAME_EXCHANGE) %q <-> %q: not supported on %s", a, b, runtime.GOOS)
}
