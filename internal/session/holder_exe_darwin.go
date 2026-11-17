package session

import "golang.org/x/sys/unix"

// cloneFile makes an APFS clone (copy-on-write) of from at to.
func cloneFile(from, to string) error { return unix.Clonefile(from, to, unix.CLONE_NOFOLLOW) }
