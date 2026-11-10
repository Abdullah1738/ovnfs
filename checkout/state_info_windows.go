//go:build windows

package checkout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func stateLockInfo(f *os.File) (int, bool, error) {
	// A foreign open handle alone is not an exclusive lease. Reading the
	// protected byte observes a range conflict without acquiring any lock.
	var byteZero [1]byte
	_, err := f.ReadAt(byteZero[:], 0)
	if err == nil || errors.Is(err, io.EOF) {
		return 0, false, nil
	}
	if !errors.Is(err, syscall.Errno(33)) {
		return 0, false, err
	}
	// Query openers without acquiring byte zero: a status observer must not
	// temporarily exclude a daemon attempting its one-shot state lock.
	var status [2]uintptr
	var data [8192]uintptr
	query := syscall.NewLazyDLL("ntdll.dll").NewProc("NtQueryInformationFile")
	result, _, _ := query.Call(f.Fd(), uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&data)), unsafe.Sizeof(data), 47)
	if result != 0 {
		return 0, false, fmt.Errorf("query state file openers: NTSTATUS %#x", result)
	}
	count := int(data[0])
	if count < 0 || count >= len(data) {
		return 0, false, fmt.Errorf("invalid state opener count")
	}
	fields := strings.Fields(string(lockIdentity(f)))
	want := 0
	if len(fields) > 0 {
		want, _ = strconv.Atoi(fields[0])
	}
	owner := 0
	foreign := 0
	for _, value := range data[1 : count+1] {
		pid := int(value)
		if pid == os.Getpid() {
			continue
		}
		foreign++
		owner = pid
	}
	// The conflicting owner must have an open handle. Multiple openers make
	// the metadata ambiguous; only a sole foreign opener proves ownership.
	if foreign == 1 && owner == want {
		return owner, true, nil
	}
	return 0, true, nil
}
