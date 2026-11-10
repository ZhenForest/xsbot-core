//go:build linux

package runtime

import (
	"fmt"
	"os"
	"syscall"
)

type Lock struct{ file *os.File }

func AcquireLock(path string) (*Lock, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another Core or offline maintenance process holds the lock: %w", err)
	}
	return &Lock{file: f}, nil
}
func (l *Lock) Close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
