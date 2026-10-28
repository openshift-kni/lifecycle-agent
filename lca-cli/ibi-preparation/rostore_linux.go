package ibi_preparation

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// defaultExchangeStoreDirs atomically swaps directories a and b in a single
// renameat2(RENAME_EXCHANGE) syscall: both paths exist throughout the call and afterwards point at
// each other's trees. It returns an error -- having swapped nothing -- when the kernel or target
// filesystem does not support RENAME_EXCHANGE, so the caller can fail closed rather than fall back to
// a non-atomic move.
//
// RENAME_EXCHANGE is a Linux-only syscall, so this implementation is built only on Linux (where the
// IBI prepare flow actually runs); see rostore_unsupported.go for the non-Linux stub.
func defaultExchangeStoreDirs(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("renameat2(RENAME_EXCHANGE) %q <-> %q: %w", a, b, err)
	}
	return nil
}
