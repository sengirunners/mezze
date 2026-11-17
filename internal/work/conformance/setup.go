package conformance

// The setup cases (Options.Setup; SPEC 11 "Setup options, frozen, undo").

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// fileState is what a snapshot records of one path.
type fileState struct {
	mode fs.FileMode
	size int64
	mod  time.Time
}

// snapshot records every path under the dirs (not following symlinks).
func snapshot(dirs []string) map[string]fileState {
	out := map[string]fileState{}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := e.Info(); err == nil {
				out[p] = fileState{mode: info.Mode(), size: info.Size(), mod: info.ModTime()}
			}
			return nil
		})
	}
	return out
}

// changed names what differs between two snapshots ("" when nothing).
func changed(a, b map[string]fileState) string {
	var diff []string
	for p, s := range b {
		if o, ok := a[p]; !ok {
			diff = append(diff, "made "+p)
		} else if o != s {
			diff = append(diff, "changed "+p)
		}
	}
	for p := range a {
		if _, ok := b[p]; !ok {
			diff = append(diff, "removed "+p)
		}
	}
	sort.Strings(diff)
	if len(diff) > 5 {
		diff = append(diff[:5], fmt.Sprintf("and %d more", len(diff)-5))
	}
	return strings.Join(diff, ", ")
}

// setupCall sends setup with args and config and returns the raw response
// (for the secret scan) and the decoded result.
func (r *run) setupCall(args orchapi.AdapterSetupArgs, cfg map[string]string) ([]byte, orchapi.AdapterSetupResult, *api.Error, error) {
	req := orchapi.AdapterRequest{Spec: api.AdapterProtocol, Verb: orchapi.VerbSetup, Config: cfg}
	req.Args, _ = json.Marshal(args)
	in, _ := json.Marshal(req)
	resp, err := r.rt(r.ctx, in)
	if err != nil {
		return nil, orchapi.AdapterSetupResult{}, nil, err
	}
	raw, _ := json.Marshal(resp)
	var out orchapi.AdapterSetupResult
	if !resp.OK {
		if resp.Error == nil {
			return raw, out, nil, fmt.Errorf("ok:false without error.code")
		}
		return raw, out, resp.Error, nil
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return raw, out, nil, fmt.Errorf("result does not decode: %v", err)
	}
	return raw, out, nil, nil
}

func (r *run) setup() {
	if !slices.Contains(r.verbs, orchapi.VerbSetup) {
		r.skip("setup checks", "the adapter has no setup verb")
		return
	}
	var decl []string
	if r.desc.Setup != nil {
		decl = r.desc.Setup.Options
	}
	data, err := os.MkdirTemp("", "zm-conformance-setup-")
	if err != nil {
		r.add("setup checks", err)
		return
	}
	defer os.RemoveAll(data)
	opts := map[string]string{}
	var bad []string
	for k, v := range r.opt.SetupOptions {
		if !slices.Contains(decl, k) {
			bad = append(bad, k)
		}
		opts[k] = v
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		r.add("setup options are declared in describe.setup.options", fmt.Errorf("not declared: %s", strings.Join(bad, ", ")))
		return
	}
	zm := &orchapi.AdapterSetupZM{DataDir: data, IssuePrefix: "conformance", HasPassword: r.opt.HasPassword}
	dirs := []string{data}
	for _, v := range opts {
		if st, err := os.Stat(v); err == nil && st.IsDir() && filepath.IsAbs(v) && !slices.Contains(dirs, v) {
			dirs = append(dirs, v)
		}
	}
	var raws [][]byte
	keep := func(raw []byte) {
		if raw != nil {
			raws = append(raws, raw)
		}
	}

	// A dry run writes nothing.
	before := snapshot(dirs)
	raw, dry, refused, err := r.setupCall(orchapi.AdapterSetupArgs{DryRun: true, Options: opts, ZM: zm}, r.opt.Config)
	keep(raw)
	if err == nil {
		if d := changed(before, snapshot(dirs)); d != "" {
			err = fmt.Errorf("the dry run wrote: %s", d)
		} else if refused != nil {
			err = fmt.Errorf("refused: %s", refused.Error())
		}
	}
	r.add("setup dry run writes nothing", err)
	if err != nil {
		return
	}
	cfg := map[string]string{}
	for k, v := range dry.Config {
		cfg[k] = v
	}
	for k, v := range r.opt.Config { // the caller's config wins, as zm's merge does
		cfg[k] = v
	}

	// Frozen: probe only.
	frozenOpts := map[string]string{}
	for k, v := range opts {
		if k != orchapi.SetupOptAllowMigrate { // zm never sends it on a frozen home
			frozenOpts[k] = v
		}
	}
	before = snapshot(dirs)
	raw, fr, refused, err := r.setupCall(orchapi.AdapterSetupArgs{Options: frozenOpts, ZM: zm, Frozen: true}, cfg)
	keep(raw)
	if err == nil && refused == nil {
		switch d := changed(before, snapshot(dirs)); {
		case d != "":
			err = fmt.Errorf("a frozen setup wrote: %s", d)
		case fr.Created || len(fr.Undo) > 0:
			err = fmt.Errorf("a frozen setup says it created something (created %v, undo %v)", fr.Created, fr.Undo)
		case fr.Schema != nil && fr.Schema.Migrated:
			err = fmt.Errorf("a frozen setup migrated the schema")
		}
	}
	r.add("setup with frozen only probes (writes, creates and migrates nothing)", err)

	// The real setup: undo stays inside data_dir.
	raw, out, refused, err := r.setupCall(orchapi.AdapterSetupArgs{Options: opts, ZM: zm}, cfg)
	keep(raw)
	if err == nil && refused != nil {
		err = fmt.Errorf("refused: %s", refused.Error())
	}
	if err == nil {
		err = undoContained(data, out)
	}
	r.add("setup undo paths stay inside data_dir", err)

	// check, on what setup made, writes nothing either.
	if slices.Contains(r.verbs, orchapi.VerbCheck) {
		for k, v := range out.Config {
			if _, set := cfg[k]; !set {
				cfg[k] = v
			}
		}
		r.checkWritesNothing(cfg, dirs)
	}

	// No result carries the secret.
	if r.opt.Secret == "" {
		r.skip("no setup result carries the secret", "no secret given (zm-work-conformance --secret-env NAME)")
		return
	}
	err = nil
	for _, raw := range raws {
		if strings.Contains(string(raw), r.opt.Secret) {
			err = fmt.Errorf("a setup answer carries the secret")
		}
	}
	r.add("no setup result carries the secret", err)
}

