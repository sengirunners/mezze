// Package exportscrub overwrites a redaction's texts in export snapshots
// (identifiers design, stage 2b part 2): JSON Lines files, of which only
// lines of items filed under the scrub's subjects change, and only comment
// bodies equal to a text; every other line is copied byte for byte.
package exportscrub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/core"
)

// Dir rewrites the snapshots named stem* in dir.
func Dir(dir, stem string, sc core.Scrub) error {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, de := range des {
		n := de.Name()
		if !strings.HasPrefix(n, stem) || !(strings.HasSuffix(n, ".jsonl") || strings.HasSuffix(n, ".jsonl.tmp")) {
			continue
		}
		errs = append(errs, scrubSnapshot(filepath.Join(dir, n), sc))
	}
	return errors.Join(errs...)
}

// scrubSnapshot rewrites one snapshot if any line changes (a temporary
// file, fsynced, renamed over it).
func scrubSnapshot(path string, sc core.Scrub) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	changed := false
	sc2 := bufio.NewScanner(bytes.NewReader(src))
	sc2.Buffer(make([]byte, 0, 1<<20), 1<<30)
	first := true
	for sc2.Scan() {
		line := sc2.Bytes()
		if !first {
			if nl, ok := scrubSnapshotLine(line, sc); ok {
				line, changed = nl, true
			}
		}
		first = false
		out.Write(line)
		out.WriteByte('\n')
	}
	if err := sc2.Err(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	tmp := path + ".scrub"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(out.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// scrubSnapshotLine is line with its comment copies redacted, when it is an
// item filed under one of sc's subjects holding a text exactly in a text
// field (core.ScrubJSON: numbers and fields this zm does not know are kept
// as written).
func scrubSnapshotLine(line []byte, sc core.Scrub) ([]byte, bool) {
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(line, &probe) != nil || !slices.Contains(sc.Subjects, probe.ID) {
		return nil, false
	}
	out, changed := core.ScrubJSON(string(line), sc.Texts)
	if !changed {
		return nil, false
	}
	return []byte(out), true
}
