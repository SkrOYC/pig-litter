package main

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestBoundarySocketHelperKeepsGateUntilRelease(t *testing.T) {
	for _, action := range []boundaryGateAction{boundaryGateHold, boundaryGateSDKAbort} {
		t.Run(string(action), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gate.sock")
			boundary, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer boundary.Close()
			t.Setenv("LITTER_FIXTURE_BOUNDARY_GATE_SOCKET", path)
			controlAddress := ""
			var control net.Listener
			if action == boundaryGateSDKAbort {
				control, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				controlAddress = control.Addr().String()
			}
			done := make(chan error, 1)
			go func() { done <- boundarySocketHelper(action, controlAddress) }()
			_ = boundary.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second))
			conn, err := boundary.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(conn)
			if entered, err := reader.ReadString('\n'); err != nil || entered != "entered\n" {
				t.Fatalf("gate entry = %q, %v", entered, err)
			}
			if action == boundaryGateSDKAbort {
				if _, err := fmt.Fprintln(conn, "abort"); err != nil {
					t.Fatal(err)
				}
				_ = control.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
				abort, err := control.Accept()
				if err != nil {
					t.Fatal(err)
				}
				_ = abort.SetDeadline(time.Now().Add(5 * time.Second))
				if request, err := bufio.NewReader(abort).ReadString('\n'); err != nil || request != "abort\n" {
					t.Fatalf("abort request = %q, %v", request, err)
				}
				if _, err := fmt.Fprintln(abort, "abort-returned"); err != nil {
					t.Fatal(err)
				}
				_ = abort.Close()
				if ack, err := reader.ReadString('\n'); err != nil || ack != "abort-returned\n" {
					t.Fatalf("abort acknowledgment = %q, %v", ack, err)
				}
			}
			select {
			case err := <-done:
				t.Fatalf("helper left the held gate: %v", err)
			default:
			}
			if _, err := fmt.Fprintln(conn, "release"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("helper did not exit after release")
			}
		})
	}
}