// undoContained checks every undo step: remove_dir of a path that, symlinks
// resolved, is strictly inside data_dir; only with created.
func undoContained(data string, out orchapi.AdapterSetupResult) error {
	if len(out.Undo) > 0 && !out.Created {
		return fmt.Errorf("undo %v without created (zm would never run it)", out.Undo)
	}
	root, err := filepath.EvalSymlinks(data)
	if err != nil {
		return err
	}
	for _, u := range out.Undo {
		if u.Op != orchapi.UndoRemoveDir {
			return fmt.Errorf("unknown undo op %q", u.Op)
		}
		p, err := filepath.EvalSymlinks(u.Path)
		if err != nil {
			return fmt.Errorf("undo path %s: %v", u.Path, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("undo path %s is not strictly inside data_dir %s", p, root)
		}
	}
	return nil
}

// checkWritesNothing runs compareCheck, under Options.Hold when there is one.
func (r *run) checkWritesNothing(cfg map[string]string, dirs []string) {
	check := func() { r.compareCheck(cfg, dirs) }
	if r.opt.Hold == nil {
		check()
	} else if err := r.opt.Hold(check); err != nil {
		r.skip(CheckWritesName, err.Error())
	}
}

// CheckWritesName names the check that check writes nothing.
const CheckWritesName = "check answers and writes nothing"

// SlotHold is an Options.Hold that runs check holding slot, a one-place
// channel that whoever reads the tracker also holds while reading. Senders on
// a channel are served in order, so once check is waiting it goes before the
// reader's next turn, however short the reader's gaps. It gives up without
// running check when ctx ends first (a first full sync can outlast any
// caller), and says why.
func SlotHold(ctx context.Context, slot chan struct{}) func(check func()) error {
	return func(check func()) error {
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("not run: a mirror sync was still reading the tracker when the wait ran out (%v); run zm tracker test once it is done", ctx.Err())
		}
		defer func() { <-slot }()
		check()
		return nil
	}
}

// compareCheck calls check with cfg and compares a snapshot of dirs (and of
// every directory cfg names) before and after.
func (r *run) compareCheck(cfg map[string]string, dirs []string) {
	dirs = slices.Clone(dirs)
	for _, v := range cfg {
		if st, err := os.Stat(v); err == nil && st.IsDir() && filepath.IsAbs(v) && !slices.Contains(dirs, v) {
			dirs = append(dirs, v)
		}
	}
	before := snapshot(dirs)
	req := orchapi.AdapterRequest{Spec: api.AdapterProtocol, Verb: orchapi.VerbCheck, Config: cfg}
	in, _ := json.Marshal(req)
	resp, err := r.rt(r.ctx, in)
	switch {
	case err != nil:
	case !resp.OK && resp.Error == nil:
		err = fmt.Errorf("ok:false without error.code")
	case !resp.OK:
		err = fmt.Errorf("refused: %s", resp.Error.Error())
	default:
		var c orchapi.AdapterCheckResult
		if jerr := json.Unmarshal(resp.Result, &c); jerr != nil {
			err = fmt.Errorf("result does not decode: %v", jerr)
		} else if d := changed(before, snapshot(dirs)); d != "" {
			err = fmt.Errorf("check wrote: %s", d)
		}
	}
	r.add(CheckWritesName, err)
}
