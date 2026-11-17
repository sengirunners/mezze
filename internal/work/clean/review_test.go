package clean

import (
	"encoding/json"
	"github.com/sengirunners/mezze/internal/orchapi"
	"reflect"
	"strings"
	"testing"
)

func TestReview2TrackerFields(t *testing.T) {
	it := orchapi.WorkItem{Title: "\u200b", Owner: "x\u200by", Labels: []string{"\u200b", "ab", "a\u200bb"}, Extra: map[string]any{"a\u200bb": "value"}}
	Item(&it)
	if it.Title != "(untitled)" || it.Owner != "xy" || len(it.Labels) != 1 || it.Labels[0] != "ab" || len(it.Extra) != 1 || it.Extra["ab"] != "value" {
		t.Fatalf("item %+v", it)
	}
}

// A newly added text-bearing item field must be cleaned or explicitly
// excluded as a routing identifier or an enum.
func TestReview2ItemFieldAudit(t *testing.T) {
	typ := reflect.TypeOf(orchapi.WorkItem{})
	exclude := map[string]string{"ID": "item id", "Project": "project id", "Area": "scope", "Source": "tracker id", "Status": "status enum", "Write": "cleared: only a replica sets it"}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if exclude[f.Name] != "" {
			continue
		}
		it := orchapi.WorkItem{Title: "title"}
		v := reflect.ValueOf(&it).Elem().Field(i)
		switch {
		case v.Kind() == reflect.String:
			v.SetString("x\u200by")
		case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String:
			v.Set(reflect.ValueOf([]string{"x\u200by"}))
		case v.Kind() == reflect.Map:
			v.Set(reflect.ValueOf(map[string]any{"x\u200by": "x\u200by"}))
		default:
			continue
		}
		Item(&it)
		b, err := json.Marshal(it)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "\u200b") {
			t.Errorf("%s survives: %s", f.Name, b)
		}
	}
}

// An adapter's item never carries a replica outbox write id; one
// decoded from tracker JSON is cleared (callers wait on a non-empty Write).
func TestItemClearsTrackerWrite(t *testing.T) {
	var it orchapi.WorkItem
	if err := json.Unmarshal([]byte(`{"id":"bd:x","title":"t","write":"x\u200by"}`), &it); err != nil || it.Write == "" {
		t.Fatalf("decode %+v %v", it, err)
	}
	Item(&it)
	if it.Write != "" {
		t.Fatalf("write %q survives", it.Write)
	}
}
