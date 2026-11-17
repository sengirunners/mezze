package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSendDiagnosticBalancedOrderAndFailure(t *testing.T) {
	type call struct {
		pair, version int
		op            string
	}
	var calls []call
	if err := collectSendObservations(6, func(pair, version int, op string) error {
		calls = append(calls, call{pair, version, op})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 36 {
		t.Fatalf("observations %d", len(calls))
	}
	ops := []string{"cli-send", "cli-version", "api-send-roundtrip"}
	for pair := range 6 {
		counts := map[string]int{}
		for _, c := range calls[pair*6 : pair*6+6] {
			if c.pair != pair {
				t.Fatal("crossed pair boundary")
			}
			counts[c.op+string(rune('0'+c.version))]++
		}
		for _, op := range ops {
			if counts[op+"0"] != 1 || counts[op+"1"] != 1 {
				t.Fatalf("unbalanced pair %d: %v", pair, counts)
			}
		}
		if calls[pair*6].version != pair%2 || calls[pair*6].op != ops[pair%3] {
			t.Fatalf("pair %d did not alternate versions and rotate operations", pair)
		}
	}
	want := errors.New("injected operation failure")
	count := 0
	err := collectSendObservations(40, func(int, int, string) error {
		count++
		if count == 5 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) || count != 5 {
		t.Fatalf("failure retried or hidden: count %d error %v", count, err)
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSendDiagnosticAPIEquivalentRequestAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
		bad            bool
	}{
		{"success", `{"id":"mail-1","recipient":"human:ana","body":"bench"}`, 200, false},
		{"server failure", `{}`, 500, true},
		{"malformed", `{`, 200, true},
		{"wrong recipient", `{"id":"mail-1","recipient":"human:other","body":"bench"}`, 200, true},
		{"missing id", `{"recipient":"human:ana","body":"bench"}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &env{hc: &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Path != "/v0/mail" || r.Header.Get("Zm-Project") != "bench" || r.Header.Get("Zm-Project-From") != "flag" || r.Header.Get("Zm-As") != "" {
					t.Fatalf("different send request: %+v", r)
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["to"] != "human:ana" || payload["body"] != "bench" || payload["needs"] != nil {
					t.Fatalf("different payload %v: %v", payload, err)
				}
				return &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status), Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})}}
			if err := diagnosticAPISend(e); (err != nil) != tc.bad {
				t.Fatalf("API error %v, want failure %v", err, tc.bad)
			}
		})
	}
}

func TestSendDiagnosticCLIAndProvenance(t *testing.T) {
	args, err := diagnosticCLIArgs("cli-send")
	if err != nil || !reflect.DeepEqual(args, []string{"send", "human:ana", "bench", "--project", "bench"}) {
		t.Fatalf("different canonical send argv: %v %v", args, err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-zm")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$DIAG_FIXTURE\" = yes ] || exit 9\nprintf 'fixture\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	p, err := diagnosticProvenance(bin, sha)
	if err != nil || p.Commit != sha || p.Path != bin || len(p.SHA256) != 64 {
		t.Fatalf("provenance %+v %v", p, err)
	}
	e := &env{bin: bin, repo: dir, base: []string{"DIAG_FIXTURE=yes"}}
	for _, op := range []string{"cli-send", "cli-version"} {
		r, err := observeSend(e, op)
		if err != nil || r.Operation != op || r.WallNS <= 0 || r.Started.IsZero() || r.UserNS == nil || r.SystemNS == nil {
			t.Fatalf("observation %+v %v", r, err)
		}
	}
	e.base = nil
	if _, err := observeSend(e, "cli-send"); err == nil {
		t.Fatal("CLI failure hidden")
	}
	if _, err := observeSend(e, "unknown"); err == nil {
		t.Fatal("unknown operation accepted")
	}
}

func TestSendDiagnosticRejectsUnboundedOrUnpinnedRequests(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-n", "0"},
		{"-n", "201"},
		{"-baseline-bin", "unused", "-candidate-bin", "unused", "-baseline-sha", "main", "-candidate-sha", strings.Repeat("a", 40), "-out", "unused.json"},
	} {
		if err := diagnoseSendCmd(args); err == nil {
			t.Fatalf("invalid request accepted: %v", args)
		}
	}
}
