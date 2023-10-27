package utils

import (
	"os"
)

// FileSystem interface
type FileSystem interface {
	Stat(string) (os.FileInfo, error)
	Mkdir(string, os.FileMode) error
	WriteFile(string, []byte, os.FileMode) error
	ReadFile(string) ([]byte, error)
}

// OsFs interacts with the OS
type OsFs struct{}

// ReadFile calls os.Readfile
func (OsFs) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// Mkdir calls os.Mkdir
func (OsFs) Mkdir(name string, perm os.FileMode) error {
	return os.Mkdir(name, perm)
}

// Stat calls os.Stat
func (OsFs) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

// WriteFile calls os.WriteFile
func (OsFs) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// FakeFS fakes filesystem operations
type FakeFS struct {
	ReadContent string
	FilesExist  map[string]bool
}

// Stat mocks os.Stat
func (f FakeFS) Stat(name string) (os.FileInfo, error) {
	if f.FilesExist[name] {
		return nil, nil
	}
	return nil, os.ErrNotExist
}

// Mkdir mocks os.mkdir
func (FakeFS) Mkdir(name string, perm os.FileMode) error {
	return nil
}

// WriteFile mocks os.WriteFile
func (FakeFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return nil
}

// ReadFile mocks os.ReadFile
func (f FakeFS) ReadFile(name string) ([]byte, error) {
	return []byte(f.ReadContent), nil
}
