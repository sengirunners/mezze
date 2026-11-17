package clean

import (
	"errors"

	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// Err removes secrets from an error's text. An *api.Error keeps its
// type; any other error keeps its identity for errors.Is (context.Canceled,
// a timeout), but not for errors.As, which could reach the unmasked text.
func Err(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	e, ok := err.(*api.Error)
	if !ok {
		msg := adapter.Mask(err.Error(), secrets)
		if msg == err.Error() {
			return err
		}
		return &redacted{msg: msg, err: err}
	}
	c := *e
	c.Message, c.Hint = adapter.Mask(c.Message, secrets), adapter.Mask(c.Hint, secrets)
	return &c
}

// redacted is an error whose text had a secret masked.
type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }

// Is answers for the original error's chain (sentinels carry no secret);
// there is deliberately no Unwrap.
func (r *redacted) Is(target error) bool { return errors.Is(r.err, target) }
