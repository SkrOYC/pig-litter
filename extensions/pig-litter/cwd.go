package piglitter

import (
	"fmt"
	"os"
	"path/filepath"
)

func childDirectory(base, requested string) (string, error) {
	path := requested
	if path == "" {
		path = base
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", reject("rejected", fmt.Sprintf("child cwd is unavailable: %v", err))
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", reject("rejected", "child cwd must be an existing directory")
	}
	return path, nil
}
