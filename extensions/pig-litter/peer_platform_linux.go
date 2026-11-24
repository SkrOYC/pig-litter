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

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func localOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

type platformLocalProcessWatch struct{ fd int }

func observeLocalProcess(pid int) (localProcessIdentity, error) {
	var zero localProcessIdentity
	if pid <= 0 {
		return zero, errors.New("invalid native host PID")
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return zero, err
	}
	selfStart := strings.IndexByte(string(self), '(')
	if selfStart < 0 || strings.TrimSpace(string(self[:selfStart])) != strconv.Itoa(os.Getpid()) {
		return zero, errors.New("process records use a different PID namespace")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return zero, err
	}
	text := string(data)
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if start < 0 || end <= start || strings.TrimSpace(text[:start]) != strconv.Itoa(pid) {
		return zero, errors.New("invalid native host process record")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) <= 19 {
		return zero, errors.New("invalid native host process record")
	}
	birth, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return zero, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return zero, err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(string(boot)))
	if err != nil || bootID == uuid.Nil {
		return zero, errors.New("invalid boot identity")
	}
	namespaces := make([]string, 0, 2)
	for _, name := range []string{"pid", "time"} {
		own, err := os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			return zero, err
		}
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, name))
		if err != nil || target != own {
			return zero, errors.New("native host namespace is unavailable")
		}
		number, ok := strings.CutPrefix(own, name+":[")
		if !ok || !strings.HasSuffix(number, "]") {
			return zero, errors.New("invalid native host namespace")
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(number, "]"), 10, 64)
		if err != nil || inode == 0 {
			return zero, errors.New("invalid native host namespace")
		}
		namespaces = append(namespaces, fmt.Sprintf("%s:[%d]", name, inode))
	}
	return localProcessIdentity{PID: pid, Birth: fmt.Sprintf("linux:%d", birth), Scope: "linux:" + bootID.String() + ":" + strings.Join(namespaces, ":")}, nil
}

func openLocalProcessWatch(pid int) (localProcessWatch, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return &platformLocalProcessWatch{fd: fd}, nil
}

func (w *platformLocalProcessWatch) exited() (bool, error) {
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	count, err := unix.Poll(fds, 0)
	if err != nil || count > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
		return false, errors.Join(err, errors.New("native host watch unavailable"))
	}
	if count == 0 {
		return false, nil
	}
	if fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
		return true, nil
	}
	return false, errors.New("unexpected native host watch event")
}

func (w *platformLocalProcessWatch) wait(stop <-chan struct{}, expires time.Time) bool {
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
		if count > 0 {
			return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
		}
	}
	return false
}

func (w *platformLocalProcessWatch) close() error { return unix.Close(w.fd) }

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
