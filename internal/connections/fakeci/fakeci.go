// Package fakeci is the example connection provider (SPEC 24.2): a fake CI
// with a job queue and a runner pool that serves zm.connection.show/1. It
// is the conformance fixture, and the shape a third party copies: answer
// GET with a summary, check the bearer token, stamp as_of.
package fakeci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Job is one CI job.
type Job struct {
	ID, Name, Repo, URL string
	Queued, Started     time.Time
}

// CI is a fake CI: a queue, a runner pool of Runners, one account.
type CI struct {
	Token   string
	Account string
	Runners int
	// WarnQueue: this many queued jobs, or one waiting longer than WarnWait, is a warning.
	WarnQueue int
	WarnWait  time.Duration
	Now       func() time.Time

	mu      sync.Mutex
	queue   []Job
	running []Job
	down    time.Time // when the pool went to zero runners
	// Body, when set, is served as is (a broken or old provider).
	Body []byte
}

// New is a CI with runners runners for account.
func New(token, account string, runners int) *CI {
	return &CI{Token: token, Account: account, Runners: runners, WarnQueue: 10, WarnWait: 5 * time.Minute, Now: time.Now}
}

// Enqueue adds a job to the queue.
func (c *CI) Enqueue(j Job) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j.Queued.IsZero() {
		j.Queued = c.Now()
	}
	c.queue = append(c.queue, j)
}

// Tick starts queued jobs on free runners.
func (c *CI) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.queue) > 0 && len(c.running) < c.Runners {
		j := c.queue[0]
		c.queue = c.queue[1:]
		j.Started = c.Now()
		c.running = append(c.running, j)
	}
}

// Finish ends a running job.
func (c *CI) Finish(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, j := range c.running {
		if j.ID == id {
			c.running = append(c.running[:i], c.running[i+1:]...)
			return
		}
	}
}

// SetRunners changes the pool size.
func (c *CI) SetRunners(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n == 0 && c.Runners > 0 {
		c.down = c.Now()
	}
	c.Runners = n
}

// Summary is what the CI serves now.
func (c *CI) Summary() orchapi.ConnectionSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Now().UTC()
	s := orchapi.ConnectionSummary{Schema: orchapi.ConnectionSchema, Title: "Fake CI", AsOf: now, StaleAfterS: 60,
		Headline: []orchapi.ConnectionRow{}, Meters: []orchapi.ConnectionMeter{}, Running: []orchapi.ConnectionRunning{}}

	queued := int64(len(c.queue))
	q := orchapi.ConnectionRow{Key: "queue", Label: "Waiting for a runner", State: orchapi.ConnectionOK, Count: &queued, Account: c.Account}
	if len(c.queue) > 0 {
		oldest := c.queue[0].Queued.UTC()
		q.Since = &oldest
		if len(c.queue) >= c.WarnQueue || now.Sub(oldest) > c.WarnWait {
			q.State = orchapi.ConnectionWarn
		}
	}
	runners := int64(c.Runners)
	r := orchapi.ConnectionRow{Key: "runners", Label: "Runners", State: orchapi.ConnectionOK, Count: &runners, Account: c.Account}
	if c.Runners == 0 {
		r.State, r.Detail = orchapi.ConnectionRed, "no runner is online"
		if !c.down.IsZero() {
			d := c.down.UTC()
			r.Since = &d
		}
		if len(c.queue) > 0 {
			q.State = orchapi.ConnectionRed
		}
	}
	s.Headline = append(s.Headline, q, r)
	s.Meters = append(s.Meters, orchapi.ConnectionMeter{Key: "runners", Label: "Runners busy", Used: float64(len(c.running)),
		Total: float64(max(c.Runners, 1)), Unit: "runners", Account: c.Account})
	for _, j := range c.running {
		st := j.Started.UTC()
		item := orchapi.ConnectionRunning{ID: j.ID, Title: j.Name, Owner: j.Repo, Started: &st, Account: c.Account}
		if j.URL != "" {
			item.Links = []orchapi.ConnectionLink{{Label: "Run", URL: j.URL}}
		}
		s.Running = append(s.Running, item)
	}
	return s
}

// ServeHTTP answers GET with the summary; any other token is 401.
func (c *CI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if c.Token != "" && r.Header.Get("Authorization") != "Bearer "+c.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	c.mu.Lock()
	body := c.Body
	c.mu.Unlock()
	if body != nil {
		_, _ = w.Write(body)
		return
	}
	if err := json.NewEncoder(w).Encode(c.Summary()); err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
	}
}

// Serve sets the raw body served (nil: the live summary).
func (c *CI) Serve(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Body = body
}
