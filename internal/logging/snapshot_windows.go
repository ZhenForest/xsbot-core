//go:build windows

package logging

import (
	"os"
	"syscall"
)

func openSnapshotFile(path string) (*os.File, error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: e}
	}
	return os.NewFile(uintptr(h), path), nil
}
