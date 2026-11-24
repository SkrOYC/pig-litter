//go:build darwin && cgo

package piglitter

/*
#cgo LDFLAGS: -lproc
#include <libproc.h>
*/
import "C"

import (
	"errors"
	"strings"
	"unsafe"
)

func memberProcessPath(pid int, birth, _ string) (string, error) {
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return "", errors.New("native process identity changed")
	}
	buffer := make([]byte, 4096)
	n := int(C.proc_pidpath(C.int(pid), unsafe.Pointer(&buffer[0]), C.uint32_t(len(buffer))))
	if n <= 0 || n > len(buffer) {
		return "", errors.New("native process executable is unavailable")
	}
	path := strings.TrimRight(string(buffer[:n]), "\x00")
	after, err := processBirth(pid)
	if err != nil || after != birth || path == "" {
		return "", errors.New("native process identity changed")
	}
	return path, nil
}
