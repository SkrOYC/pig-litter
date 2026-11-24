package piglitter

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func localOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

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
