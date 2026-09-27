package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const maxFileSize = 8 << 20

// checkedPath rejects symlinks in all managed path components. The repository
// is a dedicated checkout; concurrent external editors are not supported.
func checkedPath(root, relative string) (string, error) {
	p := filepath.Join(root, relative)
	parent := filepath.Dir(p)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil || filepath.Clean(real) != filepath.Clean(parent) {
		return "", fmt.Errorf("managed directory is missing or contains symlinks")
	}
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("managed path must be a regular file")
	}
	return p, nil
}

func readOptional(root, relative string) ([]byte, error) {
	p, err := checkedPath(root, relative)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || info.Size() > maxFileSize {
		return nil, fmt.Errorf("cannot read file or file exceeds 8 MiB")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("cannot read managed file")
	}
	return b, nil
}

// Atomic replacement is per file. The journal records both desired files so a
// failure between replacements can be resumed without creating backup copies.
func atomicWrite(name string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".rules-mcp-*")
	if err != nil {
		return fmt.Errorf("cannot create temporary file")
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot write and flush temporary file")
	}
	if err = os.Rename(temp, name); err != nil {
		return fmt.Errorf("cannot replace destination file")
	}
	return syncDirectory(filepath.Dir(name))
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
