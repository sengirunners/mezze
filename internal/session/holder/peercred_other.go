//go:build !darwin && !linux

package holder

import (
	"errors"
	"net"
)

func peerCred(*net.UnixConn) (int, int, error) {
	return 0, 0, errors.New("holder: peer credentials are not supported on this OS")
}
