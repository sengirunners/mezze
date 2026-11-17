//go:build !darwin

package session

import "errors"

// cloneFile is macOS only: elsewhere a running binary cannot be overwritten
// in place (ETXTBSY), so holders run the installed one.
func cloneFile(from, to string) error { return errors.ErrUnsupported }
