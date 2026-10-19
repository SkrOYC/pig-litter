package main

import "testing"

func TestIdleObserverRequiresExactOwnedProcess(t *testing.T) {
	processes := []observedProcess{{PID: 10, StartTicks: "123", Executable: "/private/pig"}, {PID: 20, StartTicks: "456", Executable: "/private/tui-idle-observer"}}
	got, err := idleObserverProcess(20, "/private/tui-idle-observer", processes)
	if err != nil || got.PID != 20 || got.StartTicks != "456" || got.Executable != "/private/tui-idle-observer" {
		t.Fatalf("observer process=%#v, error=%v", got, err)
	}
	for _, bad := range []struct {
		pid        int
		executable string
		processes  []observedProcess
	}{
		{10, "/private/tui-idle-observer", processes},
		{20, "/private/pig", processes},
		{30, "/private/tui-idle-observer", processes},
		{20, "/private/tui-idle-observer", []observedProcess{{PID: 20, Executable: "/private/tui-idle-observer"}}},
	} {
		if got, err := idleObserverProcess(bad.pid, bad.executable, bad.processes); err == nil {
			t.Fatalf("accepted unowned process=%#v", got)
		}
	}
}
