package orchcore

import "context"

type receiptKey struct{}

// AsAskNotice marks mail sent with ctx as an ask notice: what zm tells an
// asker or item holder about an ask's outcome, already in the ask's record.
// To an ended session it stays there; the orphan sweep never copies it to a machine.
func AsAskNotice(ctx context.Context) context.Context {
	return context.WithValue(ctx, receiptKey{}, true)
}

// IsAskNotice reports whether ctx came from AsAskNotice.
func IsAskNotice(ctx context.Context) bool {
	v, _ := ctx.Value(receiptKey{}).(bool)
	return v
}
