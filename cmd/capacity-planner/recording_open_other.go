//go:build !unix

package main

// openNonBlock is no flag off unix, where opening a file waits on no FIFO.
const openNonBlock = 0
