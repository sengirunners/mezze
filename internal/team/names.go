package team

import (
	"bytes"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// mailNames resolves a session name of this machine (SPEC 8 "Names") in
// POST /v0/mail's "to" before the call goes on to an authority, which does
// not know this machine's names: the forwarded body carries the session's
// address, so the mail stores it. Only a session the caller may see is
// resolved; any other name goes on as typed and is answered as missing.
func (s *Service) mailNames(r *http.Request) error {
	if r.Method != http.MethodPost || r.URL.Path != "/v0/mail" {
		return nil
	}
	sn, ok := orchcore.Sessions(s.d).(orchcore.SessionNames)
	if !ok {
		return nil
	}
	body, err := teamlink.ReadBody(r)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var m map[string]json.RawMessage
	var to string
	if json.Unmarshal(body, &m) != nil || json.Unmarshal(m["to"], &to) != nil {
		return nil // the authority answers the bad body
	}
	machine, name, named := api.SessionNameRef(to)
	if !named || machine != s.d.Config.Machine {
		return nil // a bare name is a machine's mailbox; another machine's names are not ours
	}
	ctx, ref := r.Context(), machine+"/"+name
	ss, ok := sn.ResolveName(ctx, ref)
	if sub := core.SubjectOf(ctx); !ok || sub == nil || !s.d.Visible(ctx, sub.Scope, ss.Address) {
		return nil
	}
	if m["to"], err = json.Marshal(ss.Address); err != nil {
		return err
	}
	if body, err = json.Marshal(m); err != nil {
		return err
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return nil
}
