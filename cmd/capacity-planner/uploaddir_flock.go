//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"os"
	"syscall"
)

// uploadDirLocks reports whether lockUploadDir locks: here it does.
const uploadDirLocks = true

// lockHeld reports whether err is lockUploadDir's on a directory another
// server holds.
func lockHeld(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }

// lockUploadDir opens dir and takes an exclusive lock on it, without
// waiting: an error when another server holds it. The system drops the lock
// when the holder exits, however it exits, so a directory no one holds was
// left by a server that stopped.
func lockUploadDir(dir string) (*os.File, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// removeLockedDir removes dir, which lock holds, then lets the lock go: a
// server starting meanwhile cannot take the directory being removed.
func removeLockedDir(dir string, lock *os.File) error {
	err := os.RemoveAll(dir)
	_ = lock.Close()
	return err
}
