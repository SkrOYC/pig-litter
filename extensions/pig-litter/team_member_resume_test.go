//go:build linux

package piglitter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestNativeReconciliationRefusesUnknownOwnership(t *testing.T) {
	ctx := context.Background()
	if err := confirmOldMemberGone(ctx, &memberOwnership{}); err == nil {
		t.Fatal("unknown pane identity was accepted")
	}
	birth, err := processBirth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := memberProcessScope(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	boot, bootErr := os.ReadFile("/proc/sys/kernel/random/boot_id")
	pidNamespace, pidErr := os.Readlink("/proc/self/ns/pid")
	timeNamespace, timeErr := os.Readlink("/proc/self/ns/time")
	want := fmt.Sprintf("linux:%s:%s:%s", strings.TrimSpace(string(boot)), pidNamespace, timeNamespace)
	if bootErr != nil || pidErr != nil || timeErr != nil || scope != want {
		t.Fatalf("native process scope = %q, want %q", scope, want)
	}
	live := &memberOwnership{ProcessScope: scope, Pane: &memberPaneIdentity{PID: os.Getpid(), Birth: birth}}
	if err := confirmOldMemberGone(ctx, live); err == nil {
		t.Fatal("live process was accepted")
	}
	if current, err := processBirth(os.Getpid()); err != nil || current != birth {
		t.Fatalf("reconciliation changed the live process: %q, %v", current, err)
	}
	unknown := &memberOwnership{ProcessScope: scope, Pane: &memberPaneIdentity{PID: 1 << 30, Birth: "old"}, Server: memberServerIdentity{PID: os.Getpid(), Birth: "different"}}
	if err := confirmOldMemberGone(ctx, unknown); err == nil {
		t.Fatal("changed server identity was accepted")
	}
	unknown.ProcessScope = "different"
	if err := confirmOldMemberGone(ctx, unknown); err == nil {
		t.Fatal("changed process scope was accepted")
	}
	unknown.ProcessScope = ""
	if err := confirmOldMemberGone(ctx, unknown); err == nil {
		t.Fatal("legacy ownership without a process scope was accepted")
	}
}
