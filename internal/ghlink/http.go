package ghlink

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink/gate"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

// MaxPayload bounds a GitHub delivery.
const MaxPayload = 5 << 20

// Routes registers /v0/github.
func (s *Service) Routes(r *core.Router) {
	r = r.For("ghlink")
	s.browse.Routes(r)
	r.HandleFunc("GET /v0/github/repo-list", core.Authed, "read: explicit whole-project read; hidden, area-only and missing match", s.h(s.handleRepoList))
	r.HandleFunc("PUT /v0/github/repo-list", core.Authed, "admin: explicit whole-project admin (from a member, at the authority); hidden and missing match", s.h(s.handleSetRepoList))
	r.HandleFunc("GET /v0/github/links", core.Local, "machine owner only: every project's GitHub link", s.h(s.handleList))
	r.HandleFunc("POST /v0/github/links", core.Local, "machine owner only: create or replace a project's link (runs gh, mints the linker's token)", s.h(s.handlePut))
	r.HandleFunc("DELETE /v0/github/links/{project}", core.Local, "machine owner only; revokes the linker's token and grant", s.h(s.handleDelete))
	r.HandleFunc("POST /v0/github/links/{project}/sync", core.Local, "machine owner only: poll now and apply the inbox", s.h(s.handleSync))
	r.HandleFunc("POST "+ReceiverPath, core.Public, "none: X-Hub-Signature-256 must verify with a link's secret, else 401 whatever repo the body names; answers nothing scoped", s.handleHook)
}

func (s *Service) h(fn func(r *http.Request, sub *authz.Subject) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r, core.SubjectOf(r.Context()))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func (s *Service) handleList(r *http.Request, _ *authz.Subject) (any, error) {
	ls, err := s.links(r.Context())
	if err != nil {
		return nil, err
	}
	out := orchapi.GitHubLinkList{Links: []orchapi.GitHubLink{}}
	for _, l := range ls {
		out.Links = append(out.Links, s.view(r.Context(), l))
	}
	return out, nil
}

