package session

// Usage attribution (SPEC 8 "Usage"): who a session works for, its job kind
// and Claude account, recorded at spawn or first registration, and its token
// totals, read from its own transcript. The loop below tails each live
// claude or codex session's transcript from where it stopped, every
// UsageEvery and once when the session ends; a daemon restart reads each
// transcript whole once. Tokens are written straight to the row: no event,
// no `updated` bump (a reader or the next heartbeat picks them up).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/events"
)

// UsageEvery is how often a live session's tokens are refreshed (tests
// shorten it).
var UsageEvery = 3 * time.Minute

// Environment a spawner sets for the session it starts (SpawnRequest.For,
// JobKind); a managed child gets both, so what it starts inherits them.
const (
	envFor     = "ZM_FOR"
	envJobKind = "ZM_JOB_KIND"
)

// cleanFor is a For (or Account) value as stored: one clean line, bounded.
func cleanFor(v string) string {
	v = oneLine(textclean.Clean(v))
	if len(v) > orchapi.MaxForLen {
		v = v[:orchapi.MaxForLen]
	}
	return v
}

// attribution checks and cleans a request's for and job_kind.
func attribution(forID, jobKind string) (string, string, error) {
	jobKind = strings.ToLower(strings.TrimSpace(jobKind))
	if !orchapi.ValidJobKind(jobKind) {
		return "", "", api.Errorf(api.CodeBadRequest, "job_kind must be interactive, standing, farm or timer, not %q", jobKind)
	}
	return cleanFor(forID), jobKind, nil
}

// agentHomeEnv is the variable that moves an agent runtime's home.
func agentHomeEnv(agent string) string {
	switch agent {
	case "claude":
		return "CLAUDE_CONFIG_DIR"
	case "codex":
		return "CODEX_HOME"
	}
	return ""
}

// runtimeHome is where agent keeps its state for a session whose recorded
// home is home ("" = the default under the user's home; "" when unknown).
func (s *Service) runtimeHome(agent, home string) string {
	if home != "" {
		return home
	}
	uh := s.d.Config.UserHome
	if uh == "" {
		return ""
	}
	switch agent {
	case "claude":
		return filepath.Join(uh, ".claude")
	case "codex":
		return filepath.Join(uh, ".codex")
	}
	return ""
}

