package orchcore

// This machine's Mezze app switch (SPEC 8 "Mezze app on a machine"): the "t3" key in
// zm's config.json, which only the machine's owner sets (zm t3 enable, or
// by hand). "t3": {} means every default. config.File cannot tell an empty
// block from none and drops it on a Save (omitzero), so an empty block is
// filled with the default checkout: zm t3 enable writes it so, and the team
// module fills a hand-written {} as the daemon starts, before any module
// saves the file.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sengirunners/potluck/kit/config"
)

// T3Checkout is the default t3code checkout, in zm's home.
const T3Checkout = "t3code"

// SetT3 turns this machine's Mezze app switch on (a "t3" key, an existing block
// kept, an empty one filled with the default checkout) or off (the key
// removed), keeping every other key of config.json in home. changed: it
// wrote.
func SetT3(home string, on bool) (changed bool, err error) {
	return rewriteConfig(home, func(doc map[string]json.RawMessage) (bool, error) {
		raw, has := doc["t3"]
		if !on {
			delete(doc, "t3")
			return has, nil
		}
		if has && !isObject(raw) {
			return false, fmt.Errorf("\"t3\" in %s is not an object: fix or remove it", filepath.Join(home, config.FileConfig))
		}
		return fillT3(doc, home)
	})
}

// FillT3 fills a "t3" block that names no checkout ("t3": {}) with the
// default one, so zm's own config writes keep the switch. No key: no
// change.
func FillT3(home string) (changed bool, err error) {
	return rewriteConfig(home, func(doc map[string]json.RawMessage) (bool, error) {
		if raw, has := doc["t3"]; !has || !isObject(raw) {
			return false, nil
		}
		return fillT3(doc, home)
	})
}

func fillT3(doc map[string]json.RawMessage, home string) (bool, error) {
	block := map[string]json.RawMessage{}
	if raw, has := doc["t3"]; has {
		if err := json.Unmarshal(raw, &block); err != nil {
			return false, err
		}
		if c, ok := block["checkout"]; ok && string(bytes.TrimSpace(c)) != `""` && string(bytes.TrimSpace(c)) != "null" {
			return false, nil
		}
	}
	b, err := json.Marshal(filepath.Join(home, T3Checkout))
	if err != nil {
		return false, err
	}
	block["checkout"] = b
	out, err := json.Marshal(block)
	if err != nil {
		return false, err
	}
	doc["t3"] = out
	return true, nil
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// rewriteConfig edits config.json in home as a document of raw keys
// (other zm versions' keys kept) and writes it back, 0600 and atomically,
// when edit says it changed. No file: edited from an empty one.
func rewriteConfig(home string, edit func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	if home == "" {
		return false, nil
	}
	path := filepath.Join(home, config.FileConfig)
	doc := map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	default:
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
	}
	changed, err := edit(doc)
	if err != nil || !changed {
		return false, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return false, err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := path + ".t3.tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
