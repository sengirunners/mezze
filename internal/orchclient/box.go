package orchclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// EnvBoxCredential names the box credential file (potluck-box/1) a Potluck
// pool box's potluck-token job grant delivers: $POTLUCK_SECRETS/POTLUCK_TOKEN.
const EnvBoxCredential = "POTLUCK_BOX_CREDENTIAL"

// BoxClient is the client for a pool box: when getenv(EnvBoxCredential)
// names a file, the client reaches the authority that file pins, over its
// tunnel, signed with the box's key and carrying the box's token. Nothing
// else is read: no Unix socket, no autostart, no ZM_HOST or ZM_TOKEN. ZM_AS
// and ZM_PROJECT still apply. ok is false when the variable is unset or
// empty (today's client then, unchanged); a blank one is an error. Any problem with the file is an
// error, never a fallback; no error quotes the token or the key.
func BoxClient(getenv func(string) string) (c *client.Client, ok bool, err error) {
	path := getenv(EnvBoxCredential)
	if path == "" {
		return nil, false, nil
	}
	cred, err := tunnel.ReadBoxCredential(strings.TrimSpace(path))
	if err != nil {
		return nil, true, boxErr(err)
	}
	if c, err = tunnel.BoxClient(cred); err != nil {
		return nil, true, boxErr(err)
	}
	c.As = getenv(config.EnvAs)
	if p := getenv(config.EnvProject); p != "" {
		c.Project, c.ProjectFrom = p, "env"
	}
	return c, true, nil
}

// boxErr is a credential the box could not use; nothing was sent.
func boxErr(err error) error {
	return &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("%s: %v (no fallback to the zm daemon)", EnvBoxCredential, err)}
}

// BoxCallError is err as a box caller reads it: an answer not signed by
// the authority the credential pins, or a token the authority refuses
// (expired or revoked), says so plainly. Other errors are unchanged.
func BoxCallError(getenv func(string) string, err error) error {
	if err == nil || getenv(EnvBoxCredential) == "" {
		return err
	}
	if errors.Is(err, tunnel.ErrHomeKey) {
		return &api.Error{Code: api.CodeUnauthenticated, Message: EnvBoxCredential + ": the answer is not signed by the authority this credential pins (fingerprint mismatch): nothing was trusted"}
	}
	if errors.Is(err, tunnel.ErrUnsigned) { // an unsigned answer is the tunnel or the home being down, not a wrong key
		return &api.Error{Code: api.CodeUnavailable, Message: EnvBoxCredential + ": the authority this credential pins sent no signed answer (its tunnel or home is down): " + err.Error()}
	}
	var e *api.Error
	if errors.As(err, &e) && e.Code == api.CodeUnauthenticated {
		return &api.Error{Code: api.CodeUnauthenticated, Message: EnvBoxCredential + ": the authority refused this box's token (expired, revoked or not this box's): " + e.Message}
	}
	return err
}
