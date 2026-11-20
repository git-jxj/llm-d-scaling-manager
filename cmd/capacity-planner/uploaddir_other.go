//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package main

import "os"

// uploadDirLocks reports whether lockUploadDir locks: here it does not, so
// no directory is swept, since a live server's cannot be told from a
// stopped one's.
const uploadDirLocks = false

// lockHeld reports whether err is lockUploadDir's on a directory another
// server holds: never, here.
func lockHeld(error) bool { return false }

// lockUploadDir opens dir; it takes no lock.
func lockUploadDir(dir string) (*os.File, error) {
	return os.Open(dir)
}
