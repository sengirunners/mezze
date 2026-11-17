package home

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	home := "/Users/ana"
	for in, want := range map[string]string{
		"file:///Volumes/b/zm": "file:///Volumes/b/zm",
		"/Volumes/b/zm/":       "file:///Volumes/b/zm",
		"~/backups":            "file:///Users/ana/backups",
		"icloud":               "icloud:/Users/ana/" + ICloudRel,
		"icloud:beads":         "icloud:/Users/ana/" + ICloudRel + "/beads",
	} {
		got, err := Parse(in, home)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "file://u:secret@localhost/x", "file://host/x", "relative/path", "icloud:../x", "bogus://x",
		"local", "embedded", "server://127.0.0.1:3307/beads", "ssh://ana@sengi-1", "s3://bucket/zm", "r2://bucket"} {
		if _, err := Parse(bad, home); err == nil {
			t.Errorf("%q: parsed", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: error leaks the password: %v", bad, err)
		}
	}
	tg, _ := Parse("icloud:beads", home)
	if tg.Spec(home) != "icloud:beads" {
		t.Errorf("spec %q", tg.Spec(home))
	}
}

func TestUnitFiles(t *testing.T) {
	u := Unit{Label: "dev.zm.cloudflared.abcd1234", Args: []string{"/z m/tools/cloudflared", "tunnel", "--config", "/z m/tunnel/config.yml"},
		Dir: "/z m/tunnel", Log: "/z m/tunnel/server.log", Env: map[string]string{"HOME": "/z m/tunnel/home"}}
	pl := LaunchdPlist(u)
	for _, want := range []string{"<key>Label</key><string>dev.zm.cloudflared.abcd1234</string>", "<string>/z m/tools/cloudflared</string>",
		"<string>tunnel</string>", "<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>",
		"<key>HOME</key><string>/z m/tunnel/home</string>", "<key>StandardErrorPath</key><string>/z m/tunnel/server.log</string>"} {
		if !strings.Contains(pl, want) {
			t.Errorf("plist lacks %q:\n%s", want, pl)
		}
	}
	u.Args[0] = "/a&b/cloudflared"
	if !strings.Contains(LaunchdPlist(u), "/a&amp;b/cloudflared") {
		t.Error("plist does not escape &")
	}
	u.Args[0] = "/z m/tools/cloudflared"
	su := SystemdUnit(u)
	for _, want := range []string{`ExecStart="/z m/tools/cloudflared" tunnel --config "/z m/tunnel/config.yml"`,
		`WorkingDirectory="/z m/tunnel"`, `Environment="HOME=/z m/tunnel/home"`, "Restart=always", "WantedBy=default.target",
		"StandardOutput=append:/z m/tunnel/server.log"} {
		if !strings.Contains(su, want) {
			t.Errorf("systemd unit lacks %q:\n%s", want, su)
		}
	}
	if m := (Systemd{Dir: "/h/.config/systemd/user"}); m.Path(u) != "/h/.config/systemd/user/dev.zm.cloudflared.abcd1234.service" {
		t.Error(m.Path(u))
	}
	if m := (Launchd{Dir: "/h/Library/LaunchAgents"}); m.Path(u) != "/h/Library/LaunchAgents/dev.zm.cloudflared.abcd1234.plist" {
		t.Error(m.Path(u))
	}
}
