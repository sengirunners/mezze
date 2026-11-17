package ghlink

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// PR lifecycle events (SPEC 18.2): every applied snapshot,
// from the receiver or the poller (one inbox, one worker), may publish
// dev.zm.github.pr.ready / .head_moved / .closed in the linked project's
// log; the CI pass publishes .checks_settled. Each is recorded once per
// (project, repo, number, head_sha, type[, CI outcome]) in ghlink_pr_events,
// so a poll after a webhook delivery, a retry or a replay never fires twice.
// ghlink_prs keeps the PR's lifecycle (ready, head, closed) to tell a ready
// from a head move.
const schemaPREvents = `
ALTER TABLE ghlink_prs ADD COLUMN ready INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ghlink_prs ADD COLUMN head TEXT NOT NULL DEFAULT '';
ALTER TABLE ghlink_prs ADD COLUMN closed INTEGER NOT NULL DEFAULT 0;
CREATE TABLE ghlink_pr_events (
	project  TEXT NOT NULL,
	repo     TEXT NOT NULL,
	number   INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	type     TEXT NOT NULL,
	result   TEXT NOT NULL DEFAULT '',
	time     INTEGER NOT NULL,
	PRIMARY KEY (project, repo, number, head_sha, type, result)
);`

var eventsMigrations = []store.Migration{{Kind: store.KindRegistry, Name: "ghlink/0006_pr_events", Script: schemaPREvents}}

// prLife is what the linker last saw of a PR's lifecycle.
type prLife struct {
	Ready  bool   // open and not a draft
	Head   string // head SHA last seen
	Closed bool   // merged or closed
}

// transitions is what snapshot p means after prev: the event types to
// publish (in order) and the new lifecycle. A draft is never ready; a PR
// becomes ready when it is first seen open and not a draft, when a draft is
// marked ready, or when it is reopened; a ready PR with a new head moved; a
// PR closes (merged or not) once until it is reopened.
func transitions(prev prLife, p PR) ([]string, prLife) {
	next := prev
	if p.HeadSHA != "" {
		next.Head = p.HeadSHA
	}
	switch p.State {
	case StateOpen:
		next.Closed = false
		if p.Draft {
			next.Ready = false
			return nil, next
		}
		next.Ready = true
		switch {
		case !prev.Ready || prev.Closed:
			return []string{orchapi.EventGitHubPRReady}, next
		case p.HeadSHA != "" && p.HeadSHA != prev.Head:
			return []string{orchapi.EventGitHubPRHeadMoved}, next
		}
	case StateMerged, StateClosed:
		next.Ready, next.Closed = false, true
		if !prev.Closed {
			return []string{orchapi.EventGitHubPRClosed}, next
		}
	}
	return nil, next
}

// lifecycle publishes what snapshot p means for project's PR and records
// the new lifecycle. The caller holds applyMu and has checked p is newer
// than the last snapshot applied.
func (s *Service) lifecycle(ctx context.Context, project string, p PR) error {
	var prev prLife
	err := s.db().QueryRowContext(ctx, `SELECT ready, head, closed FROM ghlink_prs WHERE project = ? AND repo = ? AND number = ?`,
		project, p.Repo, p.Number).Scan(&prev.Ready, &prev.Head, &prev.Closed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	types, next := transitions(prev, p)
	if len(types) > 0 {
		items, err := s.prItems(ctx, project, p.Repo, p.Number)
		if err != nil {
			return err
		}
		for _, typ := range types {
			ev := prEvent(p, items)
			if typ == orchapi.EventGitHubPRHeadMoved {
				ev.PrevHeadSHA = prev.Head
			}
			if err := s.publishPR(ctx, project, typ, "", ev); err != nil {
				return err
			}
		}
	}
	// updated stays as it was (0 for a new row): apply moves it once the
	// items are done, so a failed apply is retried in full.
	_, err = s.db().ExecContext(ctx, `INSERT INTO ghlink_prs (project, repo, number, updated, ready, head, closed) VALUES (?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT (project, repo, number) DO UPDATE SET ready = excluded.ready, head = excluded.head, closed = excluded.closed`,
		project, p.Repo, p.Number, next.Ready, next.Head, next.Closed)
	return err
}

func prEvent(p PR, items []string) orchapi.GitHubPREvent {
	if items == nil {
		items = []string{}
	}
	return orchapi.GitHubPREvent{Repo: p.Repo, Number: p.Number, HeadSHA: p.HeadSHA, Base: p.Base, Draft: p.Draft, Title: p.Title,
		Author: p.Author, Items: items, URL: p.URL, State: p.State}
}

// prItems are the work ids the linker has tied the PR to.
func (s *Service) prItems(ctx context.Context, project, repo string, number int) ([]string, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT item FROM ghlink_items WHERE project = ? AND repo = ? AND number = ? AND state != '' ORDER BY item`,
		project, repo, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var it string
		if err := rows.Scan(&it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// publishPR publishes one PR event unless it was published before for the
// same PR, head, type and result. The record is written first and taken
// back if the event cannot be stored, so a retry publishes it then.
func (s *Service) publishPR(ctx context.Context, project, typ, result string, ev orchapi.GitHubPREvent) error {
	res, err := s.db().ExecContext(ctx, `INSERT OR IGNORE INTO ghlink_pr_events (project, repo, number, head_sha, type, result, time)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, project, ev.Repo, ev.Number, ev.HeadSHA, typ, result, store.Millis(s.now()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already published
	}
	_, err = s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: fmt.Sprintf("%s#%d", ev.Repo, ev.Number),
		Project: project, Data: ev})
	if err != nil {
		_, _ = s.db().ExecContext(context.WithoutCancel(ctx), `DELETE FROM ghlink_pr_events WHERE project = ? AND repo = ? AND number = ?
			AND head_sha = ? AND type = ? AND result = ?`, project, ev.Repo, ev.Number, ev.HeadSHA, typ, result)
		return fmt.Errorf("publish %s: %w", typ, err)
	}
	return nil
}
