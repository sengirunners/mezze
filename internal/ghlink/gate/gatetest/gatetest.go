// Package gatetest is a fake gate for tests (SPEC 18.3): the test binary
// linked as "fake-gate" answers from gate.json beside the link and records
// every request (and its environment) in requests.jsonl. A test binary
// calls Main from TestMain. Tests only.
package gatetest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Name is the link's base name.
const Name = "fake-gate"

// Config is gate.json: what the fake answers.
type Config struct {
	Describe     map[string]any            `json:"describe,omitempty"` // nil = a valid default
	FailDescribe bool                      `json:"fail_describe,omitempty"`
	Match        []string                  `json:"match,omitempty"` // non-nil: offer match and answer these
	FailMatch    bool                      `json:"fail_match,omitempty"`
	Decide       map[string]map[string]any `json:"decide,omitempty"` // by PR state; "final" overrides on final
	FailDecide   bool                      `json:"fail_decide,omitempty"`
	SleepSeconds float64                   `json:"sleep_seconds,omitempty"`
	Garbage      bool                      `json:"garbage,omitempty"` // answer something that is not JSON
}

// Logged is one recorded request.
type Logged struct {
	Verb string          `json:"verb"`
	Args json.RawMessage `json:"args"`
	Env  []string        `json:"env"`
}

// Main runs the fake when this process is it, and reports whether it was.
func Main() (code int, handled bool) {
	if filepath.Base(os.Args[0]) != Name {
		return 0, false
	}
	return run(), true
}

// Install links the running test binary as dir/fake-gate and writes cfg.
func Install(dir string, cfg Config) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, Name)
	if err := os.Symlink(self, p); err != nil {
		return "", err
	}
	return p, Set(dir, cfg)
}

// Set rewrites gate.json in dir.
func Set(dir string, cfg Config) error {
	b, _ := json.Marshal(cfg)
	return os.WriteFile(filepath.Join(dir, "gate.json"), b, 0o600)
}

// Requests reads every recorded request in dir.
func Requests(dir string) []Logged {
	b, _ := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	var out []Logged
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var x Logged
		if json.Unmarshal([]byte(l), &x) == nil && x.Verb != "" {
			out = append(out, x)
		}
	}
	return out
}

func run() int {
	dir := filepath.Dir(os.Args[0])
	var cfg Config
	if b, err := os.ReadFile(filepath.Join(dir, "gate.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	in, _ := io.ReadAll(os.Stdin)
	var req struct {
		Verb string          `json:"verb"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(in, &req); err != nil {
		fmt.Fprintln(os.Stderr, "fake gate: bad request")
		return 2
	}
	if f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(Logged{Verb: req.Verb, Args: req.Args, Env: os.Environ()})
		f.Write(append(b, '\n'))
		f.Close()
	}
	if cfg.SleepSeconds > 0 {
		time.Sleep(time.Duration(cfg.SleepSeconds * float64(time.Second)))
	}
	if cfg.Garbage {
		fmt.Println("this is not json")
		return 0
	}
	ok := func(v any) int {
		b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
		fmt.Println(string(b))
		return 0
	}
	refuse := func(code, msg string) int {
		b, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
		fmt.Println(string(b))
		return 0
	}
	switch req.Verb {
	case "describe":
		if cfg.FailDescribe {
			fmt.Fprintln(os.Stderr, "fake gate: describe broken")
			return 1
		}
		if cfg.Describe != nil {
			return ok(cfg.Describe)
		}
		verbs := []string{"describe", "decide"}
		if cfg.Match != nil || cfg.FailMatch {
			verbs = append(verbs, "match")
		}
		return ok(map[string]any{"zm": "zm/v0", "name": "fake-gate", "version": "1", "verbs": verbs})
	case "match":
		if cfg.FailMatch {
			fmt.Fprintln(os.Stderr, "fake gate: match broken")
			return 1
		}
		return ok(map[string]any{"ids": cfg.Match})
	case "decide":
		if cfg.FailDecide {
			fmt.Fprintln(os.Stderr, "fake gate: decide broken")
			return 1
		}
		var a struct {
			PR struct {
				State string `json:"state"`
			} `json:"pr"`
			Final bool `json:"final"`
		}
		_ = json.Unmarshal(req.Args, &a)
		if d, found := cfg.Decide["final"]; found && a.Final {
			return ok(d)
		}
		if d, found := cfg.Decide[a.PR.State]; found {
			return ok(d)
		}
		return ok(map[string]any{"decision": "apply"})
	}
	return refuse("not_implemented", "no verb "+req.Verb)
}
