package approvals

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/project"
)

// project.toml's [asks]: parsed, checked and written back; bad values are
// refused at load, never guessed.
func TestAsksSection(t *testing.T) {
	sec := asksSection{}
	v, err := sec.Parse("project.toml", map[string]any{"remind_after": "12h", "relane_after": "30h", "lane_machine": "box-a", "lane_agent": "lane-agent"})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(askSettings)
	if got.Remind != 12*time.Hour || got.Relane != 30*time.Hour || got.Escalate != 0 || got.LaneMachine != "box-a" {
		t.Fatalf("parsed %+v", got)
	}
	if err := sec.Validate(got, project.File{}); err != nil {
		t.Fatal(err)
	}
	if w := got.withDefaults(); w.Escalate != escalateAfter {
		t.Fatalf("defaults %+v", w)
	}
	enc := sec.Encode(got)
	for _, want := range []string{"[asks]", `remind_after = "12h0m0s"`, `relane_after = "30h0m0s"`, `lane_machine = "box-a"`, `lane_agent = "lane-agent"`} {
		if !strings.Contains(enc, want) {
			t.Errorf("encode %q lacks %q", enc, want)
		}
	}
	if strings.Contains(enc, "escalate_after") || sec.Encode(askSettings{}) != "" {
		t.Errorf("encode writes unset keys: %q", enc)
	}
	for name, raw := range map[string]map[string]any{
		"unknown":    {"remind": "1h"},
		"not text":   {"remind_after": int64(3600)},
		"too short":  {"remind_after": "10s"},
		"too long":   {"remind_after": "2000h"},
		"garbage":    {"relane_after": "soon"},
		"egress str": {"lane_egress": "pkg.example.com"},
		"egress int": {"lane_egress": []any{int64(1)}},
	} {
		if _, err := sec.Parse("project.toml", raw); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	for name, s := range map[string]askSettings{
		"order":         {Remind: 10 * time.Hour, Relane: 5 * time.Hour},
		"order default": {Escalate: time.Hour},
		"machine alone": {LaneMachine: "box-a"},
		"agent alone":   {LaneAgent: "lane-agent"},
		"bad machine":   {LaneMachine: "Box A", LaneAgent: "lane-agent"},
		"vm no size":    {LaneMachine: "vm:@box-a", LaneAgent: "lane-agent"},
		"vm no machine": {LaneMachine: "vm:c2m8", LaneAgent: "lane-agent"},
		"vm bad size":   {LaneMachine: "vm:huge@box-a", LaneAgent: "lane-agent"},
		"vm bad host":   {LaneMachine: "vm:c2m8@Box A", LaneAgent: "lane-agent"},
		"vm two hosts":  {LaneMachine: "vm:c2m8@box-a@box-b", LaneAgent: "lane-agent"},
		"vm run agent":  {LaneMachine: "vm:c2m8@box-a", LaneAgent: "run"},
		"egress alone":  {LaneEgress: []string{"pkg.example.com"}},
		"egress no vm":  {LaneMachine: "box-a", LaneAgent: "lane-agent", LaneEgress: []string{"pkg.example.com"}},
		"egress ip":     {LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: []string{"10.0.0.1"}},
		"egress port":   {LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: []string{"pkg.example.com:443"}},
		"egress star":   {LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: []string{"*"}},
		"egress line":   {LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: []string{"a.example.com\nEGRESS: *"}},
		"egress 33":     {LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: manyHosts(33)},
	} {
		if err := sec.Validate(s, project.File{}); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	// A VM size on a machine (vm:<size>@<machine>): parsed, kept as written.
	v, err = sec.Parse("project.toml", map[string]any{"lane_machine": "vm:c4m16@box-a", "lane_agent": "lane-agent"})
	if err != nil {
		t.Fatal(err)
	}
	vm := v.(askSettings)
	if err := sec.Validate(vm, project.File{}); err != nil {
		t.Fatal(err)
	}
	if m, size := laneTarget(vm.LaneMachine); m != "box-a" || size != "c4m16" || laneWhere(vm) != "a c4m16 VM on box-a" {
		t.Fatalf("vm lane %q %q %q", m, size, laneWhere(vm))
	}
	if m, size := laneTarget("box-a"); m != "box-a" || size != "" || laneWhere(got) != "box-a" {
		t.Fatalf("machine lane %q %q", m, size)
	}
	if enc := sec.Encode(vm); !strings.Contains(enc, `lane_machine = "vm:c4m16@box-a"`) || strings.Contains(enc, "lane_egress") {
		t.Errorf("encode %q", enc)
	}
	// A VM lane's egress hosts (the sandbox task's own rule): parsed, checked,
	// written back as an array.
	v, err = sec.Parse("project.toml", map[string]any{"lane_machine": "vm:c4m16@box-a", "lane_agent": "lane-agent",
		"lane_egress": []any{"pkg.example.com", "*.example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	eg := v.(askSettings)
	if err := sec.Validate(eg, project.File{}); err != nil {
		t.Fatal(err)
	}
	if err := sec.Validate(askSettings{LaneMachine: "vm:c2m8@box-a", LaneAgent: "lane-agent", LaneEgress: manyHosts(32)}, project.File{}); err != nil {
		t.Fatalf("32 hosts: %v", err)
	}
	if enc := sec.Encode(eg); !strings.Contains(enc, `lane_egress = ["pkg.example.com", "*.example.org"]`) {
		t.Errorf("encode %q", enc)
	}
	if (askSettings{LaneEgress: []string{"x.example.com"}}).isZero() || !(askSettings{}).isZero() {
		t.Error("isZero")
	}
	if plainDuration(3*time.Hour) != "3 hours" || plainDuration(90*time.Minute) != "90 minutes" || plainDuration(time.Hour) != "1 hour" {
		t.Error("plainDuration")
	}
}

// manyHosts is n distinct egress host names.
func manyHosts(n int) []string {
	hs := make([]string, n)
	for i := range hs {
		hs[i] = fmt.Sprintf("h%d.example.com", i)
	}
	return hs
}