var (
	sigRe    = regexp.MustCompile(`^sha256=[0-9a-f]{64}$`)
	prefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._]{0,31}$`)
	sourceRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

func pick(v, def string, allowed ...string) (string, error) {
	if v == "" {
		return def, nil
	}
	if !slices.Contains(allowed, v) {
		return "", api.Errorf(api.CodeBadRequest, "bad transition %q (want one of %s)", v, strings.Join(allowed, ", "))
	}
	return v, nil
}

func (s *Service) handlePut(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	var req orchapi.GitHubLinkRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	p, err := s.d.Projects.Get(sub.Scope, strings.TrimSpace(req.Project))
	if err != nil {
		return nil, err
	}
	if !s.hosted(p.ID) {
		// Linking would mint a token for a link that does nothing here.
		return nil, api.Errorf(api.CodeConflict, "this machine no longer hosts %s: its authority is elsewhere", p.Slug).
			WithHint("link it on the machine that hosts it (zm github link on that machine)")
	}
	l := link{Project: p.ID, Bin: strings.TrimSpace(req.Bin), Host: strings.TrimSpace(req.Host), TokenFile: strings.TrimSpace(req.TokenFile),
		Cursors: map[string]string{}, Created: store.Millis(s.now())}
	for _, raw := range req.Repos {
		repo, ok := NormRepo(raw)
		if !ok {
			return nil, api.Errorf(api.CodeBadRequest, "bad repo %q (want owner/name)", raw)
		}
		if !slices.Contains(l.Repos, repo) {
			l.Repos = append(l.Repos, repo)
		}
	}
	if len(l.Repos) == 0 {
		return nil, api.Errorf(api.CodeBadRequest, "a GitHub link needs at least one --repo owner/name")
	}
	for _, x := range req.Prefixes {
		if !prefixRe.MatchString(x) {
			return nil, api.Errorf(api.CodeBadRequest, "bad id prefix %q", x)
		}
		l.Prefixes = append(l.Prefixes, strings.ToLower(x))
	}
	for _, x := range req.Sources {
		if !sourceRe.MatchString(x) || x == "human" || x == "machine" {
			return nil, api.Errorf(api.CodeBadRequest, "bad source %q", x)
		}
		l.Sources = append(l.Sources, x)
	}
	if l.OnOpen, err = pick(req.OnOpen, string(orchapi.WorkClaimed), string(orchapi.WorkClaimed), string(orchapi.WorkInReview), "none"); err != nil {
		return nil, err
	}
	if l.OnMerge, err = pick(req.OnMerge, string(orchapi.WorkDone), string(orchapi.WorkDone), string(orchapi.WorkInReview), "none"); err != nil {
		return nil, err
	}
	if l.OnClose, err = pick(req.OnClose, string(orchapi.WorkOpen), string(orchapi.WorkOpen), "none"); err != nil {
		return nil, err
	}
	if req.PollSeconds < 0 || (req.PollSeconds > 0 && time.Duration(req.PollSeconds)*time.Second < s.MinPoll) {
		return nil, api.Errorf(api.CodeBadRequest, "poll interval must be 0 (receiver only) or at least %s", s.MinPoll)
	}
	l.PollSeconds = req.PollSeconds
	if l.TokenFile != "" {
		if _, err := secrets.Get(ctx, secrets.File(l.TokenFile)); err != nil {
			return nil, secrets.Recode(err, api.CodeBadRequest)
		}
	}
	old, oerr := s.linkRow(ctx, p.ID)
	exists := oerr == nil
	// --match and --gate keep the link's current values when left out (like
	// the secret): re-running link to change --poll must never drop a gate,
	// which would flush every held decision without its evidence.
	// "all" and "none" clear them.
	switch {
	case len(req.Match) == 0 && exists:
		l.Match = old.Match
	case len(req.Match) == 1 && strings.EqualFold(strings.TrimSpace(req.Match[0]), "all"):
		l.Match = nil
	default:
		for _, m := range req.Match {
			switch m = strings.ToLower(strings.TrimSpace(m)); m {
			case MatchTitle, MatchBranch, MatchBody:
				if !slices.Contains(l.Match, m) {
					l.Match = append(l.Match, m)
				}
			default:
				return nil, api.Errorf(api.CodeBadRequest, "bad --match %q (want title, branch, body, or all)", m)
			}
		}
	}
	switch g := strings.TrimSpace(req.Gate); {
	case g == "" && exists:
		l.Gate = old.Gate
	case g == "" || g == "none":
	default:
		// A gate is code the owner trusts: an absolute path to an executable
		// file that answers describe (SPEC 18.3).
		if !filepath.IsAbs(g) {
			return nil, api.Errorf(api.CodeBadRequest, "--gate wants an absolute path, got %q", g)
		}
		fi, err := os.Stat(g)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return nil, api.Errorf(api.CodeBadRequest, "--gate %s is not an executable file", g)
		}
		l.Gate = filepath.Clean(g)
		probe := &gate.Process{Path: l.Gate, Env: s.gateEnv(l)}
		if _, err := probe.Describe(ctx); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "--gate %s: %s", g, errText(err))
		}
	}
	made := ""
	switch {
	case req.Secret != "":
		if len(req.Secret) < 16 || len(req.Secret) > 512 {
			return nil, api.Errorf(api.CodeBadRequest, "secret must be 16 to 512 bytes")
		}
		l.Secret = req.Secret
	case exists && old.Secret != "":
		l.Secret = old.Secret
	default: // new, or restored from an off-site set without its secret: a fresh one, printed
		made = newSecret()
		l.Secret = made
	}
	if exists {
		l.Created, l.LastPoll, l.LastEvent = old.Created, old.LastPoll, old.LastEvent
		for _, repo := range l.Repos { // keep the poll cursors of repos still linked
			for _, k := range []string{repo, "ci:" + repo} {
				if c, ok := old.Cursors[k]; ok {
					l.Cursors[k] = c
				}
			}
		}
	}
	// The linker's identity and token: member of this project only, so its
	// item writes pass the work module's authz like anyone's.
	l.TokenID = old.TokenID
	if exists {
		// Repair a linker whose grant was revoked by hand (idempotent).
		if _, err := s.d.Projects.Grant(ctx, sub, api.GrantRequest{Identity: Identity, Role: api.RoleMember, Project: p.ID}); err != nil {
			return nil, err
		}
	}
	if !exists || !s.tokenLive(ctx, p.ID, old.TokenID) {
		tok, err := s.d.Projects.Mint(ctx, sub, api.MintTokenRequest{As: Identity, Project: p.ID, Role: api.RoleMember,
			Note: "zm github link " + p.Slug})
		if err != nil {
			return nil, err
		}
		if err := writeSecretFile(s.tokenPath(p.ID), tok.Secret); err != nil {
			_, _ = s.d.Projects.RevokeToken(ctx, tok.Token.ID)
			return nil, err
		}
		if exists && old.TokenID != "" {
			_, _ = s.d.Projects.RevokeToken(ctx, old.TokenID)
		}
		l.TokenID = tok.Token.ID
	}
	repos, _ := json.Marshal(l.Repos)
	prefixes, _ := json.Marshal(nonNil(l.Prefixes))
	sources, _ := json.Marshal(nonNil(l.Sources))
	cursors, _ := json.Marshal(l.Cursors)
	match, _ := json.Marshal(nonNil(l.Match))
	if _, err := s.db().ExecContext(ctx, `INSERT INTO ghlink_links (project, repos, prefixes, sources, on_open, on_merge, on_close,
		poll_seconds, bin, host, token_file, secret, token_id, cursors, created, last_poll, last_event, gate, match)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET repos = excluded.repos, prefixes = excluded.prefixes, sources = excluded.sources,
		on_open = excluded.on_open, on_merge = excluded.on_merge, on_close = excluded.on_close, poll_seconds = excluded.poll_seconds,
		bin = excluded.bin, host = excluded.host, token_file = excluded.token_file, secret = excluded.secret,
		token_id = excluded.token_id, cursors = excluded.cursors, last_error = '', gate = excluded.gate, match = excluded.match`,
		l.Project, string(repos), string(prefixes), string(sources), l.OnOpen, l.OnMerge, l.OnClose, l.PollSeconds, l.Bin, l.Host,
		l.TokenFile, l.Secret, l.TokenID, string(cursors), l.Created, l.LastPoll, l.LastEvent, l.Gate, string(match)); err != nil {
		return nil, err
	}
	if exists && old.Gate != "" && l.Gate == "" {
		// The gate's held decisions are its own: without it they would apply
		// with no evidence. They are dropped (the PRs' next snapshots are
		// decided by 18.2 alone, as the owner asked).
		s.applyMu.Lock()
		res, err := s.db().ExecContext(ctx, `DELETE FROM ghlink_waits WHERE project = ?`, l.Project)
		s.applyMu.Unlock()
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.d.Log.Warn("ghlink: gate removed; its held decisions dropped", "project", l.Project, "waits", n)
		}
	}
	s.startPoller(l)
	return orchapi.GitHubLinkResponse{Link: s.view(ctx, l), Secret: made}, nil
}

// tokenLive reports whether the linker's token exists, is not revoked and
// its secret file is in place.
func (s *Service) tokenLive(ctx context.Context, project, id string) bool {
	if id == "" {
		return false
	}
	if tok, err := s.token(project); err != nil || tok == "" {
		return false
	}
	var rev, exp *int64
	if err := s.db().QueryRowContext(ctx, `SELECT revoked, expires FROM tokens WHERE id = ?`, id).Scan(&rev, &exp); err != nil {
		return false
	}
	return rev == nil && (exp == nil || *exp > store.Millis(s.now()))
}

// linkOf is ref's link. raw also finds the row of a project not hosted here
// (delete is the cleanup path), by the registry or, once the registry no
// longer knows it, by the ULID itself: the route is machine-owner only.
func (s *Service) linkOf(ctx context.Context, sub *authz.Subject, ref string, raw bool) (link, error) {
	id := ""
	p, err := s.d.Projects.Get(sub.Scope, ref)
	switch {
	case err == nil:
		id = p.ID
	case raw && api.IsCode(err, api.CodeNotFound) && ids.Valid(strings.ToUpper(ref)):
		id = strings.ToUpper(ref)
	default:
		return link{}, err
	}
	get := s.link
	if raw {
		get = s.linkRow
	}
	l, err := get(ctx, id)
	if err != nil {
		return l, api.NotFound("github link", ref)
	}
	return l, nil
}

func (s *Service) handleDelete(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	l, err := s.linkOf(ctx, sub, r.PathValue("project"), true)
	if err != nil {
		return nil, err
	}
	return s.unlink(ctx, sub, l)
}

// unlink revokes the linker's token and grant and deletes the link with its
// inbox, PR records and held decisions: DELETE /v0/github/links/{project},
// and the cleanup of a link whose project moved off this machine.
func (s *Service) unlink(ctx context.Context, sub *authz.Subject, l link) (orchapi.GitHubLink, error) {
	s.stopPoller(l.Project)
	v := s.view(ctx, l)
	if l.TokenID != "" {
		_, _ = s.d.Projects.RevokeToken(ctx, l.TokenID)
	}
	_, _ = s.d.Projects.Revoke(ctx, sub, api.GrantRequest{Identity: Identity, Role: api.RoleMember, Project: l.Project})
	_ = os.Remove(s.tokenPath(l.Project))
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	for _, t := range []string{"ghlink_links", "ghlink_inbox", "ghlink_prs", "ghlink_items", "ghlink_deliveries", "ghlink_waits", "ghlink_pr_events"} {
		if _, err := s.db().ExecContext(ctx, `DELETE FROM `+t+` WHERE project = ?`, l.Project); err != nil {
			return v, err
		}
	}
	return v, nil
}

// reap deletes (and revokes) project's link when this machine holds a replica
// of it: its authority is elsewhere, so the row must not come back to life
// when the replica goes away (zm leave, a move back). Never under applyMu.
func (s *Service) reap(ctx context.Context, project string) {
	if !s.replicated(project) {
		return
	}
	l, err := s.linkRow(ctx, project)
	if err != nil || s.d.Projects == nil {
		return
	}
	// unlink stops this project's poller, which may be the caller: it must
	// not cancel the cleanup under it.
	if _, err := s.unlink(context.WithoutCancel(ctx), s.d.Projects.Local(""), l); err != nil {
		s.d.Log.Warn("ghlink: cleaning up the link of a moved project failed", "project", project, "err", err)
		return
	}
	s.d.Log.Info("ghlink: link of a project that moved off this machine removed", "project", project)
}

func (s *Service) handleSync(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	l, err := s.linkOf(ctx, sub, r.PathValue("project"), false)
	if err != nil {
		return nil, err
	}
	var out orchapi.GitHubSyncResponse
	n, err := s.poll(ctx, l)
	out.PRs = n
	if err != nil {
		out.Errors = append(out.Errors, "poll: "+errText(err))
		s.setError(ctx, l.Project, "poll: "+errText(err))
	}
	applied, errs := s.drain(ctx)
	out.Applied, out.Errors = applied, append(out.Errors, errs...)
	return out, nil
}

// ---- the receiver ----

type hookPayload struct {
	Action      string `json:"action"`
	PullRequest ghPull `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handleHook is GitHub's webhook (content type application/json). The
