package main

// A separate causal diagnostic, never a budget row or input to bench repeat.
// Both fixtures use the ordinary setup and identical mail payloads. Raw paired
// observations avoid attributing an independent percentile difference to startup.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

type diagnosticBinary struct {
	Commit string `json:"commit"`
	SHA256 string `json:"binary_sha256"`
	Path   string `json:"path"`
}

type sendObservation struct {
	Pair      int       `json:"pair"`
	Sequence  int       `json:"sequence"`
	Version   string    `json:"version"`
	Operation string    `json:"operation"`
	Started   time.Time `json:"started"`
	WallNS    int64     `json:"wall_ns"`
	UserNS    *int64    `json:"child_user_cpu_ns,omitempty"`
	SystemNS  *int64    `json:"child_system_cpu_ns,omitempty"`
}

type sendDiagnostic struct {
	Schema    string            `json:"schema"`
	Started   time.Time         `json:"started"`
	Finished  time.Time         `json:"finished"`
	Runtime   string            `json:"go_runtime"`
	Platform  string            `json:"platform"`
	Runner    map[string]string `json:"runner"`
	Baseline  diagnosticBinary  `json:"baseline"`
	Candidate diagnosticBinary  `json:"candidate"`
	Pairs     int               `json:"pairs"`
	Scale     int               `json:"mail_seed_scale_per_fixture"`
	Warmups   int               `json:"warmups_per_operation_per_fixture"`
	Notes     []string          `json:"notes"`
	Before    map[string]string `json:"guest_counters_before"`
	After     map[string]string `json:"guest_counters_after"`
	Samples   []sendObservation `json:"samples"`
	Error     string            `json:"error,omitempty"`
}

func diagnoseSendCmd(args []string) error {
	fs := flag.NewFlagSet("diagnose-send", flag.ContinueOnError)
	baseBin := fs.String("baseline-bin", "", "baseline zm executable")
	nextBin := fs.String("candidate-bin", "", "candidate zm executable")
	baseSHA := fs.String("baseline-sha", "", "exact baseline source commit")
	nextSHA := fs.String("candidate-sha", "", "exact candidate source commit")
	out := fs.String("out", "", "raw diagnostic JSON (required)")
	n := fs.Int("n", 40, "paired samples per operation (1..200)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if fs.NArg() != 0 || *n < 1 || *n > 200 || *out == "" || *baseBin == "" || *nextBin == "" || !sha.MatchString(*baseSHA) || !sha.MatchString(*nextSHA) {
		return fmt.Errorf("diagnose-send needs two binaries, their full lowercase commit SHAs, -out and -n between 1 and 200")
	}
	baseline, err := diagnosticProvenance(*baseBin, *baseSHA)
	if err != nil {
		return err
	}
	candidate, err := diagnosticProvenance(*nextBin, *nextSHA)
	if err != nil {
		return err
	}
	return runSendDiagnostic(baseline, candidate, *out, *n)
}

