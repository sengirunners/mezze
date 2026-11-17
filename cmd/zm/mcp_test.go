package main

import (
	"context"
	"errors"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

func TestMCPRegistrationIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, as, token string
		env             map[string]string
		register        bool
		inherited       string
	}{
		{name: "human"},
		{name: "codex", env: map[string]string{"CODEX_THREAD_ID": "11223344-thread"}, register: true},
		{name: "managed marker", env: map[string]string{"ZM_SESSION": "mini/aabbccdd", "CODEX_THREAD_ID": "11223344-thread"}, inherited: "mini/aabbccdd"},
		{name: "inherited session env", env: map[string]string{"ZM_AS": "mini/d6614447", "CLAUDE_CODE_SESSION_ID": "11223344-claude"}, inherited: "mini/d6614447"},
		{name: "another machine's session", env: map[string]string{"ZM_AS": "studio/d6614447", "CLAUDE_CODE_SESSION_ID": "11223344-claude"}},
		{name: "explicit env", env: map[string]string{"ZM_AS": "human:ana", "CODEX_THREAD_ID": "11223344-thread"}},
		{name: "explicit flag", as: "mini/aabbccdd", env: map[string]string{"CODEX_THREAD_ID": "11223344-thread"}},
		{name: "token", token: "zm_test", env: map[string]string{"CODEX_THREAD_ID": "11223344-thread"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := mcpOptions(&client.Client{Token: tt.token}, "mini", tt.as, func(k string) string { return tt.env[k] })
			if (o.Register != nil) != tt.register {
				t.Fatalf("register=%+v, want registration=%v", o.Register, tt.register)
			}
			if o.Register != nil && (o.Register.Agent != "codex" || o.Register.RuntimeID != tt.env["CODEX_THREAD_ID"]) {
				t.Fatalf("wrong runtime identity: %+v", o.Register)
			}
			if got := ""; o.Inherited != nil {
				got = o.Inherited.Address
				if o.Inherited.Own.RuntimeID == "" || o.Inherited.NearestAgent == nil {
					t.Fatalf("inherited check lacks its proof inputs: %+v", o.Inherited)
				}
				if got != tt.inherited {
					t.Fatalf("inherited=%q, want %q", got, tt.inherited)
				}
			} else if tt.inherited != "" {
				t.Fatalf("no inherited check, want %q", tt.inherited)
			}
		})
	}
}

func TestMCPSandboxesFlag(t *testing.T) {
	box := computeapi.RunTargetList{Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "db", Kind: "database"}}, {RunTarget: computeapi.RunTarget{Name: "sengi", Kind: "box"}}}}
	noBox := computeapi.RunTargetList{Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "db", Kind: "database"}}}}
	down := errors.New("daemon not running")
	for _, tt := range []struct {
		flag     string
		list     computeapi.RunTargetList
		err      error
		want     bool
		asked    bool
		badValue bool
	}{
		{flag: "on", want: true},
		{flag: "only", want: true},
		{flag: "ONLY", want: true},
		{flag: "true", want: true},
		{flag: "off"},
		{flag: "false"},
		{flag: "T", want: true},
		{flag: "f"},
		{flag: "auto", list: box, want: true, asked: true},
		{flag: "auto", list: noBox, asked: true},
		{flag: "auto", err: down, asked: true},
		{flag: "maybe", badValue: true},
	} {
		asked := false
		got, err := sandboxesOn(context.Background(), tt.flag, func(ctx context.Context, describe bool) (computeapi.RunTargetList, error) {
			asked = true
			if _, ok := ctx.Deadline(); !ok || describe {
				t.Errorf("%s: auto must ask without describe and with a deadline", tt.flag)
			}
			return tt.list, tt.err
		})
		if (err != nil) != tt.badValue || got != tt.want || asked != tt.asked {
			t.Errorf("--sandboxes=%s: got %v err %v asked %v, want %v asked %v", tt.flag, got, err, asked, tt.want, tt.asked)
		}
	}
}
