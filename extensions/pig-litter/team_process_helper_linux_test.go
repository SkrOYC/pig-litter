//go:build linux && p08native

package piglitter

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestNativeProcessPathHelper(t *testing.T) {
	helper := os.Getenv("P08_PRODUCT_MEMBER")
	if helper == "" {
		t.Fatal("P08_PRODUCT_MEMBER is required")
	}
	pid := os.Getpid()
	birth, err := processBirth(pid)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(helper, "--pig-litter-process-path", strconv.Itoa(pid), birth).Output()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := os.Stat(executable)
	if err != nil || !os.SameFile(actual, wanted) {
		t.Fatal("helper returned a different process executable")
	}
	if err := exec.Command(helper, "--pig-litter-process-path", strconv.Itoa(pid), "wrong-birth").Run(); err == nil {
		t.Fatal("helper accepted a changed process identity")
	}
}
