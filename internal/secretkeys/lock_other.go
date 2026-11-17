//go:build !unix

package secretkeys

// lock is a no-op where zm does not run as a daemon.
func lock(string) (func(), error) { return func() {}, nil }
