//go:build !linux

package service

func Notify(string) error { return nil }
