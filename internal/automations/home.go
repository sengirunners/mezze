package automations

// The home rule (SPEC 18.6 "Who may change a definition"): a definition is
// code that runs with its home's credentials, so only the home's own Mezze app,
// writing as the home, changes it here. The caller is the daemon of the
// record's home machine (its socket owner; at the authority, that
// machine's node-bound token, the door's rule), with operate on the whole
// project; the daemon that took the socket call checked it comes from no
// registered agent session and marked it (internal/team/origin_gate.go);
// and the call names the install that holds the home's claim. A guard
// against mistakes, not a boundary: any process of the home's OS user can
// write as the home.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/sengirunners/mezze/internal/automations/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// homeHint is where a change from anywhere but the home goes.
const homeHint = "the home's own Mezze app writes its records; from anywhere else a change is a proposal a person approves (SPEC 18.6)"

// callerMachine is the machine whose own daemon makes the call: this
// machine for its socket owner (no token), or a member's machine for its
// node-bound token. ok is false for anyone else (a minted token, a tailnet
// login, the daemon itself).
func (s *Service) callerMachine(sub *authz.Subject) (string, bool) {
	switch {
	case sub == nil:
		return "", false
	case sub.Via == authz.ViaLocal && sub.TokenID == "" && sub.Node == "":
		return s.d.Config.Machine, true
	case sub.Via == authz.ViaToken && sub.Node != "" && sub.Machine != "" && sub.Machine != s.d.Config.Machine:
		return sub.Machine, true
	}
	return "", false
}

// asHome is the home rule's caller check for a write to home's records in
// project (the claim is checked by holds).
func (s *Service) asHome(r *http.Request, project, home string) error {
	sub := core.SubjectOf(r.Context())
	if sub != nil && sub.Session != "" { // it says it is a session (Zm-As), wherever its process runs
		return api.Errorf(api.CodeForbidden, "this call acts as agent session %s: a home's automation records are written by its Mezze app server, outside any agent session", sub.Session).
			WithHint("an agent changes a job through the home's Mezze app, or files a proposal a person approves")
	}
	m, ok := s.callerMachine(sub)
	if !ok {
		return api.Errorf(api.CodeForbidden, "automation records are written only by their home machine's own daemon").WithHint(homeHint)
	}
	if m != home {
		return api.Errorf(api.CodeForbidden, "home %s's records are written only by %s's own daemon; this call comes from %s", home, home, m).WithHint(homeHint)
	}
	if !sub.Scope.WholeProject(project) || !sub.Can(authz.Operate, authz.Object{Project: project}) {
		return api.Errorf(api.CodeForbidden, "a home writes its records with operate on the whole project")
	}
	return orchcore.OriginChecked(sub, r.URL.Query().Get(orchapi.OriginChecked) == "1", "its automation record writes",
		api.Errorf(api.CodeForbidden, "automation records are written only by their home machine's own daemon").WithHint(homeHint))
}

// holds checks install holds home's claim; a definition write also needs
// the claim's store to be zm.
func (s *Service) holds(ctx context.Context, st store.Store, home, install string, definition bool) error {
	if err := validInstall(install); err != nil {
		return err
	}
	h, err := st.Home(ctx, home)
	if errors.Is(err, store.ErrNotFound) {
		return api.Errorf(api.CodeConflict, "home %s has no claim: its Mezze app claims it first (PUT /v0/automation-homes/%s)", home, home)
	}
	if err != nil {
		return err
	}
	if h.Install != install {
		return api.Errorf(api.CodeConflict, "install %s does not hold home %s; install %s does", install, home, h.Install).
			WithHint("this copy of the Mezze app must not write the home's records or fire its jobs").WithData(homeView(h))
	}
	if definition && h.Store != orchapi.AutomationStoreZm {
		return api.Errorf(api.CodeConflict, "home %s keeps its definitions in the Mezze app (store %s): zm takes no definition writes for it", home, h.Store).
			WithHint("switch the home to Z3_AUTOMATION_STORE=zm and restart it")
	}
	return nil
}

func parseRev(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, bad("rev must be a positive whole number")
	}
	return n, nil
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonMarshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return b, err
}

// jsonStrict decodes b into v refusing unknown keys.
func jsonStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// scrub is the module's redaction scrubber (rule 5): bodies, fields,
// settings, history and apply errors in the project's records; at any
// length in records the scrub names.
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	st, err := s.storeOf(sc.Project)
	if err != nil {
		return nil // no such project here
	}
	return st.Scrub(ctx, sc.Subjects, sc.Texts, core.MinUnscoped)
}
