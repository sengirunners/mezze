package automations

// What a record may hold (SPEC 18.6 "The record"): sizes, typed settings,
// and one validator per kind for its fields. zm does not compile schedules
// or parse YAML: one compiler exists, in the Mezze app.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Bounds.
const (
	maxName        = 200
	maxOwner       = 200
	maxFields      = 32 << 10
	maxBody        = 64 << 10
	maxPath        = 1000
	maxSettings    = 50
	maxSettingsMap = 3500 // the {key: value} map as json.Marshal writes it
	maxHelp        = 500
	maxOptions     = 50
	maxOption      = 200
	maxApplyError  = 300
)

var (
	homeRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	installRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)
	taskIDRe  = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
	keyRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	statusRe  = regexp.MustCompile(`^[a-z_-]{0,40}$`)
)

func bad(format string, args ...any) error { return api.Errorf(api.CodeBadRequest, format, args...) }

// line checks a one-line text of at most max bytes (empty allowed when
// !required).
func line(what, s string, max int, required bool) error {
	switch {
	case required && strings.TrimSpace(s) == "":
		return bad("%s is required", what)
	case len(s) > max:
		return bad("%s is over %d bytes", what, max)
	case !utf8.ValidString(s):
		return bad("%s is not UTF-8", what)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return bad("%s must be one line of text", what)
		}
	}
	return nil
}

func validHome(home string) error {
	if !homeRe.MatchString(home) {
		return bad("home must be a machine name (letters, digits, '.', '_', '-'; at most 100)")
	}
	return nil
}

func validInstall(install string) error {
	if install == "" {
		return bad("name the Mezze app install that holds the home's claim (install)")
	}
	if !installRe.MatchString(install) {
		return bad("install must be letters, digits, '.', '_', ':' or '-' (at most 100)")
	}
	return nil
}

func validStore(s string) error {
	if s != orchapi.AutomationStoreZm && s != orchapi.AutomationStoreZ3 {
		return bad("store must be zm or z3")
	}
	return nil
}

func validBody(b string) error {
	if len(b) > maxBody {
		return bad("body is over %d bytes", maxBody)
	}
	if !utf8.ValidString(b) {
		return bad("body is not UTF-8")
	}
	return nil
}

// topLevel are the record's facts kept once, at the top level, never in
// fields (the Mezze app's file surface merges them for display and splits them on save).
var topLevel = []string{"name", "enabled", "owner", "settings"}

// kinds are the record kinds zm accepts, each with its fields' validator
// (SPEC 18.6 "Kinds"). Stage 4 adds loop.
var kinds = map[string]func(map[string]any) error{
	orchapi.AutomationKindAutomation: automationFields,
}

// secretKeys are the keys a webhook's signature map may not hold: a
// signing secret stays in the Mezze app and the door, never in the record.
var secretKeys = []string{"secret", "secret-ref", "secretRef", "secret_ref"}

