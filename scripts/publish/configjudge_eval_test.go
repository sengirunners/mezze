package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/judge"
)

// The config judge's eval cases: fail are config files that hold a credential or a personal or customer value
// (the pattern scanners miss them: none has a token's shape), pass are files that only look like it. Every run
// checks the cases' shape. With ZM_JUDGE_EVAL=1 the configured model (ZM_JUDGE, default claude:haiku) judges
// each case ZM_JUDGE_EVAL_RUNS times (default 3) on the machine's plan login, through the same call publish
// makes, and every run must agree. Run it after changing configjudge.md and record the runs in the PR.
var configJudgeEval = []struct {
	name, path, text string
	want             judge.Status
}{
	{"fail/db-password", "deploy/app.yaml", "database:\n  host: db.internal\n  password: \"Autumn2019\"\n", judge.Fail},
	{"fail/passphrase-words", ".env.production", "ADMIN_PASSPHRASE=correct-horse-battery-staple\nPORT=8080\n", judge.Fail},
	{"fail/conn-string", "config/db.toml", "url = \"postgres://app:" + "s3cr3tpw@db.prod.acme-health.io:5432/app\"\n", judge.Fail},
	{"fail/bearer", "scripts/notify.sh", "#!/bin/sh\ncurl -H 'Authorization: Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0' https://api.vendor.io/v1/ping\n", judge.Fail},
	{"fail/webhook-secret", ".github/workflows/hook.yml", "env:\n  WEBHOOK_SIGNING_SECRET: whs_live_not_a_shape_scanners_know_7731\n", judge.Fail},
	{"fail/home-dir", "settings.json", "{\"cache\": \"/Us" + "ers/janedoe-private/Library/Caches/app\", \"owner\": \"Jane Doe\"}\n", judge.Fail},
	{"fail/private-email", "ops/contact.ini", "[alerts]\nnotify = jane.doe.1984@" + "gmail.com\nphone = +1 415 555 0132\n", judge.Fail},
	{"fail/customer", "fixtures/seed.yaml", "patients:\n  - name: Margaret Okafor\n    mrn: 004471982\n    phone: 212-555-0187\n", judge.Fail},
	{"pass/secrets-ref", ".github/workflows/ci.yml", "env:\n  TOKEN: ${{ secrets.RELEASE_TOKEN }}\n  DB_PASSWORD: ${DB_PASSWORD}\n", judge.Pass},
	{"pass/placeholder", ".env.example", "API_KEY=changeme\nDATABASE_PASSWORD=<your-password>\nSECRET=\n", judge.Pass},
	{"pass/test-values", "testdata/config.yaml", "# test-only values\npassword: test-token\napi_url: http://localhost:8080\nuser: dummy\n", judge.Pass},
	{"pass/key-file-path", "deploy/tls.toml", "cert = \"/etc/ssl/app.crt\"\nkey_file = \"/etc/ssl/app.key\"\n", judge.Pass},
	{"pass/bot-address", "release.json", "{\"from\": \"releases@example.com\", \"reply_to\": \"noreply@sengirunners.com\"}\n", judge.Pass},
	{"pass/digest", "deploy/Dockerfile", "FROM golang:1.27@sha256:4f1c7bde1f2d9c7a6b0f0b0a1d2e3f4a5b6c7d8e9f00112233445566778899aa\nRUN go build ./...\n", judge.Pass},
	{"pass/getenv", "run.sh", "#!/bin/sh\nexport TOKEN=\"$(security find-generic-password -s zm -w)\"\nexec zm \"$@\"\n", judge.Pass},
	{"pass/plain-config", "config.yaml", "listen: 127.0.0.1:7070\nlog_level: info\nworkers: 4\n", judge.Pass},
}

func TestConfigJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	var fails, passes int
	for _, c := range configJudgeEval {
		if !strings.HasPrefix(c.name, string(c.want)+"/") || !configLike(c.path) {
			t.Errorf("%s: not a config judge case (its name says its verdict, its path must be one the judge reads)", c.name)
		}
		if c.want == judge.Fail {
			fails++
		} else {
			passes++
		}
	}
	if fails < 8 || passes < 8 {
		t.Fatalf("want at least 8 fail and 8 pass cases, have %d and %d", fails, passes)
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
		return
	}
	for _, c := range configJudgeEval {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i := range runs {
				r := judgeOnce(judge.FromEnv(nil), []judgeFile{{Path: c.path, Text: c.text}})
				got := judge.Pass
				if r != "" {
					got = judge.Fail
				}
				if strings.HasPrefix(r, "held: ") {
					got = judge.Held
				}
				if got != c.want {
					t.Errorf("run %d: want %s, got %s: %s", i+1, c.want, got, r)
				} else {
					t.Logf("run %d: %s %s", i+1, got, strings.TrimSpace(r))
				}
			}
		})
	}
}
