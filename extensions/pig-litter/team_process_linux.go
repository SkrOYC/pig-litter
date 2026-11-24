package piglitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func processBirth(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process identity for %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete process identity for %d", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("invalid process birth for %d: %w", pid, err)
	}
	return fields[19], nil
}

func memberProcessDigest(ctx context.Context, pid int, birth string) (string, error) {
	current, err := processBirth(pid)
	if err != nil || current != birth {
		return "", fmt.Errorf("native member process identity changed")
	}
	path := "/proc/" + strconv.Itoa(pid) + "/exe"
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", fmt.Errorf("native member executable is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", fmt.Errorf("native member executable changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, r: file}); err != nil {
		return "", err
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		return "", fmt.Errorf("native member executable changed during hashing")
	}
	current, err = processBirth(pid)
	if err != nil || current != birth {
		return "", fmt.Errorf("native member process identity changed during hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
