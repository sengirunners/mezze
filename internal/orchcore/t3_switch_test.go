package orchcore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The switch: on writes the default checkout (keeping a block the owner
// wrote, and every other key), off removes the key; FillT3 fills only an
// empty block.
func TestT3Switch(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	path := filepath.Join(home, "config.json")
	read := func() map[string]json.RawMessage {
		t.Helper()
		var doc map[string]json.RawMessage
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for k, v := range doc {
			var c bytes.Buffer
			json.Compact(&c, v)
			doc[k] = c.Bytes()
		}
		return doc
	}
	want := `{"checkout":` + string(mustJSON(t, filepath.Join(home, "t3code"))) + `}`

	// No key: FillT3 writes nothing (no file made either).
	if changed, err := FillT3(home); err != nil || changed {
		t.Fatalf("fill without a key: %v %v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("fill made a config.json")
	}
	os.WriteFile(path, []byte(`{"machine":"box","future_key":{"x":1}}`), 0o600)
	if changed, err := SetT3(home, true); err != nil || !changed {
		t.Fatalf("enable: %v %v", changed, err)
	}
	doc := read()
	if string(doc["t3"]) != want || string(doc["machine"]) != `"box"` || string(doc["future_key"]) != `{"x":1}` {
		t.Fatalf("after enable: %s %s %s", doc["t3"], doc["machine"], doc["future_key"])
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if changed, err := SetT3(home, true); err != nil || changed {
		t.Fatalf("enable again: %v %v", changed, err)
	}
	// A block the owner wrote is kept.
	os.WriteFile(path, []byte(`{"t3":{"checkout":"/srv/t3code","port":4000}}`), 0o600)
	if changed, err := SetT3(home, true); err != nil || changed {
		t.Fatalf("enable over a block: %v %v", changed, err)
	}
	// An empty one is filled, its other fields kept.
	os.WriteFile(path, []byte(`{"t3":{"port":4000}}`), 0o600)
	if changed, err := FillT3(home); err != nil || !changed {
		t.Fatalf("fill: %v %v", changed, err)
	}
	var blk map[string]any
	json.Unmarshal(read()["t3"], &blk)
	if blk["checkout"] != filepath.Join(home, "t3code") || blk["port"] != float64(4000) {
		t.Fatalf("filled block %v", blk)
	}
	if changed, err := SetT3(home, false); err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	if _, has := read()["t3"]; has {
		t.Fatal("disable left the key")
	}
	if changed, err := SetT3(home, false); err != nil || changed {
		t.Fatalf("disable again: %v %v", changed, err)
	}
	// A key that is no object is the owner's to fix.
	os.WriteFile(path, []byte(`{"t3":"yes"}`), 0o600)
	if _, err := SetT3(home, true); err == nil {
		t.Fatal("enable over a non-object t3")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
