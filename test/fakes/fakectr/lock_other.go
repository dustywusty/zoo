//go:build !unix

package main

// lock is a no-op where flock isn't available; run commands sequentially.
func lock(string) (func(), error) { return func() {}, nil }
