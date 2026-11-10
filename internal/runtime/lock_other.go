//go:build !linux

package runtime

import "fmt"

type Lock struct{}

func AcquireLock(string) (*Lock, error) { return nil, fmt.Errorf("runtime requires Linux") }
func (*Lock) Close() error              { return nil }
