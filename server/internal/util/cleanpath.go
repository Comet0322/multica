package util

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidPath reports a filesystem path that is empty or contains a NUL byte.
var ErrInvalidPath = errors.New("invalid filesystem path")

// CleanPath validates p and returns its lexically cleaned form: absolute paths
// stay anchored at their volume root, relative ones at ".". Empty paths and
// paths with embedded NUL bytes are rejected instead of being passed to the OS.
func CleanPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", ErrInvalidPath
	}
	anchor := "."
	if filepath.IsAbs(p) {
		anchor = filepath.VolumeName(p) + string(filepath.Separator)
	}
	rel, err := filepath.Rel(anchor, p)
	if err != nil {
		return "", err
	}
	return filepath.Join(anchor, rel), nil
}

// Stat is os.Stat on a validated path.
func Stat(path string) (os.FileInfo, error) {
	clean, err := CleanPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return os.Stat(clean)
}

// ReadFile is os.ReadFile on a validated path.
func ReadFile(path string) ([]byte, error) {
	clean, err := CleanPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.ReadFile(filepath.Clean(clean))
}

// WriteFile is os.WriteFile on a validated path.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	clean, err := CleanPath(path)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.WriteFile(clean, data, perm)
}

// MkdirAll is os.MkdirAll on a validated path.
func MkdirAll(path string, perm os.FileMode) error {
	clean, err := CleanPath(path)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return os.MkdirAll(clean, perm)
}
