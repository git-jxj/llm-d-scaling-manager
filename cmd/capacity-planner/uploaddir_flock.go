//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"syscall"
)

// uploadDirLocks reports whether lockUploadDir locks: here it does.
const uploadDirLocks = true

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
