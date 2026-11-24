//go:build darwin && cgo

package piglitter

/*
#cgo LDFLAGS: -lproc
#include <libproc.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type memberProcessHandle struct {
	pid   int
	birth string
}

func openMemberProcess(pid int, birth string) (*memberProcessHandle, error) {
	if _, err := memberProcessPath(pid, birth); err != nil {
		return nil, err
	}
	return &memberProcessHandle{pid: pid, birth: birth}, nil
}

func (handle *memberProcessHandle) kill() error {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		birth, err := processBirth(handle.pid)
		if errors.Is(err, os.ErrNotExist) || err == nil && birth != handle.birth {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-deadline.C:
			return errors.New("native member process has not exited")
		case <-ticker.C:
		}
	}
}

func (*memberProcessHandle) close() error { return nil }

func processBirth(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	start := process.Proc.P_starttime
	if int(process.Proc.P_pid) != pid || start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1000000 {
		return "", errors.New("native process start time is invalid")
	}
	return fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), nil
}

func memberProcessScope(pid int) (string, error) {
	before, err := processBirth(pid)
	if err != nil {
		return "", err
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(boot))
	if err != nil || bootID == uuid.Nil {
		return "", errors.New("native process boot identity is invalid")
	}
	after, err := processBirth(pid)
	if err != nil || before != after {
		return "", errors.New("native process identity changed")
	}
	return "darwin:" + bootID.String(), nil
}

func memberProcessPath(pid int, birth string) (string, error) {
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return "", errors.New("native process identity changed")
	}
	buffer := make([]byte, 4096)
	n := int(C.proc_pidpath(C.int(pid), unsafe.Pointer(&buffer[0]), C.uint32_t(len(buffer))))
	if n <= 0 || n > len(buffer) {
		return "", errors.New("native process executable is unavailable")
	}
	path := strings.TrimRight(string(buffer[:n]), "\x00")
	after, err := processBirth(pid)
	if err != nil || after != birth || path == "" {
		return "", errors.New("native process identity changed")
	}
	return path, nil
}

func memberProcessMatchesExecutable(pid int, birth, executable string) (bool, error) {
	path, err := memberProcessPath(pid, birth)
	if err != nil {
		return false, err
	}
	wanted, err := os.Stat(executable)
	if err != nil {
		return false, err
	}
	actual, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	after, err := processBirth(pid)
	if err != nil || after != birth {
		return false, errors.New("native process identity changed")
	}
	return os.SameFile(wanted, actual), nil
}

func memberProcessDigest(ctx context.Context, pid int, birth string) (string, error) {
	path, err := memberProcessPath(pid, birth)
	if err != nil {
		return "", err
	}
	digest, err := memberExecutableDigest(ctx, path)
	if err != nil {
		return "", err
	}
	after, err := processBirth(pid)
	if err != nil || after != birth {
		return "", errors.New("native process identity changed during hashing")
	}
	return digest, nil
}
