package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// faults are the ways memSetup breaks the setup contract.
type faults struct {
	dryWrites, frozenWrites, undoOutside, undoSymlink, leak, checkWrites bool
}

// memSetup is mem with a setup verb that makes data_dir/db, as a correct
// adapter would, unless f says otherwise.
func memSetup(f faults) map[string]adapter.Handler {
	h := mem()
	h[orchapi.VerbDescribe] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.AdapterDescribe{Spec: api.Spec, Name: "mem", Prefix: "mm", Verbs: append([]string{orchapi.VerbSetup, orchapi.VerbCheck}, orchapi.RequiredVerbs...),
			Setup: &orchapi.AdapterSetupInfo{Touches: []string{"data_dir/db"}, Options: []string{orchapi.SetupOptMode}}}, nil
	}
	h[orchapi.VerbCheck] = func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
		if f.checkWrites && req.Config["dir"] != "" {
			os.WriteFile(filepath.Join(req.Config["dir"], "checked"), nil, 0o600)
		}
		return orchapi.AdapterCheckResult{}, nil
	}
	h[orchapi.VerbSetup] = func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
		a, err := adapter.Args[orchapi.AdapterSetupArgs](req)
		if err != nil {
			return nil, err
		}
		data := a.ZM.DataDir
		out := orchapi.AdapterSetupResult{}
		if f.leak {
			out.Notes = []string{"password is " + os.Getenv("ZM_TEST_CONF_SECRET")}
		}
		if a.DryRun {
			if f.dryWrites {
				os.WriteFile(filepath.Join(data, "dry"), nil, 0o600)
			}
			out.Config = map[string]string{"mode": "x", "dir": data}
			return out, nil
		}
		if a.Frozen && !f.frozenWrites {
			return out, nil
		}
		db := filepath.Join(data, "db")
		if err := os.MkdirAll(db, 0o700); err != nil {
			return nil, err
		}
		out.Created, out.Undo = true, []orchapi.AdapterUndo{{Op: orchapi.UndoRemoveDir, Path: db}}
		switch {
		case f.undoOutside:
			out.Undo[0].Path = os.TempDir()
		case f.undoSymlink:
			link := filepath.Join(data, "link")
			os.Symlink(os.TempDir(), link)
			out.Undo[0].Path = link
		}
		return out, nil
	}
	return h
}

// The daemon's path (no setup): check's own write fails it, held or not; a
// reader of the tracker fails it only when the hold does not keep it out,
// even a reader that takes the slot back with no gap at all; a hold that
// gives up reports a skip with its reason and never calls check.
func TestCheckWritesUnderHold(t *testing.T) {
	want := func(what, line string, res []Result) {
		t.Helper()
		if out := Format(res); !strings.Contains(out, line) {
			t.Errorf("%s: want %q in:\n%s", what, line, out)
		}
	}
	bg := context.Background()
	slot := make(chan struct{}, 1)
	writes := inProcess(memSetup(faults{checkWrites: true}))
	cfg := func() map[string]string { return map[string]string{"dir": t.TempDir()} }
	want("write, no hold", "FAIL  "+CheckWritesName, Run(bg, writes, Options{Config: cfg()}))
	want("write, held", "FAIL  "+CheckWritesName, Run(bg, writes, Options{Config: cfg(), Hold: SlotHold(bg, slot)}))
	if len(slot) != 0 {
		t.Fatal("the hold kept the slot")
	}

	// A reader that touches a file holding the slot, as a sync's bd reads
	// do, and takes it again the moment it lets go.
	reads := cfg()
	file := filepath.Join(reads["dir"], "db")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case slot <- struct{}{}:
			}
			if err := os.WriteFile(file, []byte(strconv.Itoa(i)), 0o600); err != nil {
				t.Error(err)
			}
			<-slot
		}
	}()
	h := memSetup(faults{})
	check := h[orchapi.VerbCheck]
	h[orchapi.VerbCheck] = func(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
		time.Sleep(50 * time.Millisecond) // the reader's time to get in
		return check(ctx, req)
	}
	want("reader, no hold", "FAIL  "+CheckWritesName, Run(bg, inProcess(h), Options{Config: reads}))
	soon, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	for i := range 20 { // a polling hold misses gaps this short
		if out := Format(Run(bg, inProcess(h), Options{Config: reads, Hold: SlotHold(soon, slot)})); !strings.Contains(out, "PASS  "+CheckWritesName) {
			t.Fatalf("reader, held (run %d):\n%s", i, out)
		}
	}
	close(stop)
	<-done

	sent := false
	h[orchapi.VerbCheck] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		sent = true
		return orchapi.AdapterCheckResult{}, nil
	}
	gone, cancelGone := context.WithCancel(bg)
	cancelGone()
	slot <- struct{}{}
	defer func() { <-slot }()
	want("gave up", "SKIP  "+CheckWritesName+": not run: a mirror sync was still reading", Run(bg, inProcess(h), Options{Config: reads, Hold: SlotHold(gone, slot)}))
	if sent {
		t.Fatal("a hold that gave up still called check")
	}
}

func TestSetupCases(t *testing.T) {
	t.Setenv("ZM_TEST_CONF_SECRET", "s3cret-for-conformance")
	opt := Options{Setup: true, Secret: "s3cret-for-conformance"}
	res := Run(context.Background(), inProcess(memSetup(faults{})), opt)
	out := Format(res)
	if n := Failed(res); n > 0 {
		t.Fatalf("a correct setup failed %d:\n%s", n, out)
	}
	for _, want := range []string{"PASS  setup dry run writes nothing", "PASS  setup with frozen only probes", "PASS  setup undo paths stay inside data_dir", "PASS  no setup result carries the secret", "PASS  check answers and writes nothing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
	for name, c := range map[string]struct {
		f    faults
		want string
	}{
		"dry run writes":          {faults{dryWrites: true}, "FAIL  setup dry run writes nothing"},
		"frozen writes":           {faults{frozenWrites: true}, "FAIL  setup with frozen only probes"},
		"undo outside":            {faults{undoOutside: true}, "FAIL  setup undo paths stay inside data_dir"},
		"undo symlink to outside": {faults{undoSymlink: true}, "FAIL  setup undo paths stay inside data_dir"},
		"secret in a note":        {faults{leak: true}, "FAIL  no setup result carries the secret"},
		"check writes":            {faults{checkWrites: true}, "FAIL  check answers and writes nothing"},
	} {
		if out := Format(Run(context.Background(), inProcess(memSetup(c.f)), opt)); !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q in:\n%s", name, c.want, out)
		}
	}
	// Undeclared options are refused before any setup call.
	if out := Format(Run(context.Background(), inProcess(memSetup(faults{})), Options{Setup: true, SetupOptions: map[string]string{"server": "x:1"}})); !strings.Contains(out, "FAIL  setup options are declared") {
		t.Fatalf("undeclared option:\n%s", out)
	}
	// Without Setup (the daemon's tracker test) setup is never sent.
	var verbs []string
	rt := func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		if strings.Contains(string(in), `"verb":"setup"`) {
			verbs = append(verbs, "setup")
		}
		return inProcess(memSetup(faults{}))(ctx, in)
	}
	Run(context.Background(), rt, Options{Write: true})
	if len(verbs) > 0 {
		t.Fatal("a run without Setup called setup")
	}
}
