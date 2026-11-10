//go:build !linux

package media

func available(string) (uint64, error) { return ^uint64(0), nil }
