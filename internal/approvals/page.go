package approvals

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// View is what the calling device may see. Asks and confirmations only for
// an active approver; a device that is not paired sees only itself.
func (s *Service) View(ctx context.Context, rm *core.Remote) (orchapi.DeviceView, error) {
	v := orchapi.DeviceView{Node: rm.NodeID, NodeName: rm.Node}
	a, ok := s.approverOf(ctx, rm)
	if !ok {
		if err := phoneRefusal(rm); err == nil {
			v.OneTap = true
		} else if e := (*api.Error)(nil); isPhoneOS(rm.OS) && errors.As(err, &e) {
			v.OneTapWhy = strings.TrimSpace(e.Message + ". " + e.Hint)
		}
		return v, nil
	}
	v.Approver = &a.Approver
	if a.State == orchapi.ApproverUnconfirmed {
		if st, err := s.stuck(ctx, s.d.Store.Registry()); err == nil && st {
			a.Note = s.stuckNote(a.Name, s.markerForDevice())
		}
	}
	if a.State != orchapi.ApproverActive || rm.Self {
		return v, nil
	}
	pub, err := s.PublicKey()
	if err != nil {
		return v, api.Errorf(api.CodeUnavailable, "authority receipt key unavailable")
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE d.ask IS NULL AND a.withdrawn IS NULL`+notHeld+` ORDER BY a.priority, a.id DESC LIMIT 100`)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		ask, err := scanAsk(rows)
		if err != nil {
			return v, err
		}
		ask.ReceiptKey = KeyID(pub)
		v.Asks = append(v.Asks, ask)
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	// The person: whose device the machine's owner said this is when adding
	// it (zm approver add --for), read by that identity's own grants; else
	// the machine's owner, as the socket reads. Deciding stays the device's
	// (POST decide).
	person := s.d.Projects.Local("")
	if a.identity != "" {
		if person, err = s.d.Projects.IdentitySubject(ctx, a.identity); err != nil {
			return v, err
		}
	}
	v.Identity = first(person.Identity.Name, s.d.Config.Human)
	if s.d.GitHubLogin != nil && person.Identity.ID != "" {
		v.GitHub = s.d.GitHubLogin(ctx, person.Identity.ID)
	}
	in, err := s.inbox(core.WithSubject(ctx, person), url.Values{})
	if err != nil {
		return v, err
	}
	v.Decisions, v.Skipped = in.Decisions, in.Skipped
	if fy, ok := orchcore.Mail(s.d).(orchcore.ForYouMail); ok {
		if v.ForYou, err = fy.ForYou(ctx, person, 100); err != nil {
			return v, err
		}
	}
	// The joined projects' items live at their authorities.
	if tf, ok := orchcore.Team(s.d).(orchcore.TeamForYou); ok {
		ms, skipped := tf.TeamForYou(ctx, person, 100)
		v.ForYou, v.Skipped = newestFirst(append(v.ForYou, ms...), 100), append(v.Skipped, skipped...)
	}
	un, err := s.unconfirmed(ctx)
	if err != nil {
		return v, err
	}
	for _, u := range un {
		if u.Node != a.Node {
			v.Confirm = append(v.Confirm, u)
		}
	}
	return v, nil
}

// devicePerson is the person approver a acts for: its --for identity by its
// own grants, else this daemon's machine owner.
func (s *Service) devicePerson(ctx context.Context, a approverRow) (*authz.Subject, error) {
	if a.identity == "" {
		return s.d.Projects.Local(""), nil
	}
	return s.d.Projects.IdentitySubject(ctx, a.identity)
}

// DevicePerson is core.Deps.DevicePerson: the person of the active approver
// bound to tailnet node, for its phone alerts. Any other node: ok false.
func (s *Service) DevicePerson(ctx context.Context, node string) (*authz.Subject, bool) {
	if node == "" {
		return nil, false
	}
	a, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, node))
	if err != nil || a.State != orchapi.ApproverActive {
		return nil, false
	}
	person, err := s.devicePerson(ctx, a)
	if err != nil || person == nil {
		return nil, false
	}
	return person, true
}

// handlePushKey is GET /v0/approvals/push-key: the key a paired device's
// browser subscribes to phone alerts with (SPEC 16 "Phone alerts").
func (s *Service) handlePushKey(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if _, err := s.activeApprover(r.Context(), rm); err != nil {
		api.WriteError(w, err)
		return
	}
	if orchcore.DevicePushOf(s.d) == nil {
		api.WriteError(w, api.Errorf(api.CodeNotImplemented, "phone alerts are not available on this daemon"))
		return
	}
	key, err := orchcore.DevicePushOf(s.d).PushPublicKey(r.Context())
	respond(w, map[string]string{"public_key": key}, err)
}

// handlePush is POST /v0/approvals/push (subscribe) and /push/off: a paired
// device's phone alerts, for its person, while it stays an active approver.
func (s *Service) handlePush(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var req orchapi.DevicePushRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	me, err := s.activeApprover(ctx, rm)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if orchcore.DevicePushOf(s.d) == nil {
		api.WriteError(w, api.Errorf(api.CodeNotImplemented, "phone alerts are not available on this daemon"))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/off") {
		respond(w, map[string]bool{"on": false}, orchcore.DevicePushOf(s.d).UnsubscribeDevice(ctx, rm.NodeID, req.Endpoint))
		return
	}
	person, err := s.devicePerson(ctx, me)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, map[string]bool{"on": true}, orchcore.DevicePushOf(s.d).SubscribeDevice(ctx, person, rm.NodeID, req))
}

// handleGotIt is POST /v0/approvals/got-it: the device's Got it on a for-you
// mail (SPEC 9 "For you"). An active approver's device is its person's
// (zm approver add --for, else the machine owner's), so it marks read only
// mail addressed to that person.
func (s *Service) handleGotIt(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var req orchapi.GotItRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	me, err := s.activeApprover(ctx, rm)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	person, err := s.devicePerson(ctx, me)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var a orchapi.AckResponse
	err = api.NotFound("mail", req.ID)
	if fy, ok := orchcore.Mail(s.d).(orchcore.ForYouMail); ok {
		a, err = fy.GotIt(ctx, person, req.ID)
	}
	if tf, ok := orchcore.Team(s.d).(orchcore.TeamForYou); ok && api.IsCode(err, api.CodeNotFound) {
		// Not here: a joined project's, at its authority.
		a, err = tf.TeamGotIt(ctx, person, req.ID)
	}
	respond(w, a, err)
}

// newestFirst is ms newest first, at most limit.
func newestFirst(ms []orchapi.Mail, limit int) []orchapi.Mail {
	sort.SliceStable(ms, func(i, j int) bool {
		if !ms[i].Created.Equal(ms[j].Created) {
			return ms[i].Created.After(ms[j].Created)
		}
		return ms[i].ID > ms[j].ID
	})
	if len(ms) > limit {
		ms = ms[:limit]
	}
	return ms
}

func (s *Service) handleDevice(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.View(r.Context(), rm)
	respond(w, v, err)
}

type pageData struct {
	Nonce   string
	Machine string
	View    orchapi.DeviceView
	Code    string
	// PageURL is this page's own address, and QR it as an inline SVG, for a
	// device that is not a phone to hand the page to one.
	PageURL string
	QR      template.HTML
}

func (s *Service) handlePage(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if u := s.httpsURL(r); u != "" {
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	v, err := s.View(r.Context(), rm)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	data := pageData{Nonce: base64.StdEncoding.EncodeToString(nb), Machine: s.d.Config.Machine, View: v,
		Code: normCode(r.URL.Query().Get("code"))}
	if len(data.Code) > 16 || strings.ContainsFunc(data.Code, func(r rune) bool { return !strings.ContainsRune(codeAlphabet, r) }) {
		data.Code = ""
	}
	if v.Approver == nil && !v.OneTap && data.Code == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		data.PageURL = scheme + "://" + r.Host + "/v0/approvals"
		if q, err := QR(data.PageURL); err == nil {
			data.QR = template.HTML(q.SVG()) // our own markup: rects only
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+data.Nonce+
		"' 'self'; worker-src 'self'; connect-src 'self'; manifest-src 'self'; img-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(http.StatusOK)
	if err := page.Execute(w, data); err != nil {
		s.d.Log.Warn("approvals: page", "err", err)
	}
}

// page is the one screen a device sees: pair, wait for confirmation, or
// decide. Every value is escaped by html/template; mail bodies are data.
var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Decisions</title>
<link rel="manifest" href="/v0/approvals/manifest.webmanifest"><link rel="apple-touch-icon" href="/v0/approvals/icon.png">
<meta name="apple-mobile-web-app-capable" content="yes"><meta name="mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-title" content="Decisions"><meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<meta name="theme-color" content="#151515">
<style>
:root{color-scheme:light dark;--fg:#1b1b1b;--bg:#fafaf7;--muted:#6b6b6b;--line:#ddd;--ok:#1d7a3a;--no:#a3261c}
@media (prefers-color-scheme:dark){:root{--fg:#ececec;--bg:#151515;--muted:#9a9a9a;--line:#333;--ok:#4cc072;--no:#ef6b5f}}
body{font:16px/1.45 system-ui,-apple-system,sans-serif;color:var(--fg);background:var(--bg);margin:0 auto;max-width:640px;padding:16px}
h1{font-size:1.2rem;margin:0 0 4px}.muted{color:var(--muted);font-size:.9rem}
.card{border:1px solid var(--line);border-radius:10px;padding:12px;margin:12px 0}
.subj{font-family:ui-monospace,monospace;font-size:.85rem;word-break:break-all}.body{white-space:pre-wrap;margin:8px 0}
button{font:inherit;padding:10px 18px;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg);margin-right:8px}
button.approve{border-color:var(--ok);color:var(--ok)}button.decline{border-color:var(--no);color:var(--no)}
input{font:inherit;padding:10px;border-radius:8px;border:1px solid var(--line);width:12ch;letter-spacing:.1em;text-transform:uppercase}
input[type=radio]{width:auto}h2{font-size:1.05rem;margin:6px 0}.choice{margin:8px 0}.choice button{text-align:left;width:100%;margin:0}
textarea{font:inherit;width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg);margin:6px 0}
#msg{color:var(--no);min-height:1.4em}
body{padding:max(16px,env(safe-area-inset-top)) 16px max(16px,env(safe-area-inset-bottom))}
h3{font-size:.95rem;margin:18px 0 4px;color:var(--muted)}.row{margin-top:8px}.row button{margin-bottom:6px}
.tag{font-size:.75rem;margin-left:6px;padding:1px 6px;border:1px solid var(--ok);border-radius:6px}
.choice-btn.rec{border-width:2px}.done{opacity:.8}details summary{color:var(--muted);margin:12px 0}.fold>summary{font-weight:600}.fold.sub{margin-left:12px}
label{display:block;margin:4px 0}input[type=checkbox]{width:auto}
.qr{display:block;width:min(72vw,260px);height:auto;margin:12px 0;border-radius:8px}
</style></head><body>
<h1>Decisions</h1>
<div class="muted" id="who">this device: {{.View.NodeName}}{{with .View.Approver}} · approver <b>{{.Name}}</b> ({{.State}}){{end}}</div>
<p id="msg" role="status"></p>
{{- if not .View.Approver}}
{{- if .Code}}
<div class="card"><p>Approve decisions from this device.</p><input id="code" type="hidden" value="{{.Code}}"><button id="pair" class="approve">Use this device</button></div>
{{- else if .View.OneTap}}
<div class="card"><p>Approve decisions from this phone.</p><button id="pairphone" class="approve">Use this phone</button></div>
{{- else if .View.OneTapWhy}}
<div class="card"><p>This phone can't approve here yet.</p><p class="muted">{{.View.OneTapWhy}}</p></div>
{{- else}}
<div class="card"><p>Scan this with your phone's camera, then tap <b>Use this phone</b>.</p>{{.QR}}
<p class="muted subj">{{.PageURL}}</p><p class="muted">Your phone needs the Tailscale app, signed in with the account this zm uses.</p></div>
{{- end}}
{{- else if eq .View.Approver.State "unconfirmed"}}
<div class="card"><p>Paired. An existing approver must confirm this device on theirs before it can decide.</p>
{{- with .View.Approver.Note}}<p id="stuck"><b>Nobody can confirm it right now.</b> {{.}}</p>{{end}}</div>
{{- else if eq .View.Approver.State "active"}}
{{- range .View.Confirm}}
<div class="card"><p>Confirm new approver <b>{{.Name}}</b>{{with .Identity}}, {{.}}'s phone,{{end}} {{if .Key}}with a key{{else}}on device <b>{{.NodeName}}</b>{{with .Login}} ({{.}}){{end}}{{end}}?</p>
{{- with .Key}}<p><b>This is a key on a machine that may run agents.</b> Confirm only if the device you are pairing shows this same fingerprint: <code class="subj">{{.}}</code></p>{{end}}
<p class="muted">Not expected? Do not confirm; remove it on the Mac: zm approver rm {{.Name}}</p>
<button class="confirm" data-name="{{.Name}}" data-node="{{.Node}}">Confirm {{.Name}}</button></div>
{{- end}}
<main id="decisions"><p class="muted">Loading…</p></main>
<script type="module" src="/v0/approvals/app.js"></script>
{{- end}}
<script nonce="{{.Nonce}}">
const msg = document.getElementById('msg');
async function post(path, body) {
  msg.textContent = '';
  try {
    const r = await fetch(path, {method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json', 'Potluck-Protocol': 'potluck/1'}, body: JSON.stringify(body)});
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { msg.textContent = (j.error && j.error.message) || r.statusText; return; }
    if (j.mail_error) { msg.textContent = j.mail_error; return; }
    location.replace('/v0/approvals');
  } catch (e) { msg.textContent = String(e); }
}
const pair = document.getElementById('pair');
if (pair) pair.addEventListener('click', () => post('/v0/approvals/pair', {code: document.getElementById('code').value}));
const pairPhone = document.getElementById('pairphone');
if (pairPhone) pairPhone.addEventListener('click', () => { pairPhone.disabled = true; post('/v0/approvals/pair-phone', {}); });
for (const b of document.querySelectorAll('button.decide')) {
  b.addEventListener('click', () => {
    const body = {decision: b.dataset.decision};
    const t = document.getElementById('text-' + b.dataset.ask);
    if (t && t.value.trim()) body.text = t.value.trim();
    const as = document.querySelector('input[name="as-' + b.dataset.ask + '"]:checked');
    if (as) body.as = as.value;
    for (const x of document.querySelectorAll('button')) x.disabled = true;
    post('/v0/asks/' + encodeURIComponent(b.dataset.ask) + '/decide', body); });
}
for (const b of document.querySelectorAll('button.confirm')) {
  b.addEventListener('click', () => post('/v0/approvers/' + encodeURIComponent(b.dataset.name) + '/confirm', {node: b.dataset.node}));
}
</script></body></html>
`))
