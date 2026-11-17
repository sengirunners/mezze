package main

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A gate that names only the PR must fail a decline receipt: the decision
// defaults to approve unless --any-decision or --signature-only.
func TestGateExpect(t *testing.T) {
	pr := orchapi.ReceiptExpect{Repo: "a/b", PR: 1, HeadSHA: "x"}
	if e, err := gateExpect(pr, false, false); err != nil || e.Decision != orchapi.DecisionApprove {
		t.Fatalf("default gate %+v %v", e, err)
	}
	d := pr
	d.Decision = "decline"
	if e, _ := gateExpect(d, false, false); e.Decision != "decline" {
		t.Fatalf("explicit decision %+v", e)
	}
	if e, _ := gateExpect(pr, false, true); e.Decision != "" {
		t.Fatalf("any decision %+v", e)
	}
	if _, err := gateExpect(d, false, true); err == nil {
		t.Fatal("--decision with --any-decision accepted")
	}
	if _, err := gateExpect(orchapi.ReceiptExpect{}, false, false); err == nil {
		t.Fatal("gate without repo, pr, head accepted")
	}
	if e, err := gateExpect(orchapi.ReceiptExpect{}, true, false); err != nil || e.Decision != "" {
		t.Fatalf("signature only %+v %v", e, err)
	}
}

// A gate may name a work item or question instead of a pull request, never both.
func TestGateExpectSubjects(t *testing.T) {
	for name, tc := range map[string]struct {
		exp orchapi.ReceiptExpect
		ok  bool
	}{
		"nothing":           {orchapi.ReceiptExpect{}, false},
		"half a pr":         {orchapi.ReceiptExpect{Repo: "a/b"}, false},
		"a pr":              {orchapi.ReceiptExpect{Repo: "a/b", PR: 1, HeadSHA: "x"}, true},
		"a work item alone": {orchapi.ReceiptExpect{Work: "bd:zm-1"}, false},
		"a work ask":        {orchapi.ReceiptExpect{Work: "bd:zm-1", Ask: "ASK"}, true},
		"a work card":       {orchapi.ReceiptExpect{Work: "bd:zm-1", Card: "CARD"}, true},
		"a question":        {orchapi.ReceiptExpect{Question: "abc"}, true},
		"pr and work":       {orchapi.ReceiptExpect{Repo: "a/b", PR: 1, HeadSHA: "x", Work: "bd:zm-1"}, false},
		"repo and qst":      {orchapi.ReceiptExpect{Repo: "a/b", Question: "abc"}, false},
	} {
		got, err := gateExpect(tc.exp, false, false)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && got.Decision != orchapi.DecisionApprove {
			t.Errorf("%s: the decision must default to approve: %q", name, got.Decision)
		}
	}
}
