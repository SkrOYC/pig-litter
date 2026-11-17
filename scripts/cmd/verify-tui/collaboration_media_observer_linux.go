package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func collaborationMonotonicNow() (int64, error) {
	var value unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &value); err != nil {
		return 0, err
	}
	return value.Nano(), nil
}

func collaborationObserverPTY() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "observer-master")
	fail := func(err error) (*os.File, *os.File, error) {
		_ = master.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 33, Col: 100}); err != nil {
		_ = slave.Close()
		return fail(err)
	}
	return master, slave, nil
}

func runCollaborationMediaObserver(receipt, tmux, socket, session string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	master, slave, err := collaborationObserverPTY()
	if err != nil {
		return err
	}
	defer master.Close()
	command := exec.Command(tmux, "-S", socket, "-f", "/dev/null", "attach-session", "-E", "-f", "ignore-size", "-t", session)
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		_ = slave.Close()
		return err
	}
	_ = slave.Close()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	observer, observerErr := readObservedProcess(os.Getpid())
	client, clientErr := readObservedProcess(command.Process.Pid)
	if err := errors.Join(observerErr, clientErr); err != nil {
		_ = command.Process.Kill()
		<-wait
		return err
	}
	if err := writeCaptureJSON(receipt, collaborationObserverIdentity{Observer: observer, Client: client}); err != nil {
		_ = command.Process.Kill()
		<-wait
		return err
	}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(os.Stdout, master)
		copied <- err
	}()
	select {
	case err := <-wait:
		return err
	case <-copied:
		_ = command.Process.Signal(syscall.SIGTERM)
		return <-wait
	case <-ctx.Done():
		_ = command.Process.Signal(syscall.SIGTERM)
		<-wait
		return nil
	}
}
