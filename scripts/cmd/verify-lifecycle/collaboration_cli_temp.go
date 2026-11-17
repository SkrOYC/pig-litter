package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type collaborationTempAlias struct {
	directory string
	path      string
	target    string
	identity  os.FileInfo
	linkID    os.FileInfo
	targetID  os.FileInfo
}

func collaborationNewTempAlias(target string) (collaborationTempAlias, error) {
	if !filepath.IsAbs(target) {
		return collaborationTempAlias{}, errors.New("temporary directory target must be absolute")
	}
	targetID, err := os.Lstat(target)
	if err != nil || !targetID.IsDir() || targetID.Mode()&os.ModeSymlink != 0 {
		return collaborationTempAlias{}, errors.New("temporary directory target must be an existing directory")
	}
	directory, err := os.MkdirTemp("/tmp", "p02-")
	if err != nil {
		return collaborationTempAlias{}, err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		_ = os.Remove(directory)
		return collaborationTempAlias{}, err
	}
	path := filepath.Join(directory, "tmp")
	if err := os.Symlink(target, path); err != nil {
		_ = os.Remove(directory)
		return collaborationTempAlias{}, err
	}
	linkID, err := os.Lstat(path)
	if err != nil {
		_ = os.Remove(path)
		_ = os.Remove(directory)
		return collaborationTempAlias{}, err
	}
	return collaborationTempAlias{directory: directory, path: path, target: target, identity: identity, linkID: linkID, targetID: targetID}, nil
}

func (alias collaborationTempAlias) release(joined bool) (string, error) {
	current, err := os.Lstat(alias.directory)
	if err != nil {
		return alias.path, fmt.Errorf("inspect temporary alias directory: %w", err)
	}
	if !current.IsDir() || !os.SameFile(alias.identity, current) {
		return alias.path, errors.New("temporary alias directory changed")
	}
	linkID, err := os.Lstat(alias.path)
	if err != nil {
		return alias.path, fmt.Errorf("inspect temporary alias identity: %w", err)
	}
	if linkID.Mode()&os.ModeSymlink == 0 || !os.SameFile(alias.linkID, linkID) {
		return alias.path, errors.New("temporary alias identity changed")
	}
	target, err := os.Readlink(alias.path)
	if err != nil {
		return alias.path, fmt.Errorf("inspect temporary alias: %w", err)
	}
	if target != alias.target {
		return alias.path, errors.New("temporary alias changed")
	}
	targetID, err := os.Lstat(alias.target)
	if err != nil {
		return alias.path, fmt.Errorf("inspect temporary target: %w", err)
	}
	if !targetID.IsDir() || !os.SameFile(alias.targetID, targetID) {
		return alias.path, errors.New("temporary target changed")
	}
	entries, err := os.ReadDir(alias.target)
	if err != nil || !joined || len(entries) != 0 {
		return alias.path, err
	}
	if err := os.Remove(alias.path); err != nil {
		return alias.path, err
	}
	if err := os.Remove(alias.directory); err != nil {
		return alias.directory, err
	}
	return "", nil
}
