package issues

// The bd door's project-level reads and writes: memories (beads' kv rows
// under "kv.memory.", which bd prime injects) and bd sql (one read-only
// SELECT through bd-shaped views).

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
)

// MemoryPrefix is where beads keeps memories in its config table.
const MemoryPrefix = "kv.memory."

const (
	maxMemoryKey   = 245 // beads' config key is VARCHAR(255), "kv.memory." included
	maxMemoryValue = 1 << 20
	// SQLMaxRows caps one bd sql answer; SQLTimeout bounds its query.
	SQLMaxRows = 50000
	SQLTimeout = 30 * time.Second
)

// oneProject is the single project with a native store in sc (the bd door
// always names its project); none or several is an error.
func (s *Service) oneProject(ctx context.Context, sc authz.Scope) (located, error) {
	ls, err := s.readStores(ctx, sc)
	if err != nil {
		return located{}, s.storeErr(err, "")
	}
	switch len(ls) {
	case 1:
		return ls[0], nil
	case 0:
		return located{}, api.Errorf(api.CodeNotFound, "no project with native issues here")
	}
	return located{}, api.Errorf(api.CodeBadRequest, "several projects have native issues: name one (?project=)")
}

// wholeProject: memories and raw SQL are project-level, so only a reader of
// every area of the project sees them.
func wholeProject(l located, what string) error {
	if !l.vis.All {
		return api.Errorf(api.CodeForbidden, "%s covers the whole project; you read only some of its areas", what)
	}
	return nil
}

func (s *Service) memories(ctx context.Context, sc authz.Scope) (map[string]string, error) {
	l, err := s.oneProject(ctx, sc)
	if err != nil {
		return nil, err
	}
	if err := wholeProject(l, "memories"); err != nil {
		return nil, err
	}
	kv, err := l.ps.KV(ctx, MemoryPrefix)
	if err != nil {
		return nil, s.storeErr(err, "")
	}
	out := make(map[string]string, len(kv))
	for k, v := range kv {
		out[strings.TrimPrefix(k, MemoryPrefix)] = v
	}
	return out, nil
}

// memoryStore is the resolved project's store for a memory write: write on
// the project level and a role above guest.
func (s *Service) memoryStore(ctx context.Context, sub *authz.Subject, key string) (istore.ProjectStore, error) {
	t, err := s.d.WriteTarget(ctx, "", authz.Write)
	if err != nil {
		return nil, err
	}
	o := authz.Object{Project: t.Project.ID}
	if err := sub.Check(authz.Write, o, "memory", key); err != nil {
		return nil, err
	}
	if sub.Scope.Relation(o) == authz.Guest {
		return nil, api.Errorf(api.CodeForbidden, "guests are read-only on issues")
	}
	if !sub.Scope.WholeProject(t.Project.ID) {
		return nil, api.Errorf(api.CodeForbidden, "memories cover the whole project; you write only some of its areas")
	}
	// The key is looked at only once the caller may write here.
	if err := validMemoryKey(key); err != nil {
		return nil, err
	}
	ps, err := s.writeStore(ctx, t.Project)
	if err != nil {
		return nil, s.storeErr(err, "")
	}
	return ps, nil
}

func validMemoryKey(key string) error {
	if strings.TrimSpace(key) == "" || len(key) > maxMemoryKey {
		return api.Errorf(api.CodeBadRequest, "a memory key is 1 to %d bytes", maxMemoryKey)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return api.Errorf(api.CodeBadRequest, "a memory key has no control characters")
		}
	}
	return nil
}

// remember stores a memory; action is "remembered" or "updated".
func (s *Service) remember(ctx context.Context, sub *authz.Subject, key, value string) (orchapi.Memory, error) {
	ps, err := s.memoryStore(ctx, sub, key)
	if err != nil {
		return orchapi.Memory{}, err
	}
	if textclean.Blank(value) {
		return orchapi.Memory{}, api.Errorf(api.CodeBadRequest, "memory content cannot be empty")
	}
	if len(value) > maxMemoryValue {
		return orchapi.Memory{}, api.Errorf(api.CodeBadRequest, "a memory is at most 1 MiB")
	}
	value = textclean.Clean(value)
	old, err := ps.SetKV(ctx, MemoryPrefix+key, value)
	if err != nil {
		return orchapi.Memory{}, s.storeErr(err, "")
	}
	m := orchapi.Memory{Key: key, Value: value, Action: "remembered"}
	if old != "" {
		m.Action = "updated"
	}
	return m, nil
}

// forget deletes a memory: not_found when there was none.
func (s *Service) forget(ctx context.Context, sub *authz.Subject, key string) (orchapi.Memory, error) {
	ps, err := s.memoryStore(ctx, sub, key)
	if err != nil {
		return orchapi.Memory{}, err
	}
	old, err := ps.DeleteKV(ctx, MemoryPrefix+key)
	if err != nil {
		return orchapi.Memory{}, s.storeErr(err, "")
	}
	if old == "" {
		return orchapi.Memory{}, api.NotFound("memory", key)
	}
	return orchapi.Memory{Key: key, Value: old, Action: "deleted"}, nil
}

// readSQL runs one read-only SELECT over the project's bd-shaped views.
func (s *Service) readSQL(ctx context.Context, sub *authz.Subject, sc authz.Scope, query string) (orchapi.SQLResult, error) {
	l, err := s.oneProject(ctx, sc)
	if err != nil {
		return orchapi.SQLResult{}, err
	}
	if err := wholeProject(l, "bd sql"); err != nil {
		return orchapi.SQLResult{}, err
	}
	r, ok := l.ps.(istore.SQLReader)
	if !ok {
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "this issue store does not serve bd sql")
	}
	tctx, cancel := context.WithTimeout(ctx, SQLTimeout)
	defer cancel()
	res, err := r.ReadSQL(tctx, query, SQLMaxRows, func(id string) (bool, error) { return s.visibleEdge(tctx, sub, id) })
	switch {
	case ctx.Err() != nil:
		return orchapi.SQLResult{}, ctx.Err()
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "query ran past %s", SQLTimeout)
	case errors.Is(err, istore.ErrNotSelect):
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "bd sql under zm is read-only: one SELECT (or WITH) statement; writes go through bd's verbs")
	case errors.Is(err, istore.ErrTooManyRows):
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "more than %d rows: add a LIMIT or a narrower WHERE", SQLMaxRows)
	case errors.Is(err, istore.ErrTooLarge):
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "the answer is over 32 MiB: select fewer columns or rows")
	case err != nil:
		// The query's own error (syntax, unknown column) is the caller's to see.
		return orchapi.SQLResult{}, api.Errorf(api.CodeBadRequest, "query error: %v", err)
	}
	return orchapi.SQLResult{Columns: res.Columns, Rows: res.Rows}, nil
}