// claudeAccount is the login a Claude session started with: oauthAccount in
// the config Claude reads ($CLAUDE_CONFIG_DIR/.claude.json, else
// ~/.claude.json, which cswap rewrites on every switch).
func (s *Service) claudeAccount(home string) string {
	path := ""
	switch {
	case home != "":
		path = filepath.Join(home, ".claude.json")
	case s.d.Config.UserHome != "":
		path = filepath.Join(s.d.Config.UserHome, ".claude.json")
	default:
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		OAuth struct {
			Email string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return cleanFor(c.OAuth.Email)
}

// usageTail is how far a session's transcript files were read and what they
// added up to.
type usageTail struct {
	main    string           // the transcript; "" until found
	offsets map[string]int64 // file -> bytes read (whole lines)
	seen    map[string]bool  // claude: message id + request id counted
	prev    map[string]string
	sum     orchapi.SessionTokens
}

type usageTracker struct {
	mu    sync.Mutex // one read at a time
	tails map[string]*usageTail
	kick  chan struct{}
	endMu sync.Mutex
	ended map[string]bool // session8s that ended since the last pass
}

// usageEnded asks the loop for a session's final tokens.
func (s *Service) usageEnded(id8 string) {
	u := &s.usage
	u.endMu.Lock()
	if u.ended == nil {
		u.ended = map[string]bool{}
	}
	u.ended[id8] = true
	k := u.kick
	u.endMu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func (s *Service) usageLoop() {
	defer s.wg.Done()
	u := &s.usage
	u.endMu.Lock()
	u.kick = make(chan struct{}, 1)
	k := u.kick
	u.endMu.Unlock()
	t := time.NewTicker(UsageEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-k:
			s.usageFinals()
		case <-t.C:
			s.usageFinals()
			s.usagePass()
		}
	}
}

// usagePass refreshes every live local agent session's tokens.
func (s *Service) usagePass() {
	rows, err := listRows(s.ctx, s.d.Store.Registry(), `WHERE machine = ? AND state != ? AND runtime_id != '' AND agent IN ('claude', 'codex')`,
		s.d.Config.Machine, string(orchapi.StateEnded))
	if err != nil {
		s.d.Log.Warn("usage: list sessions", "err", err)
		return
	}
	live := map[string]bool{}
	for _, r := range rows {
		live[r.ID] = true
		s.refreshTokens(r, false)
	}
	s.usage.mu.Lock()
	for id := range s.usage.tails {
		if !live[id] {
			delete(s.usage.tails, id)
		}
	}
	s.usage.mu.Unlock()
}

// usageFinals reads the sessions that ended to their end, once.
func (s *Service) usageFinals() {
	u := &s.usage
	u.endMu.Lock()
	ids := u.ended
	u.ended = nil
	u.endMu.Unlock()
	for id := range ids {
		r, err := loadRow(s.ctx, s.d.Store.Registry(), id)
		if err != nil || r.RuntimeID == "" {
			continue
		}
		s.refreshTokens(r, true)
	}
}

// refreshTokens reads what r's transcript added since the last read and
// stores the total when it changed; final drops the tail afterwards.
func (s *Service) refreshTokens(r row, final bool) {
	home := s.runtimeHome(r.Agent, r.agentHome)
	if home == "" {
		return
	}
	u := &s.usage
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tails == nil {
		u.tails = map[string]*usageTail{}
	}
	t := u.tails[r.ID]
	if t == nil {
		t = &usageTail{offsets: map[string]int64{}, seen: map[string]bool{}, prev: map[string]string{}}
		u.tails[r.ID] = t
	}
	if final {
		delete(u.tails, r.ID)
	}
	if t.main == "" {
		t.main = findTranscript(r.Agent, home, r.RuntimeID, r.Started)
		if t.main == "" {
			return
		}
	}
	files := []string{t.main}
	if r.Agent == "claude" {
		files = append(files, claudeSubagentFiles(t.main)...)
	}
	for _, f := range files {
		t.read(r.Agent, f)
	}
	if t.sum == (orchapi.SessionTokens{}) || (r.Tokens != nil && *r.Tokens == t.sum) {
		return
	}
	sum := t.sum
	if _, err := s.d.Store.Registry().ExecContext(s.ctx, `UPDATE sessions SET tokens = ? WHERE id = ?`, tokensJSON(&sum), r.ID); err != nil {
		s.d.Log.Warn("usage: store tokens", "session", r.Address, "err", err)
	}
}

// findTranscript locates a runtime session's transcript: Claude's
// <home>/projects/*/<id>.jsonl, Codex's
// <home>/sessions/YYYY/MM/DD/rollout-*-<id>.jsonl around its start day.
func findTranscript(agent, home, rid string, started time.Time) string {
	if strings.ContainsAny(rid, `/\*?[`) {
		return ""
	}
	var pats []string
	switch agent {
	case "claude":
		pats = []string{filepath.Join(home, "projects", "*", rid+".jsonl")}
	case "codex":
		for _, d := range []int{0, -1, 1} {
			day := started.Local().AddDate(0, 0, d)
			pats = append(pats, filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"), "rollout-*-"+rid+".jsonl"))
		}
	}
	for _, p := range pats {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			sort.Strings(m)
			return m[0]
		}
	}
	return ""
}

// claudeSubagentFiles are a Claude transcript's subagent transcripts
// (<id>/subagents/*.jsonl beside it).
func claudeSubagentFiles(main string) []string {
	m, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "*.jsonl"))
	sort.Strings(m)
	return m
}

