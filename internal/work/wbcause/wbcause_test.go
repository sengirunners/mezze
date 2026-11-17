package wbcause

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func TestData(t *testing.T) {
	it := orchapi.WorkItem{ID: "bd:solver-1", Status: orchapi.WorkClaimed}
	b, _ := json.Marshal(Data(it, Of("mini/aaaaaaaa", time.UnixMilli(42))))
	var back orchapi.WorkItem
	if !strings.Contains(string(b), `"`+Key+`":"mini/aaaaaaaa 42"`) || json.Unmarshal(b, &back) != nil || back.ID != it.ID || back.Status != it.Status {
		t.Fatalf("marked data %s", b)
	}
	if b, _ = json.Marshal(Data(it, "")); strings.Contains(string(b), Key) {
		t.Fatalf("unmarked data %s", b)
	}
}
