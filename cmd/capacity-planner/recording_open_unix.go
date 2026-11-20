//go:build unix

package main

import "syscall"

// openNonBlock opens a pipe without waiting for a writer, so that the server
// can refuse it (openRecording); a regular file reads as it does without it.
const openNonBlock = syscall.O_NONBLOCK
