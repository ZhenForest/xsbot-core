//go:build !linux

package plugins

import "fmt"

func load(file string) Candidate {
	return Candidate{File: file, Problem: fmt.Errorf("Go plugin runtime requires Linux")}
}
