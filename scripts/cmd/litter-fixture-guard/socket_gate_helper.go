package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func holdBoundaryViaExec(ctx sdk.Context, action boundaryGateAction) (any, error) {
	var listener net.Listener
	var controlDone chan error
	controlAddress := ""
	if action == boundaryGateSDKAbort {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer listener.Close()
		controlAddress = listener.Addr().String()
		controlDone = make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				controlDone <- err
				return
			}
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil || line != "abort\n" {
				controlDone <- fmt.Errorf("helper abort request %q: %v", line, err)
				return
			}
			ctx.Abort()
			_, err = fmt.Fprintln(conn, "abort-returned")
			controlDone <- err
		}()
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	result, callErr := ctx.Exec(self, []string{"--boundary-socket-helper", string(action), controlAddress})
	if listener != nil {
		_ = listener.Close()
		if err := <-controlDone; callErr == nil && err != nil {
			callErr = err
		}
	}
	if callErr != nil {
		return nil, callErr
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("boundary helper exit %d: %s", result.ExitCode, result.Stderr)
	}
	return nil, nil
}

func boundarySocketHelper(action boundaryGateAction, controlAddress string) error {
	conn, err := net.DialTimeout("unix", os.Getenv("LITTER_FIXTURE_BOUNDARY_GATE_SOCKET"), 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, "entered"); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	if action == boundaryGateSDKAbort {
		command, err := reader.ReadString('\n')
		if err != nil || command != "abort\n" {
			return fmt.Errorf("boundary abort request %q: %v", command, err)
		}
		control, err := net.DialTimeout("tcp", controlAddress, 5*time.Second)
		if err != nil {
			return err
		}
		defer control.Close()
		if _, err := fmt.Fprintln(control, "abort"); err != nil {
			return err
		}
		ack, err := bufio.NewReader(control).ReadString('\n')
		if err != nil || strings.TrimSpace(ack) != "abort-returned" {
			return fmt.Errorf("SDK abort acknowledgement %q: %v", ack, err)
		}
		if _, err := fmt.Fprintln(conn, "abort-returned"); err != nil {
			return err
		}
	}
	release, err := reader.ReadString('\n')
	if err != nil || release != "release\n" {
		return fmt.Errorf("boundary release %q: %v", release, err)
	}
	return nil
}
