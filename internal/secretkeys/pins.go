package secretkeys

// Pins (SPEC 17.12): the sender's record of the key it trusts for each
// machine of each project, trust on first use. A key the directory lists is
// new (never pinned), pinned (the one trusted), or changed (another key was
// trusted): a sender seals to a changed key only after a person trusts it
// (zm secret trust). The directory is the authority's word; the pin is the
// sender's own, and its fingerprint is computed here from the key itself
// (Verify), never taken from the listing, so an authority that swaps a
// machine's key is caught on every send after the first.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// PinsFile is the pins' path under $ZM_HOME (0600, in a 0700 directory).
const PinsFile = "secretdrop/pins.json"

// Pin verdicts.
const (
	PinNew     = "new"
	PinPinned  = "pinned"
	PinChanged = "changed"
)

// Verify checks a listed key: a zmkey: id, a 32-byte seal key, and the
// fingerprint those two make. A listing that fails it is refused whole.
func Verify(k orchapi.SecretKey) error {
	pub, err := base64.RawURLEncoding.DecodeString(k.SealKey)
	switch {
	case err != nil || len(pub) != 32:
		return fmt.Errorf("the directory lists a malformed seal key for %s", k.Machine)
	case !strings.HasPrefix(k.KeyID, "zmkey:") || len(k.KeyID) != len("zmkey:")+26:
		return fmt.Errorf("the directory lists a malformed key id for %s", k.Machine)
	case Fingerprint(k.KeyID, pub) != k.Fingerprint:
		return fmt.Errorf("the directory lists a fingerprint for %s that its key does not make: do not trust this listing", k.Machine)
	case k.Project == "" || k.Machine == "":
		return fmt.Errorf("the directory lists a key with no project or machine")
	}
	return nil
}

// Pin is the key a sender trusts for one machine.
type Pin struct {
	Fingerprint string    `json:"fingerprint"`
	KeyID       string    `json:"key_id"`
	Pinned      time.Time `json:"pinned"`
}

type pinsDoc struct {
	// Machines is keyed "<project>/<machine>": a machine name is unique only
	// within its project's authority.
	Machines map[string]Pin `json:"machines"`
}

func pinKey(k orchapi.SecretKey) string { return k.Project + "/" + k.Machine }

// Pins is one machine's pins file.
type Pins struct{ path string }

// OpenPins is the pins file of the zm home at home.
func OpenPins(home string) *Pins { return &Pins{path: filepath.Join(home, PinsFile)} }

// Path is the pins file's path.
func (p *Pins) Path() string { return p.path }

func (p *Pins) read() (pinsDoc, error) {
	doc := pinsDoc{Machines: map[string]Pin{}}
	b, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		// Never treated as empty: that would trust every key anew.
		return doc, fmt.Errorf("%s is not readable pins (%v): fix or remove it; removing it trusts every key anew", p.path, err)
	}
	if doc.Machines == nil {
		doc.Machines = map[string]Pin{}
	}
	return doc, nil
}

// Check is what the pins say of the listed key k (after Verify), and the
// pin held for its machine (zero when new).
func (p *Pins) Check(k orchapi.SecretKey) (string, Pin, error) {
	if err := Verify(k); err != nil {
		return "", Pin{}, err
	}
	doc, err := p.read()
	if err != nil {
		return "", Pin{}, err
	}
	old, ok := doc.Machines[pinKey(k)]
	switch {
	case !ok:
		return PinNew, Pin{}, nil
	case old.Fingerprint == k.Fingerprint:
		return PinPinned, old, nil
	}
	return PinChanged, old, nil
}

// Trust pins the listed key k (after Verify) for its machine, in place of
// any other.
func (p *Pins) Trust(k orchapi.SecretKey, now time.Time) error {
	if err := Verify(k); err != nil {
		return err
	}
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(p.path)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := p.read()
	if err != nil {
		return err
	}
	doc.Machines[pinKey(k)] = Pin{Fingerprint: k.Fingerprint, KeyID: k.KeyID, Pinned: now.UTC()}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pins-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p.path)
}
