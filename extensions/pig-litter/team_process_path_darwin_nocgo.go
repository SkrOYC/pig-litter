//go:build darwin && !cgo

package piglitter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func memberProcessPath(pid int, birth, helper string) (string, error) {
	if !safeLaunchPath(helper) {
		return "", errors.New("native process inspection helper is unavailable")
	}
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return "", errors.New("native process identity changed")
	}
	info, err := os.Lstat(helper)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("native process inspection helper changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helper, "--pig-litter-process-path", strconv.Itoa(pid), birth)
	command.Env = []string{}
	output, err := command.Output()
	if err != nil || len(output) > 4096 {
		return "", errors.New("native process executable is unavailable")
	}
	path := strings.TrimSuffix(string(output), "\n")
	after, err := processBirth(pid)
	recheck, statErr := os.Lstat(helper)
	if err != nil || after != birth || statErr != nil || !os.SameFile(info, recheck) || info.Size() != recheck.Size() || !info.ModTime().Equal(recheck.ModTime()) || !filepath.IsAbs(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", errors.New("native process identity changed during inspection")
	}
	return path, nil
}
