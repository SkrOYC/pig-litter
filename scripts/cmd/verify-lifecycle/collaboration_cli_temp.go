package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type collaborationTempDirectory struct {
	path     string
	identity os.FileInfo
}

func collaborationNewTempDirectory() (collaborationTempDirectory, error) {
	parent, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return collaborationTempDirectory{}, err
	}
	path, err := os.MkdirTemp(parent, "collab-")
	if err != nil {
		return collaborationTempDirectory{}, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		_ = os.Remove(path)
		return collaborationTempDirectory{}, err
	}
	return collaborationTempDirectory{path: path, identity: identity}, nil
}

func (directory collaborationTempDirectory) release(joined bool) (string, error) {
	current, err := os.Lstat(directory.path)
	if err != nil {
		return directory.path, fmt.Errorf("inspect temporary directory: %w", err)
	}
	if !current.IsDir() || !os.SameFile(directory.identity, current) {
		return directory.path, errors.New("temporary directory identity changed")
	}
	entries, err := os.ReadDir(directory.path)
	if err != nil || !joined || len(entries) != 0 {
		return directory.path, err
	}
	if err := os.Remove(directory.path); err != nil {
		return directory.path, err
	}
	return "", nil
}
