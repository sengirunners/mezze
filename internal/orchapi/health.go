package orchapi

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Health is GET /v0/health as zm reads it: the base's answer and the
// sections zm's modules add to it through health parts (core.Deps
// AddHealthPart in orchcore.Wire), decoded. Only local callers get the parts.
//
// Health's own fields are the parts, each under its JSON name: adding one
// is adding a field.
type Health struct {
	api.Health
	// Backup: this home's off-site backup (SPEC 17.9); Heartbeat: the
	// watchdog heartbeat's last send, next to it.
	Backup    *HomeBackup     `json:"backup"`
	Heartbeat *HeartbeatBrief `json:"heartbeat"`
	// Holders: managed sessions' holder processes (SPEC 8).
	Holders *HolderReport `json:"holders"`
	// CardJudge: ask cards the card judge has held too long (SPEC 16).
	CardJudge *CardJudgeHold `json:"card_judge"`
}

// DriverNotice is health's `drivers` section (wiring keeps it): the bindings
// whose driver the daemon cannot run, as of the last check. Absent when there
// are none. Health's own parts do not name it: the module adds it.
type DriverNotice struct {
	Checked  time.Time `json:"checked"`
	Refused  int       `json:"refused"`
	Problems []string  `json:"problems"`
}

// CardJudgeHold is health's card_judge section: ask cards the card judge
// has not answered for over the notice time, 15 minutes unless ZM_JUDGE_NOTICE_AFTER says otherwise (so it is down, not slow), which reach
// nobody until it does. Absent when there are none.
type CardJudgeHold struct {
	Held   int       `json:"held"`
	Since  time.Time `json:"since"`  // the oldest one's hold
	Reason string    `json:"reason"` // why the oldest is still held
}

// healthPart is one of Health's part fields: its index and JSON name.
type healthPart struct {
	field int
	key   string
}

var healthParts = func() []healthPart {
	var out []healthPart
	t := reflect.TypeFor[Health]()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if key == "" || f.Type.Kind() != reflect.Pointer {
			panic("orchapi.Health." + f.Name + ": a part is a pointer field with a JSON name")
		}
		out = append(out, healthPart{i, key})
	}
	return out
}()

// UnmarshalJSON reads the base's fields into api.Health and each part into
// its field; other parts are dropped, as api.Health drops them.
func (h *Health) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	*h = Health{}
	v := reflect.ValueOf(h).Elem()
	for _, p := range healthParts {
		raw, ok := all[p.key]
		if !ok {
			continue
		}
		delete(all, p.key)
		if err := json.Unmarshal(raw, v.Field(p.field).Addr().Interface()); err != nil {
			return err
		}
	}
	base, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(base, &h.Health)
}

// MarshalJSON is the base's encoding with the parts that are set at the top
// level, as the daemon writes them.
func (h Health) MarshalJSON() ([]byte, error) {
	out := h.Health
	out.Parts = maps.Clone(out.Parts) // the caller's map stays as it was
	v := reflect.ValueOf(h)
	for _, p := range healthParts {
		f := v.Field(p.field)
		if f.IsNil() {
			continue
		}
		b, err := json.Marshal(f.Interface())
		if err != nil {
			return nil, err
		}
		if out.Parts == nil {
			out.Parts = map[string]json.RawMessage{}
		}
		out.Parts[p.key] = b
	}
	return out.MarshalJSON()
}
