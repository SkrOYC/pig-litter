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
	return peerObservedHost(os.Getpid())
}

func peerObservedHost(pid int) (peerHostIdentity, error) {
	if pid <= 0 || pid > 1<<31-1 {
		return peerHostIdentity{}, errors.New("invalid peer process ID")
	}
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

func peerHostGone(prior, current peerHostIdentity) bool {
	if prior.PID <= 0 || prior.PID > 1<<31-1 || prior.Scope == "" || prior.Scope != current.Scope || prior == current {
		return false
	}
	var sec, usec int64
	if _, err := fmt.Sscanf(prior.Birth, "darwin:%d:%d", &sec, &usec); err != nil || sec <= 0 || usec < 0 || usec >= 1000000 || prior.Birth != fmt.Sprintf("darwin:%d:%d", sec, usec) {
		return false
	}
	self, selfErr := peerObservedHost(os.Getpid())
	native, nativeErr := peerObservedHost(current.PID)
	if selfErr != nil || nativeErr != nil || self.Scope != prior.Scope || native != current {
		return false
	}
	// Signal zero only checks existence. A matching process or zombie stays uncertain.
	err := unix.Kill(prior.PID, 0)
	if err == nil {
		observed, observeErr := peerObservedHost(prior.PID)
		if observeErr != nil || observed.Scope != prior.Scope || observed.Birth == prior.Birth {
			return false
		}
	} else if !errors.Is(err, unix.ESRCH) {
		return false
	}
	selfAfter, selfErr := peerObservedHost(os.Getpid())
	nativeAfter, nativeErr := peerObservedHost(current.PID)
	return selfErr == nil && nativeErr == nil && selfAfter == self && nativeAfter == current
}
