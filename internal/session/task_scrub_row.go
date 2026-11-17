package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type taskSessionText struct {
	name, value, original string
	valid                 bool
}

// Inspect the schema rather than a field list so a new session text column is
// automatically included in redaction. The primary key is an object identity.
func (s *Service) taskSessionTexts(ctx context.Context, id string) ([]taskSessionText, error) {
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(sessions)`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		if pk == 0 && strings.EqualFold(typ, "TEXT") {
			names = append(names, name)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(names) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
	}
	values := make([]sql.NullString, len(names))
	args := make([]any, len(names))
	for i := range values {
		args[i] = &values[i]
	}
	if err = db.QueryRowContext(ctx, `SELECT `+strings.Join(quoted, ",")+` FROM sessions WHERE id=?`, id).Scan(args...); err != nil {
		return nil, err
	}
	out := make([]taskSessionText, len(names))
	for i, n := range names {
		out[i] = taskSessionText{name: n, value: values[i].String, original: values[i].String, valid: values[i].Valid}
	}
	return out, nil
}

func scrubTaskSessionValue(input string, clean func(string) string) string {
	trim := strings.TrimSpace(input)
	if !strings.HasPrefix(trim, "{") && !strings.HasPrefix(trim, "[") {
		return clean(input)
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return clean(input)
	}
	var walk func(any) any
	walk = func(x any) any {
		switch y := x.(type) {
		case string:
			return clean(y)
		case []any:
			for i := range y {
				y[i] = walk(y[i])
			}
		case map[string]any:
			out := make(map[string]any, len(y))
			for k, v := range y {
				out[clean(k)] = walk(v)
			}
			return out
		}
		return x
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return clean(input)
	}
	return string(b)
}

func (s *Service) writeTaskSessionTexts(ctx context.Context, id string, fields []taskSessionText) error {
	for _, f := range fields {
		if !f.valid || f.value == f.original {
			continue
		}
		name := `"` + strings.ReplaceAll(f.name, `"`, `""`) + `"`
		if _, err := s.d.Store.Registry().ExecContext(ctx, fmt.Sprintf(`UPDATE sessions SET %s=? WHERE id=?`, name), f.value, id); err != nil {
			return err
		}
	}
	return nil
}
