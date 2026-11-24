package piglitter

import (
	"errors"
	"fmt"
	"net"
	"os"
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
	if pid <= 0 {
		return localProcessIdentity{}, errors.New("invalid native host PID")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return localProcessIdentity{}, err
	}
	start := process.Proc.P_starttime
	if int(process.Proc.P_pid) != pid || start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1000000 {
		return localProcessIdentity{}, errors.New("invalid native host process record")
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return localProcessIdentity{}, err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(boot))
	if err != nil || bootID == uuid.Nil {
		return localProcessIdentity{}, errors.New("invalid boot identity")
	}
	return localProcessIdentity{PID: pid, Birth: fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), Scope: "darwin:" + bootID.String()}, nil
}

func openLocalProcessWatch(pid int) (localProcessWatch, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	return &platformLocalProcessWatch{fd: fd}, nil
}

func (w *platformLocalProcessWatch) exited() (bool, error) {
	var events [1]unix.Kevent_t
	timeout := unix.NsecToTimespec(0)
	count, err := unix.Kevent(w.fd, nil, events[:], &timeout)
	if err != nil || count > 0 && events[0].Flags&unix.EV_ERROR != 0 {
		return false, errors.Join(err, errors.New("native host watch unavailable"))
	}
	if count == 0 {
		return false, nil
	}
	if events[0].Filter == unix.EVFILT_PROC && events[0].Fflags&unix.NOTE_EXIT != 0 {
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
		timeout := unix.NsecToTimespec(min(time.Until(expires), time.Second).Nanoseconds())
		var events [1]unix.Kevent_t
		count, err := unix.Kevent(w.fd, nil, events[:], &timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil || count > 0 && events[0].Flags&unix.EV_ERROR != 0 {
			return false
		}
		if count > 0 {
			return events[0].Filter == unix.EVFILT_PROC && events[0].Fflags&unix.NOTE_EXIT != 0
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
	var credential *unix.Xucred
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		credential, callErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if callErr != nil {
		return 0, callErr
	}
	return credential.Uid, nil
}
