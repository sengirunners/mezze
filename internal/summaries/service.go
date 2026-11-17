// Package summaries builds bounded deterministic project digests over orchcore.WorkReader.
// SQLite is the durable driver; a remote archive driver can implement Repository later.
// No model, credentials or user text leaves the daemon. Go's standard library is
// sufficient for this small scoped derivation and calendar scheduler.
package summaries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// Migrations are this module's tables; its New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "summaries/0001", Script: `
CREATE TABLE summary_configs(project TEXT PRIMARY KEY, config TEXT NOT NULL);
CREATE TABLE project_summaries(id TEXT PRIMARY KEY, project TEXT NOT NULL, day TEXT NOT NULL, version TEXT NOT NULL, generated INTEGER NOT NULL, data TEXT NOT NULL, UNIQUE(project,day,version));
CREATE INDEX project_summaries_history ON project_summaries(project,day DESC,id DESC);
`},
}

// Repository is the archive seam. SQLite is the built-in durable implementation.
type Repository interface {
	Get(context.Context, string, string, string) (orchapi.ProjectSummary, error)
	Put(context.Context, orchapi.ProjectSummary) (orchapi.ProjectSummary, error)
}

type Service struct {
	d      *core.Deps
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	now    func() time.Time
}

func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: time.Now}
	d.AddScrubber(s.scrub)
	return s
}

func (s *Service) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			s.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (s *Service) Close() error {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	return nil
}

func (s *Service) tick(ctx context.Context) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT config FROM summary_configs ORDER BY project`)
	if err != nil {
		return
	}
	var configs []orchapi.SummaryConfig
	for rows.Next() {
		var raw string
		var c orchapi.SummaryConfig
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &c) == nil && c.Enabled {
			configs = append(configs, c)
		}
	}
	rows.Close()
	for _, c := range configs {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			continue
		}
		now := s.now().In(loc)
		if now.Hour()*60+now.Minute() < c.Hour*60+c.Minute {
			continue
		}
		day := now.AddDate(0, 0, -1).Format(time.DateOnly)
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, runErr := s.generate(bounded, s.d.System(), c, day)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if runErr == nil && c.LastGenerated != nil && c.LastGenerated.Equal(result.Generated) && c.LastError == "" {
			continue
		}
		previousError := c.LastError
		attempt := s.now().UTC()
		c.LastAttempt = &attempt
		c.LastError = ""
		if runErr != nil {
			c.LastError = "The scheduled work digest could not be generated; it will retry while this daemon runs."
		} else {
			c.LastGenerated = &result.Generated
		}
		raw, _ := json.Marshal(c)
		// A concurrent owner edit wins over this old run's status.
		updateResult, statusErr := s.d.Store.Registry().ExecContext(ctx, `UPDATE summary_configs SET config=? WHERE project=? AND json_extract(config,'$.version')=?`, string(raw), c.Project, c.Version)
		if statusErr == nil && s.d.Events != nil && (runErr == nil || c.LastError != previousError) {
			if n, _ := updateResult.RowsAffected(); n > 0 {
				_, _ = s.d.Events.Publish(ctx, events.Input{Type: orchapi.EventSummaryScheduleChanged, Project: c.Project, Data: orchapi.ProjectHint{Project: c.Project}})
			}
		}
	}
}

func (s *Service) Get(ctx context.Context, project, day, version string) (orchapi.ProjectSummary, error) {
	var raw string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT data FROM project_summaries WHERE project=? AND day=? AND version=?`, project, day, version).Scan(&raw)
	var out orchapi.ProjectSummary
	if err == nil {
		err = json.Unmarshal([]byte(raw), &out)
	}
	return out, err
}

func (s *Service) Put(ctx context.Context, out orchapi.ProjectSummary) (orchapi.ProjectSummary, error) {
	return s.putArchive(ctx, out, nil)
}

func (s *Service) putArchive(ctx context.Context, out orchapi.ProjectSummary, bindings []titleBinding) (orchapi.ProjectSummary, error) {
	raw, err := json.Marshal(archivedSummary{ProjectSummary: out, Bindings: bindings})
	if err != nil {
		return out, err
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO project_summaries(id,project,day,version,generated,data) VALUES(?,?,?,?,?,?) ON CONFLICT(project,day,version) DO NOTHING`, out.ID, out.Project, out.Day, out.ConfigVersion, store.Millis(out.Generated), string(raw))
	if err != nil {
		return out, err
	}
	if _, err = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM project_summaries WHERE project=? AND id NOT IN (SELECT id FROM project_summaries WHERE project=? ORDER BY generated DESC,id DESC LIMIT 400)`, out.Project, out.Project); err != nil {
		return out, err
	}
	return s.Get(ctx, out.Project, out.Day, out.ConfigVersion)
}

func (s *Service) config(ctx context.Context, project string) (orchapi.SummaryConfig, error) {
	out := orchapi.SummaryConfig{Project: project, Hour: 7, Timezone: "UTC"}
	var raw string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT config FROM summary_configs WHERE project=?`, project).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		out.Version = configVersion(out)
		return out, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &out)
	}
	if err == nil {
		out.Project = project
		err = validateConfig(&out)
	}
	return out, err
}

func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id,data FROM project_summaries WHERE project=?`, sc.Project)
	if err != nil {
		return err
	}
	type record struct{ id, raw string }
	var records []record
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.raw); err != nil {
			break
		}
		records = append(records, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range records {
		var v archivedSummary
		if err = json.Unmarshal([]byte(r.raw), &v); err != nil {
			return err
		}
		scrubArchived(&v, sc)
		js, _ := json.Marshal(v)
		if _, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE project_summaries SET data=? WHERE id=? AND project=?`, string(js), r.id, sc.Project); err != nil {
			return err
		}
	}
	return nil
}

var _ core.Module = (*Service)(nil)
var _ Repository = (*Service)(nil)
