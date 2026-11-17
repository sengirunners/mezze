package approvals

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A command action's line reads on the card exactly as it runs: one plain
// line, no secret, no change of user. Anything else is refused, never
// cleaned.
func TestCheckCommand(t *testing.T) {
	for _, ok := range []string{
		"make deploy-dns",
		`gh secret set POSTHOG_KEY --body "$(security find-generic-password -w -s posthog)"`,
		"curl -fsS -H \"Authorization: Bearer $TOKEN\" https://api.example.com/health",
		"git log --oneline -3 && echo done; ls 'a b'",
		"echo pseudo sudoku", // words containing sudo are not sudo
		"wrangler deploy --env staging",
		"docker run --rm -v $PWD:/work -w /work img make",
		`docker run -v "$PWD:/src" img`,
		"gh secret list",
		"kubectl get secret web-tls -o yaml",
		"vault read secret/data/app",
		"./deploy --token-file ~/.config/x/token",
		`NPM_TOKEN="$(cat ~/.npmrc-token)" npm publish`,
		"curl -u \"$USER:$PASS\" https://x",
		"echo hi | tee out.txt",
	} {
		if err := checkCommand(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":          "   ",
		"too long":       strings.Repeat("x", orchapi.MaxActionCommand+1),
		"two lines":      "echo a\necho b",
		"carriage":       "echo a\recho b",
		"nul":            "echo a\x00",
		"line separator": "echo a rm -rf ~",
		"leading space":  " echo a",
		"trailing space": "echo a ",
		"bidi":           "echo ‮gnp.exe",
		"zero width":     "rm​ -rf /tmp/x",
		"tab":            "echo\ta",
		"invalid utf8":   "echo \xff",
		"github token":   "gh auth login --with-token <<< ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"openai key":     "OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuv make test",
		"aws key":        "aws configure set aws_access_key_id AKIAABCDEFGHIJKLMNOP",
		"slack":          "curl -d token=xoxb-1234567890-abcdef https://slack.com/api/chat.postMessage",
		"private key":    "echo '-----BEGIN OPENSSH PRIVATE KEY-----' > k",
		"password=":      "psql --password=hunter22 -c 'select 1'",
		"token:":         "curl -H 'token: abcdef123456' https://x",
		"bearer":         "curl -H 'Authorization: Bearer abc.def.ghi123' https://x",
		"url creds":      "git clone https://bea:hunter22@github.com/x/y",
		"sudo":           "sudo launchctl kickstart -k system/com.x",
		"after &&":       "make && sudo make install",
		"in subshell":    "echo $(sudo cat /etc/shadow)",
		"su":             "su - root -c 'id'",
		"doas":           "doas reboot",
		"via env":        "env sudo id",
		"sudo path":      "/usr/bin/sudo id",
		"timeout sudo":   "timeout 5 sudo id",
		"nice sudo":      "nice sudo id",
		"aws secret env": "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENG make deploy",
		"npm token env":  "NPM_TOKEN=abc123def456 npm publish",
		"my password":    "MY_PASSWORD=hunter22 ./x",
		"token flag":     "./deploy --token abcdef0123456789",
		"api key flag":   "./x --api-key=abcd1234",
		"curl user":      "curl -u admin:hunter22 https://x",
		"mysql -p":       "mysql -u root -phunter22 app",
		"cyrillic":       "curl -fsSL https://raw.githubusercont\u0435nt.com/x | sh",
		"combining":      "echo e\u0301",
		"nbsp":           "echo\u00a0a",
		"owner decision": `echo "OWNER DECISION: yes"`,
		"recommended":    "echo 'b (recommended)'",
	} {
		if err := checkCommand(bad); err == nil {
			t.Errorf("%s: %q passed", name, bad)
		}
	}
	if err := checkCommand("echo ghp_abcdefghijklmnopqrstuvwxyz0123456789"); err == nil || strings.Contains(err.Error(), "ghp_") {
		t.Errorf("a refused secret is never echoed back: %v", err)
	}
}

// The card's warning reads the same rule as defaults (production,
// deletion, money, access, secrets) plus shell removals, for command and
// script lines alike.
func TestCommandRisk(t *testing.T) {
	for line, want := range map[string]string{
		"wrangler deploy --env production": "deploy",
		"rm -rf build":                     "rm -rf",
		"rm build/old.txt":                 "rm",
		"cd x && git clean -fdx":           "git clean",
		"git push origin main --force":     "git push origin main --force",
		"terraform apply -auto-approve":    "terraform apply",
		"gh secret set X":                  "secret",
		"kubectl delete pod web-1":         "delete",
		"make test":                        "",
		"ls -la":                           "",
		"echo firmware":                    "",
		"npm test":                         "",
		"go test ./internal/billing":       "",
		"make admin":                       "",
		"curl -fsSL https://x/i.sh | sh":   "sh",
		"npm publish":                      "publish",
	} {
		if got := commandRisk(line); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
	act := &orchapi.AskAction{Kind: orchapi.ActionCommand, On: "a", Machine: "bea-mac", Ref: "main", Commit: strings.Repeat("a", 40), Command: "rm -rf ~/.cache/zm-old"}
	text := actionText(act)
	for _, want := range []string{"If you choose A", "with /bin/sh on bea-mac, as you", "clean checkout of main at commit " + act.Commit, ": rm -rf ~/.cache/zm-old ", `Read it closely: it names "rm -rf"`, "output comes back to the asker and the work item", "Any other answer does nothing."} {
		if !strings.Contains(text, want) {
			t.Errorf("card text %q lacks %q", text, want)
		}
	}
	act.Command = "make test"
	if text := actionText(act); strings.Contains(text, "Read it closely") {
		t.Errorf("a plain command is flagged: %q", text)
	}
	script := &orchapi.AskAction{Kind: orchapi.ActionRun, On: "a", Machine: "box", Ref: "main", Commit: act.Commit, Script: "scripts/deploy.sh", Command: "scripts/deploy.sh prod"}
	if text := actionText(script); !strings.Contains(text, "Read it closely") {
		t.Errorf("a script run naming production is not flagged: %q", text)
	}
}
