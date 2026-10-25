package parley

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AtomicWriteFile writes b to path with a cross-process lock sibling
// ("<path>.lock"), then a temp-file rename (crash-safe).
func AtomicWriteFile(path string, b []byte, perm os.FileMode) error {
	return LockedWrite(path, b, perm)
}

// LockedWrite serializes writers across processes using a lock sibling
// ("<path>.lock", O_CREATE|O_EXCL), then writes atomically via temp file
// + rename. A lock older than 5s is stale (writer crash) and is broken;
// acquisition fails after ~2s while the lock remains healthy.
func LockedWrite(path string, b []byte, perm os.FileMode) error {
	lockPath := path + ".lock"
	deadline := time.Now().Add(2 * time.Second)
	var lockFile *os.File
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			lockFile = f
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("parley: lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 5*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("parley: lock: timeout waiting for %s", lockPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lockFile != nil {
		defer lockFile.Close()
		defer os.Remove(lockPath)
	}
	return atomicRename(path, b, perm)
}

func atomicRename(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".parley-tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