// signature is checked against every link's secret before the body is
// parsed; one that matches none, an unknown repo included, is 401. A
// delivery id seen before is dropped; a replayed older snapshot changes
// nothing (snapshots apply in updated_at order).
func (s *Service) handleHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bad := api.Errorf(api.CodeUnauthenticated, "signature does not match").
		WithHint("set the webhook's secret to the one zm github link printed; content type application/json")
	// Cheap refusals first: no well-formed signature, nothing to verify against.
	sig := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if !sigRe.MatchString(sig) {
		api.WriteError(w, bad)
		return
	}
	ls, err := s.links(ctx)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if len(ls) == 0 {
		api.WriteError(w, bad)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxPayload+1))
	if err != nil || len(body) > MaxPayload {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "payload too large or unreadable"))
		return
	}
	var ok []link
	for _, l := range ls {
		if verifyHub([]byte(l.Secret), sig, body) {
			ok = append(ok, l)
		}
	}
	if len(ok) == 0 {
		api.WriteError(w, bad)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		api.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case "pull_request":
	default:
		api.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true, "ignored": true})
		return
	}
	var pl hookPayload
	if err := json.Unmarshal(body, &pl); err != nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "not a pull_request payload"))
		return
	}
	p, valid := pl.PullRequest.snapshot(pl.Repository.FullName)
	delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if len(delivery) > 100 {
		delivery = delivery[:100]
	}
	now := s.now()
	for _, l := range ok {
		if !valid || !hasRepo(l, p.Repo) {
			continue
		}
		// The delivery id and the snapshot commit together: a failed enqueue
		// leaves GitHub's redelivery welcome.
		err := store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
			if delivery != "" {
				res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghlink_deliveries (project, delivery, time) VALUES (?, ?, ?)`,
					l.Project, delivery, store.Millis(now))
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					return nil // a redelivery
				}
			}
			return s.enqueueTx(ctx, tx, l.Project, []PR{p})
		})
		if err != nil {
			api.WriteError(w, err)
			return
		}
		s.browse.Invalidate(l.Project, p.Repo)
		s.poke()
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func verifyHub(secret []byte, sig string, body []byte) bool {
	if len(secret) == 0 {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(strings.TrimSpace(sig)), []byte(want))
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "ghsec_" + base64.RawURLEncoding.EncodeToString(b)
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