// automationFields is the automation kind's check: the file's frontmatter
// as written, intent not the Mezze app's compiled form; only what zm itself must refuse
// is checked.
func automationFields(f map[string]any) error {
	on, ok := f["on"].(map[string]any)
	if !ok {
		if v, has := f["on"]; has && v != nil {
			return bad("fields.on must be an object")
		}
		return nil
	}
	hook, ok := on["webhook"].(map[string]any)
	if !ok {
		return nil
	}
	for _, where := range []struct {
		name string
		m    map[string]any
	}{{"fields.on.webhook", hook}, {"fields.on.webhook.signature", asMap(hook["signature"])}} {
		for _, k := range secretKeys {
			if _, has := where.m[k]; has {
				return bad("%s.%s: a webhook's signing secret stays in the Mezze app (zm.automationSecretSet on the home); the record holds only the signature's shape", where.name, k)
			}
		}
	}
	return nil
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// validKind is kind, defaulted, if zm knows it.
func validKind(kind string) (string, error) {
	if kind == "" {
		kind = orchapi.AutomationKindAutomation
	}
	if kinds[kind] == nil {
		return "", bad("unknown kind %q (known: automation)", kind)
	}
	return kind, nil
}

// validFields is raw compacted, if it is a JSON object within bounds that
// kind's validator takes.
func validFields(kind string, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, bad("fields must be a JSON object: %v", err)
	}
	if buf.Len() > maxFields {
		return nil, bad("fields are over %d bytes", maxFields)
	}
	var f map[string]any
	if err := json.Unmarshal(buf.Bytes(), &f); err != nil || f == nil {
		return nil, bad("fields must be a JSON object")
	}
	for _, k := range topLevel {
		if _, has := f[k]; has {
			return nil, bad("fields.%s: %s is a top-level key of the record, never inside fields", k, k)
		}
	}
	if err := kinds[kind](f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// validSettings checks typed settings: at most maxSettings keys, each value
// of its type, and the {key: value} map within maxSettingsMap bytes as
// json.Marshal writes it (it reaches a script as one argument). Values are
// stored compacted.
func validSettings(in map[string]orchapi.AutomationSetting) (map[string]orchapi.AutomationSetting, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxSettings {
		return nil, bad("settings: at most %d keys", maxSettings)
	}
	out := make(map[string]orchapi.AutomationSetting, len(in))
	values := make(map[string]any, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := in[k]
		if !keyRe.MatchString(k) {
			return nil, bad("settings: key %q must be letters, digits, '_', '.' or '-' (at most 64)", k)
		}
		if err := line("settings."+k+".help", s.Help, maxHelp, false); err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(s.Value))
		dec.UseNumber()
		var v any
		if len(bytes.TrimSpace(s.Value)) == 0 || dec.Decode(&v) != nil || v == nil {
			return nil, bad("settings.%s: value is required", k)
		}
		ok := false
		switch s.Type {
		case "string":
			_, ok = v.(string)
		case "number":
			n, isNum := v.(json.Number)
			f, err := n.Float64()
			ok = isNum && err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
		case "boolean":
			_, ok = v.(bool)
		case "enum":
			str, isStr := v.(string)
			if len(s.Options) == 0 || len(s.Options) > maxOptions {
				return nil, bad("settings.%s: an enum lists 1 to %d options", k, maxOptions)
			}
			seen := map[string]bool{}
			for _, o := range s.Options {
				if err := line("settings."+k+" option", o, maxOption, true); err != nil {
					return nil, err
				}
				if seen[o] {
					return nil, bad("settings.%s: option %q twice", k, o)
				}
				seen[o] = true
			}
			ok = isStr && seen[str]
		default:
			return nil, bad("settings.%s: type must be string, number, boolean or enum (no secret type: secrets go through grants)", k)
		}
		if s.Type != "enum" && len(s.Options) > 0 {
			return nil, bad("settings.%s: only an enum has options", k)
		}
		if !ok {
			return nil, bad("settings.%s: value is not a valid %s", k, s.Type)
		}
		compact, _ := json.Marshal(v)
		s.Value = compact
		out[k] = s
		values[k] = v
	}
	b, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if len(b) > maxSettingsMap {
		return nil, bad("settings: the {key: value} map is %d bytes; at most %d (it reaches a script as one argument)", len(b), maxSettingsMap)
	}
	return out, nil
}

// mergePatch applies an RFC 7396 JSON merge patch to target (a JSON
// document; empty is null). Numbers keep their digits (a 64-bit id is not
// rounded through a float) and a patch that changes nothing answers target
// as written, so it makes no revision.
func mergePatch(target, patch json.RawMessage) (json.RawMessage, error) {
	p, err := decodeExact(patch)
	if err != nil {
		return nil, err
	}
	var t any
	if len(bytes.TrimSpace(target)) > 0 {
		if t, err = decodeExact(target); err != nil {
			return nil, err
		}
	}
	before, err := encodeExact(t)
	if err != nil {
		return nil, err
	}
	out, err := encodeExact(merge(t, p))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(target)) > 0 && bytes.Equal(before, out) {
		return target, nil
	}
	return out, nil
}

// decodeExact decodes one JSON value with numbers as json.Number.
func decodeExact(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// encodeExact encodes v without HTML escaping or a trailing newline.
func encodeExact(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func merge(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = merge(tm[k], v)
	}
	return tm
}

func validHealth(h *orchapi.AutomationHealth) error {
	if err := validInstall(h.Install); err != nil {
		return err
	}
	switch h.State {
	case orchapi.AutomationHealthy, orchapi.AutomationDegraded, orchapi.AutomationPaused, orchapi.AutomationUnapplied, orchapi.AutomationConflict:
	default:
		return bad("state must be healthy, degraded, paused, unapplied or conflict")
	}
	switch h.Secret {
	case "", "none", "set", "missing":
	default:
		return bad("secret must be none, set or missing")
	}
	if h.AppliedRev < 0 || h.FailureStreak < 0 {
		return bad("applied_rev and failure_streak are never negative")
	}
	if len(h.ApplyError) > maxApplyError || !utf8.ValidString(h.ApplyError) {
		return bad("apply_error is at most %d bytes of text (never a run's error text or output)", maxApplyError)
	}
	if !statusRe.MatchString(h.LastRunStatus) {
		return bad("last_run_status is a status word (at most 40 of a-z, '_', '-'), never a run's text")
	}
	for _, t := range []struct{ name, v string }{{"last_run_at", h.LastRunAt}, {"next_run_at", h.NextRunAt}, {"as_of", h.AsOf}} {
		if t.v == "" && t.name != "as_of" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, t.v); err != nil {
			return bad("%s must be an RFC 3339 time", t.name)
		}
	}
	return nil
}
