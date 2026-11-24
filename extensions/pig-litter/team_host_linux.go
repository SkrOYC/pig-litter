package piglitter

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func embeddedPeerHost() (peerHostIdentity, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, path := range []string{"/proc/self/stat", "/proc/self/ns", "/proc/thread-self/ns", "/proc/sys/kernel/random/boot_id"} {
		var fs unix.Statfs_t
		if err := unix.Statfs(path, &fs); err != nil || fs.Type != unix.PROC_SUPER_MAGIC {
			return peerHostIdentity{}, errors.New("peer process identity is unavailable")
		}
	}
	for _, name := range []string{"pid", "time"} {
		leader, leaderErr := os.Readlink("/proc/self/ns/" + name)
		thread, threadErr := os.Readlink("/proc/thread-self/ns/" + name)
		if leaderErr != nil || threadErr != nil || leader != thread {
			return peerHostIdentity{}, errors.New("peer process namespace differs")
		}
	}
	return peerObservedHost(os.Getpid())
}

func peerObservedHost(pid int) (peerHostIdentity, error) {
	var identity peerHostIdentity
	if pid <= 0 {
		return identity, errors.New("invalid host PID")
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return identity, err
	}
	start := strings.IndexByte(string(self), '(')
	if start < 0 || strings.TrimSpace(string(self[:start])) != strconv.Itoa(os.Getpid()) {
		return identity, errors.New("process namespace differs")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return identity, err
	}
	text := string(data)
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if start < 0 || end <= start || strings.TrimSpace(text[:start]) != strconv.Itoa(pid) {
		return identity, errors.New("invalid process record")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) <= 19 {
		return identity, errors.New("invalid process birth")
	}
	birth, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return identity, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return identity, err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(string(boot)))
	if err != nil || bootID == uuid.Nil {
		return identity, errors.New("invalid boot identity")
	}
	namespaces := make([]string, 0, 2)
	for _, name := range []string{"pid", "time"} {
		own, err := os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			return identity, err
		}
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, name))
		if err != nil || target != own {
			return identity, errors.New("host namespace is unavailable")
		}
		number, ok := strings.CutPrefix(own, name+":[")
		if !ok || !strings.HasSuffix(number, "]") {
			return identity, errors.New("invalid process namespace")
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(number, "]"), 10, 64)
		if err != nil || inode == 0 {
			return identity, errors.New("invalid process namespace")
		}
		namespaces = append(namespaces, fmt.Sprintf("%s:[%d]", name, inode))
	}
	return peerHostIdentity{PID: pid, Birth: fmt.Sprintf("linux:%d", birth), Scope: "linux:" + bootID.String() + ":" + strings.Join(namespaces, ":")}, nil
}

func peerUnfilteredThread() bool {
	data, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Seccomp:" {
			return fields[1] == "0"
		}
	}
	return false
}

func peerHostGone(prior, current peerHostIdentity) bool {
	if prior.PID <= 0 || current.PID <= 0 || prior.Scope == "" || prior.Scope != current.Scope || prior == current {
		return false
	}
	birth, ok := strings.CutPrefix(prior.Birth, "linux:")
	if !ok {
		return false
	}
	value, err := strconv.ParseUint(birth, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != birth {
		return false
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, path := range []string{"/proc/self/stat", "/proc/self/ns", "/proc/thread-self/ns", "/proc/thread-self/status", "/proc/sys/kernel/random/boot_id"} {
		var fs unix.Statfs_t
		if err := unix.Statfs(path, &fs); err != nil || fs.Type != unix.PROC_SUPER_MAGIC {
			return false
		}
	}
	for _, name := range []string{"pid", "time"} {
		leader, leaderErr := os.Readlink("/proc/self/ns/" + name)
		thread, threadErr := os.Readlink("/proc/thread-self/ns/" + name)
		if leaderErr != nil || threadErr != nil || leader != thread {
			return false
		}
	}
	self, selfErr := peerObservedHost(os.Getpid())
	native, nativeErr := peerObservedHost(current.PID)
	if selfErr != nil || nativeErr != nil || self.Scope != current.Scope || native != current || !peerUnfilteredThread() {
		return false
	}
	fd, err := unix.PidfdOpen(prior.PID, 0)
	gone := errors.Is(err, unix.ESRCH)
	if err == nil {
		defer unix.Close(fd)
		observed, observeErr := peerObservedHost(prior.PID)
		if observeErr != nil || observed.Scope != prior.Scope {
			return false
		}
		if observed.Birth != prior.Birth {
			gone = true
		} else {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			count, pollErr := unix.Poll(fds, 0)
			gone = pollErr == nil && count > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) == 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
		}
	} else if !gone {
		return false
	}
	selfAfter, selfErr := peerObservedHost(os.Getpid())
	nativeAfter, nativeErr := peerObservedHost(current.PID)
	return gone && selfErr == nil && nativeErr == nil && selfAfter == self && nativeAfter == current && peerUnfilteredThread()
}
