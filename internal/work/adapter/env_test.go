package adapter

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// The SPEC 17.2 allowlist: from a daemon environment where everything is
// set (the farm's live server among it), an adapter gets the base, what it
// declares, zm's own values and its secrets from their source, nothing else.
func TestEnvIsTheBaseAndTheDeclaration(t *testing.T) {
	shell := func(k string) (string, bool) { return "shell-" + k, true }
	zm := map[string]string{orchapi.EnvDataDir: "/zm/trackers/p/bd", orchapi.EnvToolsDir: "/zm/tools"}
	secret := func(s orchapi.AdapterSecret) (string, error) {
		if s.From == orchapi.SecretSlot {
			return "pw", nil
		}
		return "", nil // a config key that names no file: unset
	}
	env, secrets, err := Env(nil, shell, zm, secret)
	if err != nil || len(secrets) != 0 || strings.Join(env, " ") != "PATH=shell-PATH HOME=shell-HOME LANG=shell-LANG TERM=shell-TERM TZ=shell-TZ" {
		t.Fatalf("no declaration: %q %q %v", env, secrets, err)
	}
	d := &orchapi.AdapterEnv{Vars: []string{orchapi.EnvDataDir, "JIRA_URL", "PATH"}, Secrets: []orchapi.AdapterSecret{
		{Name: "BEADS_DOLT_PASSWORD", From: orchapi.SecretSlot}, {Name: "GH_TOKEN", From: orchapi.SecretConfig + "token_file"}}}
	env, secrets, err = Env(d, shell, zm, secret)
	want := []string{"PATH=shell-PATH", "HOME=shell-HOME", "LANG=shell-LANG", "TERM=shell-TERM", "TZ=shell-TZ",
		"ZM_DATA_DIR=/zm/trackers/p/bd", "JIRA_URL=shell-JIRA_URL", "BEADS_DOLT_PASSWORD=pw"}
	if err != nil || !slices.Equal(env, want) || !slices.Equal(secrets, []string{"pw"}) {
		t.Fatalf("declared:\n got %q %q %v\nwant %q", env, secrets, err, want)
	}
	// A zm-filled name zm has no value for is unset, never the shell's.
	if env, _, _ := Env(&orchapi.AdapterEnv{Vars: []string{orchapi.EnvToolsDir}}, shell, nil, secret); slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "ZM_") }) {
		t.Fatalf("a ZM_ name from the shell: %q", env)
	}
	if _, _, err := Env(&orchapi.AdapterEnv{Secrets: []orchapi.AdapterSecret{{Name: "T", From: orchapi.SecretSlot}}}, shell, zm,
		func(orchapi.AdapterSecret) (string, error) { return "", errors.New("unreadable") }); err == nil {
		t.Fatal("an unreadable secret was not an error")
	}
}

func TestCheckEnvRefuses(t *testing.T) {
	for _, d := range []*orchapi.AdapterEnv{
		{Vars: []string{"BAD-NAME"}},
		{Vars: []string{"ZM_TOKEN"}}, // zm's own credentials are never passed
		{Vars: []string{"TOK"}, Secrets: []orchapi.AdapterSecret{{Name: "TOK", From: orchapi.SecretSlot}}},
		{Secrets: []orchapi.AdapterSecret{{Name: "HOME", From: orchapi.SecretSlot}}},
		{Secrets: []orchapi.AdapterSecret{{Name: "TOK", From: "env:TOK"}}},
		{Secrets: []orchapi.AdapterSecret{{Name: "TOK", From: orchapi.SecretConfig}}},
	} {
		if CheckEnv(d) == nil {
			t.Errorf("accepted %+v", d)
		}
	}
	// Plain variables zm never passes from its shell (SPEC 17.2).
	for v, why := range map[string]string{
		"GH_TOKEN": "secret-shaped", "AWS_SECRET_ACCESS_KEY": "secret-shaped", "DB_PASSWORD": "secret-shaped",
		"API_KEY": "secret-shaped", "OPENAI_API_KEY": "secret-shaped", "API_KEY_FILE": "secret-shaped", "token": "secret-shaped",
		"SSH_AUTH_SOCK": "inject", "DYLD_INSERT_LIBRARIES": "inject", "LD_PRELOAD": "inject", "GIT_SSH_COMMAND": "inject",
		"GIT_ASKPASS": "inject", "GIT_CONFIG_GLOBAL": "inject",
		"BEADS_DOLT_SERVER_HOST": "tracker CLI's own", "BD_REAL": "tracker CLI's own", "DOLT_CLI_PASSWORD": "secret-shaped",
	} {
		if err := CheckEnv(&orchapi.AdapterEnv{Vars: []string{v}}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", v, err, why)
		}
	}
	for _, v := range []string{"XDG_CONFIG_HOME", "JIRA_URL", "MONKEY", "KEYCHAIN_ITEM", "GIT_AUTHOR_NAME"} {
		if err := CheckEnv(&orchapi.AdapterEnv{Vars: []string{v}}); err != nil {
			t.Errorf("%s refused: %v", v, err)
		}
	}
	if Describe(nil) != "" || !strings.Contains(Describe(&orchapi.AdapterEnv{Vars: []string{"JIRA_URL"}, Secrets: []orchapi.AdapterSecret{{Name: "T", From: orchapi.SecretSlot}}}),
		"JIRA_URL, T (secret, from slot)") {
		t.Fatal("Describe")
	}
	if err := CheckEnv(&orchapi.AdapterEnv{Vars: []string{orchapi.EnvDataDir, orchapi.EnvToolsDir, "XDG_CONFIG_HOME"},
		Secrets: []orchapi.AdapterSecret{{Name: "GH_TOKEN", From: orchapi.SecretConfig + "token_file"}}}); err != nil {
		t.Fatal(err)
	}
}
