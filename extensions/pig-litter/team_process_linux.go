package piglitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type memberProcessHandle struct{ fd int }

func openMemberProcess(pid int, birth string) (*memberProcessHandle, error) {
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return nil, errors.New("native member process identity changed")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("open native member process handle: %w", err)
	}
	after, err := processBirth(pid)
	if err != nil || after != birth {
		_ = unix.Close(fd)
		return nil, errors.New("native member process identity changed while opening its handle")
	}
	return &memberProcessHandle{fd: fd}, nil
}

func (handle *memberProcessHandle) kill() error {
	err := unix.PidfdSendSignal(handle.fd, unix.SIGKILL, nil, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (handle *memberProcessHandle) close() error { return unix.Close(handle.fd) }

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

func memberProcessScope(pid int) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, path := range []string{"/proc/self/stat", "/proc/sys/kernel/random/boot_id", "/proc/self/ns", "/proc/" + strconv.Itoa(pid) + "/ns"} {
		var fs unix.Statfs_t
		if err := unix.Statfs(path, &fs); err != nil || fs.Type != unix.PROC_SUPER_MAGIC {
			return "", errors.New("native process scope is not available from procfs")
		}
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(string(boot)))
	if err != nil || bootID == uuid.Nil {
		return "", errors.New("native process boot identity is invalid")
	}
	namespaces := make([]string, 0, 2)
	for _, name := range []string{"pid", "time"} {
		link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/" + name)
		number, ok := strings.CutPrefix(link, name+":[")
		if err != nil || !ok || !strings.HasSuffix(number, "]") {
			return "", errors.New("native process namespace is unavailable")
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(number, "]"), 10, 64)
		if err != nil || inode == 0 {
			return "", errors.New("native process namespace is invalid")
		}
		namespaces = append(namespaces, fmt.Sprintf("%s:[%d]", name, inode))
	}
	return "linux:" + bootID.String() + ":" + strings.Join(namespaces, ":"), nil
}

func memberProcessMatchesExecutable(pid int, birth, executable string) (bool, error) {
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return false, errors.New("native member process identity changed")
	}
	wanted, err := os.Lstat(executable)
	if err != nil || !wanted.Mode().IsRegular() {
		return false, errors.New("native member executable is unavailable")
	}
	actual, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil || !actual.Mode().IsRegular() {
		return false, errors.New("native member process executable is unavailable")
	}
	after, err := processBirth(pid)
	if err != nil || after != birth {
		return false, errors.New("native member process identity changed")
	}
	return os.SameFile(wanted, actual), nil
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
