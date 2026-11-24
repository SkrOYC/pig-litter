package piglitter

import (
	"errors"
	"fmt"
	"net"
	"os"
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
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	start := process.Proc.P_starttime
	if int(process.Proc.P_pid) != pid || start.Sec <= 0 {
		return "", errors.New("invalid native host process record")
	}
	return fmt.Sprintf("%d.%d", start.Sec, start.Usec), nil
}

func watchLocalProcess(identity localProcessIdentity) (*localProcessWatch, error) {
	before, err := localProcessBirth(identity.PID)
	if err != nil || before != identity.Birth {
		return nil, errors.New("native host process identity is unavailable")
	}
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	change := unix.Kevent_t{Ident: uint64(identity.PID), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
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
		timeout := unix.NsecToTimespec(min(time.Until(expires), time.Second).Nanoseconds())
		var events [1]unix.Kevent_t
		count, err := unix.Kevent(w.fd, nil, events[:], &timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil || count > 0 && events[0].Flags&unix.EV_ERROR != 0 {
			return false
		}
		if count > 0 && events[0].Filter == unix.EVFILT_PROC && events[0].Fflags&unix.NOTE_EXIT != 0 {
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
