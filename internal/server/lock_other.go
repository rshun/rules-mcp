//go:build !linux

package server

import (
	"fmt"
	"os"
)

// Development tests only. The executable refuses to serve outside Linux.
func acquireLock(name string) (func(), error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("another rules-mcp operation is running")
	}
	return func() { f.Close(); os.Remove(name) }, nil
}
func syncDirectory(string) error { return nil }
