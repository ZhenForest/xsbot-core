//go:build !windows

package logging

import "os"

func openSnapshotFile(path string) (*os.File, error) { return os.Open(path) }
