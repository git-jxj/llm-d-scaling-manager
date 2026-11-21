package main

// The answers' goldens pin the answer alone: the self-check is off in this
// package's tests unless a test asks for it (TestSelfCheck*).
func init() { defaultSelfCheck = false }
