//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// uploadDirLocks reports whether lockUploadDir locks: here it does.
const uploadDirLocks = true

// uploadDirLockName is the file in a store's directory that lockUploadDir
// holds open: Windows locks no directory, but a file opened with no sharing
// cannot be opened again until its holder closes it or exits.
const uploadDirLockName = "lock"

// lockHeld reports whether err is lockUploadDir's on a directory another
// server holds.
func lockHeld(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }

// lockUploadDir opens dir's lock file, made when missing, shared with no
// one: an error when another server holds it. The system closes it when the
// holder exits, however it exits, so a directory no one holds was left by a
// server that stopped.
func lockUploadDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, uploadDirLockName)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "lock", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// removeLockedDir removes dir, which lock holds, and lets the lock go:
// Windows removes no file that is open, so the lock goes first.
func removeLockedDir(dir string, lock *os.File) error {
	_ = lock.Close()
	return os.RemoveAll(dir)
}
