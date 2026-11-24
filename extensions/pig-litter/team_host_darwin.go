package piglitter

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func embeddedPeerHost() (peerHostIdentity, error) {
	pid := os.Getpid()
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return peerHostIdentity{}, err
	}
	start := process.Proc.P_starttime
	if int(process.Proc.P_pid) != pid || start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1000000 {
		return peerHostIdentity{}, errors.New("invalid peer process start time")
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return peerHostIdentity{}, err
	}
	bootID, err := uuid.Parse(strings.TrimSpace(boot))
	if err != nil || bootID == uuid.Nil {
		return peerHostIdentity{}, errors.New("invalid boot identity")
	}
	return peerHostIdentity{PID: pid, Birth: fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), Scope: "darwin:" + bootID.String()}, nil
}
