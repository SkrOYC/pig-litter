package piglitter

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func localOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

type localProcessWatch struct{ fd int }

func localProcessBirth(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid native host PID")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("invalid native host process record")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", errors.New("invalid native host process record")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19], nil
}

func watchLocalProcess(identity localProcessIdentity) (*localProcessWatch, error) {
	before, err := localProcessBirth(identity.PID)
	if err != nil || before != identity.Birth {
		return nil, errors.New("native host process identity is unavailable")
	}
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		return nil, err
	}
	after, err := localProcessBirth(identity.PID)
	if err != nil || after != identity.Birth {
		return nil, errors.Join(errors.New("native host changed while arming exit observation"), unix.Close(fd))
	}
	return &localProcessWatch{fd: fd}, nil
}

func (w *localProcessWatch) wait(stop <-chan struct{}, expires time.Time) bool {
	defer unix.Close(w.fd)
	for time.Now().Before(expires) {
		select {
		case <-stop:
			return false
		default:
		}
		remaining := time.Until(expires)
		timeout := int(min(remaining, time.Second).Milliseconds())
		if timeout < 1 {
			timeout = 1
		}
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		count, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil || count > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return false
		}
		if count > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return true
		}
	}
	return false
}

func (w *localProcessWatch) close() error { return unix.Close(w.fd) }

func localPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Ucred
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		credential, callErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if callErr != nil {
		return 0, callErr
	}
	return credential.Uid, nil
}