// read adds the whole lines appended to path since the last read.
func (t *usageTail) read(agent, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	off := t.offsets[path]
	if fi, err := f.Stat(); err != nil || fi.Size() <= off {
		return
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // a partial last line waits for its newline
		}
		off += int64(len(line))
		if !bytes.Contains(line, []byte(`usage`)) {
			continue
		}
		switch agent {
		case "claude":
			t.claudeLine(line)
		case "codex":
			t.codexLine(path, line)
		}
	}
	t.offsets[path] = off
}

// claudeLine counts an assistant message's usage once per message id and
// request id (Claude Code writes one line per content block, each with the
// message's usage).
func (t *usageTail) claudeLine(line []byte) {
	var l struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Message   struct {
			ID    string `json:"id"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil {
		return
	}
	if l.Message.ID != "" || l.RequestID != "" {
		key := l.Message.ID + "\x00" + l.RequestID
		if t.seen[key] {
			return
		}
		t.seen[key] = true
	}
	x := l.Message.Usage
	t.sum.Input += max(x.Input, 0)
	t.sum.CacheWrite += max(x.CacheWrite, 0)
	t.sum.CacheRead += max(x.CacheRead, 0)
	t.sum.Output += max(x.Output, 0)
}

// codexTokens is a Codex token_count usage (input includes the cached part).
type codexTokens struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

// codexLine adds a token_count event's last request. Codex re-emits the
// same counters (its cumulative total unchanged): that is no new request.
func (t *usageTail) codexLine(path string, line []byte) {
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Info *struct {
				Total *codexTokens `json:"total_token_usage"`
				Last  *codexTokens `json:"last_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "event_msg" || l.Payload.Type != "token_count" || l.Payload.Info == nil {
		return
	}
	in := l.Payload.Info
	if in.Last == nil || in.Last.Input+in.Last.Output <= 0 {
		return // a context-size report, not a request
	}
	if in.Total != nil {
		b, _ := json.Marshal(in.Total)
		if t.prev[path] == string(b) {
			return
		}
		t.prev[path] = string(b)
	}
	x := in.Last
	cached, write := max(x.Cached, 0), max(x.CacheWrite, 0)
	t.sum.Input += max(x.Input-cached-write, 0)
	t.sum.CacheRead += cached
	t.sum.CacheWrite += write
	t.sum.Output += max(x.Output, 0)
}

// workLoop keeps a local session's last work item (Session.Work) when it
// claims one on this machine's work module; a replica's claims through its
// authority arrive by RecordWork from the team module's heartbeat.
func (s *Service) workLoop() {
	defer s.wg.Done()
	if s.d.Events == nil {
		return
	}
	filter := events.Filter{Types: []string{orchapi.EventWorkClaimed}}
	evs, cancel := s.d.Events.Subscribe(s.d.System(), filter, 256)
	defer func() { cancel() }()
	for {
		select {
		case <-s.ctx.Done():
			return
		case e, ok := <-evs:
			if !ok { // fell behind: a missed claim keeps the older work
				evs, cancel = s.d.Events.Subscribe(s.d.System(), filter, 256)
				continue
			}
			var l orchapi.Lease
			if json.Unmarshal(e.Data, &l) == nil && l.Key != "" {
				s.RecordWork(s.ctx, l.Holder, l.Key)
			}
		}
	}
}

// RecordWork implements core.SessionWorkRecorder: address, a session on
// this machine, holds a lease on key, which becomes its work and stays
// after the release. A lock (lock:…) is not work.
func (s *Service) RecordWork(ctx context.Context, address, key string) {
	id8, ok := s.id8Of(address)
	key = oneLine(key)
	if !ok || !strings.Contains(address, "/") || key == "" || strings.HasPrefix(key, "lock:") {
		return
	}
	if _, err := s.update(ctx, id8, orchapi.EventSessionUpdated, func(r *row) (bool, error) {
		if r.Work == key {
			return false, nil
		}
		r.Work = key
		return true, nil
	}); err != nil && !errors.Is(err, errNoRow) {
		s.d.Log.Warn("session work", "session", address, "err", err)
	}
}
