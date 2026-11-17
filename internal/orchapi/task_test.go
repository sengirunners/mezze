package orchapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

func TestSandboxFieldsRoundTrip(t *testing.T) {
	for _, v := range []any{TaskStartRequest{}, Task{}, api.MachineStats{}} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "sandbox") {
			t.Fatalf("zero %T carries sandbox: %s", v, b)
		}
	}
	var r TaskStartRequest
	if err := json.Unmarshal([]byte(`{"prompt":"p","agent":"claude","sandbox":{"size":"c2m8"}}`), &r); err != nil || r.Sandbox == nil || r.Sandbox.Size != "c2m8" {
		t.Fatalf("request: %v %+v", err, r)
	}
	b, _ := json.Marshal(TaskStartRequest{Sandbox: &TaskSandbox{}})
	if !strings.Contains(string(b), `"sandbox":{}`) {
		t.Fatalf("empty size: %s", b)
	}
	var old TaskStartRequest
	if err := json.Unmarshal([]byte(`{"prompt":"p","agent":"claude"}`), &old); err != nil || old.Sandbox != nil {
		t.Fatalf("old request: %v %+v", err, old)
	}
	b, _ = json.Marshal(Task{Sandbox: &TaskSandbox{Size: "c4m16"}})
	var tk Task
	if err := json.Unmarshal(b, &tk); err != nil || tk.Sandbox == nil || tk.Sandbox.Size != "c4m16" {
		t.Fatalf("task: %v %s", err, b)
	}
	b, _ = json.Marshal(api.MachineStats{SandboxTasks: true, SandboxSlots: 2})
	var ms api.MachineStats
	if err := json.Unmarshal(b, &ms); err != nil || !ms.SandboxTasks || ms.SandboxSlots != 2 {
		t.Fatalf("stats: %v %s", err, b)
	}
}

// A sandbox task's egress hosts reach the driver only as host names: an
// address, a bare or inner *, a port, a path or a line break is refused
// before the line (a bad entry must never become a header line or an open
// door in the driver).
func TestCheckTaskSandboxEgress(t *testing.T) {
	ok := func(hosts ...string) TaskStartRequest {
		return TaskStartRequest{Agent: "claude", Prompt: "p", Sandbox: &TaskSandbox{Egress: hosts}}
	}
	if err := CheckTaskSandbox(ok("pkg.example.com", "*.cdn.example.com", "Registry.Example.ORG", "a-b.c1.io", "a.0xy")); err != nil {
		t.Fatalf("valid hosts refused: %v", err)
	}
	for _, h := range []string{"", "*", "*.com", "localhost", "127.0.0.1", "127.1", "10.0.0.1", "0x7f.0x1", "0x7f.0.0.0x1", "a.0X7F", "a.0x", "example.com:443", "https://example.com",
		"example.com/x", "a.com,b.com", "a.com\nEGRESS: evil.com", "a.com\r", " a.com", "a*.example.com", "x.*.example.com", "-a.com", "a-.com",
		"[::1]", "a..com", "a.com.", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 130) + "com"} {
		err := CheckTaskSandbox(ok(h))
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%q: %v (want bad_request)", h, err)
		}
	}
	many := make([]string, maxEgress+1)
	for i := range many {
		many[i] = "example.com"
	}
	if err := CheckTaskSandbox(ok(many...)); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many hosts: %v", err)
	}
	var r TaskStartRequest
	if err := json.Unmarshal([]byte(`{"prompt":"p","sandbox":{"egress":["pkg.example.com"]}}`), &r); err != nil || r.Sandbox == nil || len(r.Sandbox.Egress) != 1 {
		t.Fatalf("egress round trip: %v %+v", err, r)
	}
}
