//go:build !unix

package fake

func lock(string) func() { return func() {} }