func diagnosticProvenance(bin, commit string) (diagnosticBinary, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return diagnosticBinary{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return diagnosticBinary{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return diagnosticBinary{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return diagnosticBinary{}, err
	}
	return diagnosticBinary{Commit: commit, Path: path, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func runSendDiagnostic(baseline, candidate diagnosticBinary, out string, n int) (resultErr error) {
	r := sendDiagnostic{Schema: "zm-send-diagnostic-v1", Started: time.Now().UTC(), Runtime: runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH, Baseline: baseline, Candidate: candidate, Pairs: n, Scale: 5000, Warmups: 2,
		Runner: map[string]string{"name": os.Getenv("RUNNER_NAME"), "os": os.Getenv("RUNNER_OS"), "arch": os.Getenv("RUNNER_ARCH"), "run_id": os.Getenv("GITHUB_RUN_ID"), "harness_sha": os.Getenv("GITHUB_SHA")},
		Notes: []string{"CLI send includes exec, runtime/package initialization, Cobra, client setup, RPC and output.",
			"CLI version includes exec, runtime/package initialization and Cobra; it bypasses client setup.",
			"API send is a persistent Unix HTTP roundtrip, not internal daemon duration; child CPU is absent for API rows.",
			"Timed CLI commands have a 10-second cancellation deadline and 1-second output-drain limit; these diagnostic observations are not budget verdicts.",
			"Both isolated fixtures remain live on the same VM; sample order alternates versions and rotates operations.",
			"No thresholds, retries, percentile subtraction or causal verdict are applied. Guest counters do not identify physical-host load."}}
	defer func() {
		if r.Finished.IsZero() {
			r.Finished, r.After = time.Now().UTC(), diagnosticCounters()
		}
		if resultErr != nil {
			r.Error = resultErr.Error()
		}
		b, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			err = os.WriteFile(out, b, 0o600)
		}
		if err != nil {
			resultErr = fmt.Errorf("diagnostic output: %w (operation error: %v)", err, resultErr)
		}
	}()
	fixtures := []*env{{bin: baseline.Path, scale: r.Scale, want: []string{"version", "send"}}, {bin: candidate.Path, scale: r.Scale, want: []string{"version", "send"}}}
	for i, e := range fixtures {
		defer e.cleanup() // also covers a partially completed setup
		if err := e.setup(); err != nil {
			return fmt.Errorf("fixture %d setup: %w", i, err)
		}
		e.hc.Timeout = 10 * time.Second
		defer e.hc.CloseIdleConnections()
	}
	for i, e := range fixtures {
		// Use the canonical coreRows for the same two warmups as sample().
		rows := e.coreRows()
		for range r.Warmups {
			for _, op := range []string{"version", "send"} {
				if _, err := rows[op](); err != nil {
					return fmt.Errorf("fixture %d warmup %s: %w", i, op, err)
				}
			}
			if err := diagnosticAPISend(e); err != nil {
				return fmt.Errorf("fixture %d API warmup: %w", i, err)
			}
		}
	}
	r.Before = diagnosticCounters()
	resultErr = collectSendObservations(n, func(pair, version int, op string) error {
		observation, err := observeSend(fixtures[version], op)
		observation.Pair, observation.Sequence, observation.Version = pair, len(r.Samples), []string{"baseline", "candidate"}[version]
		r.Samples = append(r.Samples, observation)
		if err != nil {
			return fmt.Errorf("pair %d %s %s: %w", pair, observation.Version, op, err)
		}
		return nil
	})
	// Snapshot immediately after observations, before fixture teardown adds work.
	r.Finished, r.After = time.Now().UTC(), diagnosticCounters()
	return resultErr
}

// Every pair has both versions of every operation; deterministic rotation
// avoids permanently placing one version or command first.
func collectSendObservations(n int, observe func(pair, version int, op string) error) error {
	ops := []string{"cli-send", "cli-version", "api-send-roundtrip"}
	for pair := range n {
		for j := range ops {
			for k := range 2 {
				if err := observe(pair, (k+pair)%2, ops[(j+pair)%len(ops)]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func diagnosticCLIArgs(op string) ([]string, error) {
	switch op {
	case "cli-send":
		return []string{"send", "human:ana", "bench", "--project", "bench"}, nil
	case "cli-version":
		return []string{"--version"}, nil
	default:
		return nil, fmt.Errorf("unknown diagnostic CLI operation %q", op)
	}
}

func observeSend(e *env, op string) (sendObservation, error) {
	r := sendObservation{Operation: op}
	if op == "api-send-roundtrip" {
		r.Started = time.Now().UTC()
		t := time.Now()
		err := diagnosticAPISend(e)
		r.WallNS = time.Since(t).Nanoseconds()
		return r, err
	}
	args, err := diagnosticCLIArgs(op)
	if err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prototype := e.cmd(nil, args...)
	c := exec.CommandContext(ctx, prototype.Path, prototype.Args[1:]...)
	c.Dir, c.Env = prototype.Dir, prototype.Env
	c.WaitDelay = time.Second
	var output bytes.Buffer
	c.Stdout, c.Stderr = &output, &output
	r.Started = time.Now().UTC()
	t := time.Now()
	err = c.Run()
	r.WallNS = time.Since(t).Nanoseconds()
	if c.ProcessState != nil {
		user, system := c.ProcessState.UserTime().Nanoseconds(), c.ProcessState.SystemTime().Nanoseconds()
		r.UserNS, r.SystemNS = &user, &system
	}
	if err != nil {
		return r, fmt.Errorf("%v: %s", err, output.String())
	}
	return r, nil
}

func diagnosticAPISend(e *env) error {
	var answer orchapi.Mail
	if err := e.api(http.MethodPost, "/v0/mail", "", "bench", orchapi.SendMailRequest{To: "human:ana", Body: "bench"}, &answer); err != nil {
		return err
	}
	if answer.ID == "" || answer.Recipient != "human:ana" || answer.Body != "bench" {
		return fmt.Errorf("API send answered a different operation: %+v", answer)
	}
	return nil
}

func diagnosticCounters() map[string]string {
	result := map[string]string{}
	for _, path := range []string{"/etc/hostname", "/proc/version", "/proc/cpuinfo", "/proc/stat", "/proc/loadavg", "/proc/meminfo", "/proc/pressure/cpu", "/proc/pressure/io", "/proc/pressure/memory", "/sys/fs/cgroup/cpu.stat"} {
		f, err := os.Open(path)
		if err != nil {
			result[path] = "unavailable: " + err.Error()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 64<<10))
		_ = f.Close()
		if err != nil {
			result[path] = "unavailable: " + err.Error()
		} else {
			result[path] = string(b)
		}
	}
	return result
}
