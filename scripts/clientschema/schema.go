package main

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
)

// A JSON Schema (2020-12) for a Go type, as encoding/json writes it:
//
//   - in a response body, a struct field is required unless it is omitted
//     when empty (omitempty, omitzero) or sits under an embedded pointer: a
//     required field is always present;
//   - a field that can be written as null (a pointer, slice, map or []byte
//     without omitempty) allows null;
//   - no schema sets additionalProperties or enum: a newer server adds
//     fields and values within potluck/1, and a client validating with an
//     older copy must not reject them (docs/clients.md, "Stability").
//
// Request bodies and event data require nothing, so their types are
// components of their own (mode). A request field the client leaves out
// decodes as its zero value. Event data may be older than a field (replayed
// from a log, or written by another machine's daemon), and the bus withholds
// some keys from some readers (owner-only, members-only, authority-only).
type schema = map[string]any

// mode is where a type is used: what its fields require and its name's suffix.
type mode int

const (
	response mode = iota // fields not omitted when empty are required
	request              // nothing required; "Input" unless the name ends in "Request"
	data                 // event data: nothing required; "Data"
)

func (m mode) suffix(name string) string {
	switch {
	case m == request && !strings.HasSuffix(name, "Request"):
		return name + "Input"
	case m == data:
		return name + "Data"
	}
	return name
}

type gen struct {
	defs   map[string]schema
	names  map[typeMode]string
	byName map[string]typeMode
	errs   []string
}

type typeMode struct {
	t reflect.Type
	m mode
}

func newGen() *gen {
	return &gen{defs: map[string]schema{}, names: map[typeMode]string{}, byName: map[string]typeMode{}}
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	rawType       = reflect.TypeFor[json.RawMessage]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	textType      = reflect.TypeFor[encoding.TextMarshaler]()
)

// ref is the schema of t: a $ref for a named struct (its component made on
// first use), else inline.
func (g *gen) ref(t reflect.Type, m mode) schema {
	switch {
	case t == timeType:
		return schema{"type": "string", "format": "date-time"}
	case t == rawType:
		return schema{} // any JSON value
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
		_, custom := customTypes[t]
		switch {
		case custom:
		case t.Implements(marshalerType) || reflect.PointerTo(t).Implements(marshalerType):
			g.errs = append(g.errs, fmt.Sprintf("%s has its own MarshalJSON: describe it in customTypes", t))
			return schema{}
		case t.Implements(textType):
			return schema{"type": "string"}
		case reflect.PointerTo(t).Implements(textType):
			// Used only when the value is addressable: a string or not,
			// depending on how it is reached.
			g.errs = append(g.errs, fmt.Sprintf("%s has MarshalText on its pointer: describe it in customTypes", t))
			return schema{}
		}
	}
	switch t.Kind() {
	case reflect.Bool:
		return schema{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return schema{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return schema{"type": "number"}
	case reflect.String:
		return schema{"type": "string"}
	case reflect.Interface:
		return schema{}
	case reflect.Pointer:
		return g.ref(t.Elem(), m)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return schema{"type": "string", "contentEncoding": "base64"}
		}
		return schema{"type": "array", "items": g.ref(t.Elem(), m)}
	case reflect.Map:
		return schema{"type": "object", "additionalProperties": g.ref(t.Elem(), m)}
	case reflect.Struct:
		if t.Name() == "" {
			return g.object(t, m)
		}
		return schema{"$ref": "#/components/schemas/" + g.component(t, m)}
	}
	g.errs = append(g.errs, fmt.Sprintf("%s: no JSON form", t))
	return schema{}
}

// component names t's schema in mode m and makes it once.
func (g *gen) component(t reflect.Type, m mode) string {
	k := typeMode{t, m}
	if n, ok := g.names[k]; ok {
		return n
	}
	n := t.Name()
	if o, ok := nameOverrides[t]; ok {
		n = o
	}
	if i := strings.IndexByte(n, '['); i >= 0 { // a generic type's instance
		n = n[:i] + "Of" + strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, n[i:])
	}
	n = m.suffix(n)
	if other, ok := g.byName[n]; ok {
		g.errs = append(g.errs, fmt.Sprintf("%s and %s are both named %s: add one to nameOverrides", other.t, t, n))
	}
	g.names[k], g.byName[n] = n, k
	g.defs[n] = nil // placeholder: a recursive type refers to itself
	s := g.object(t, m)
	if c, ok := customTypes[t]; ok && c.adjust != nil {
		c.adjust(s)
	}
	g.defs[n] = s
	return n
}

// object is a struct's schema: its JSON fields as properties.
func (g *gen) object(t reflect.Type, m mode) schema {
	props := schema{}
	var required []string
	for _, f := range jsonFields(t) {
		var s schema
		if f.asString {
			s = schema{"type": "string"}
		} else {
			s = g.ref(f.typ, m)
		}
		if f.nullable() {
			s = nullable(s)
		}
		props[f.name] = s
		if m == response && !f.omit && !f.underPtr {
			required = append(required, f.name)
		}
	}
	out := schema{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// nullable lets s also be null.
func nullable(s schema) schema {
	if typ, ok := s["type"].(string); ok {
		c := schema{}
		for k, v := range s {
			c[k] = v
		}
		c["type"] = []string{typ, "null"}
		return c
	}
	if len(s) == 0 {
		return s // any already allows null
	}
	return schema{"anyOf": []schema{s, {"type": "null"}}}
}

type jsonField struct {
	name     string
	typ      reflect.Type
	omit     bool // omitempty or omitzero: absent when empty, never null
	underPtr bool // under an embedded pointer: absent when that is nil
	asString bool // ",string" on a number or bool (or an unnamed pointer to one)
	depth    int
	tagged   bool
}

// nullable: encoding/json can write the field as null.
func (f jsonField) nullable() bool {
	if f.omit {
		return false
	}
	switch f.typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return f.typ != rawType
	}
	return false
}

// jsonFields are t's fields as encoding/json writes them (its typeFields):
// embedded structs flattened, the shallowest name winning (a tagged one on a
// tie at one depth, else neither); a struct embedded twice at one depth
// contributes none of its fields there.
func jsonFields(t reflect.Type) []jsonField {
	var all []jsonField
	type level struct {
		t        reflect.Type
		underPtr bool
	}
	current, next := []level{}, []level{{t, false}}
	visited := map[reflect.Type]bool{}
	for depth := 0; len(next) > 0; depth++ {
		current, next = next, nil
		count := map[reflect.Type]int{}
		for _, l := range current {
			count[l.t]++
		}
		for _, l := range current {
			if visited[l.t] {
				continue
			}
			visited[l.t] = true
			for i := range l.t.NumField() {
				sf := l.t.Field(i)
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				ft := sf.Type
				if sf.Anonymous {
					et := ft
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue
					}
					if name == "" && et.Kind() == reflect.Struct {
						next = append(next, level{et, l.underPtr || ft.Kind() == reflect.Pointer})
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				f := jsonField{name: name, typ: ft, depth: depth, tagged: name != "", underPtr: l.underPtr}
				if name == "" {
					f.name = sf.Name
				}
				for _, o := range strings.Split(opts, ",") {
					switch o {
					case "omitempty", "omitzero":
						f.omit = true
					case "string":
						qt := ft
						if qt.Name() == "" && qt.Kind() == reflect.Pointer {
							qt = qt.Elem()
						}
						switch qt.Kind() {
						case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
							reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
							reflect.Float32, reflect.Float64:
							f.asString = true
						}
					}
				}
				all = append(all, f)
				if count[l.t] > 1 { // a second copy: the tie below drops both
					all = append(all, f)
				}
			}
		}
	}
	byName := map[string][]jsonField{}
	var order []string
	for _, f := range all {
		if _, ok := byName[f.name]; !ok {
			order = append(order, f.name)
		}
		byName[f.name] = append(byName[f.name], f)
	}
	var out []jsonField
	for _, n := range order {
		fs := byName[n]
		top := fs[0].depth
		for _, f := range fs {
			top = min(top, f.depth)
		}
		var win []jsonField
		for _, f := range fs {
			if f.depth == top {
				win = append(win, f)
			}
		}
		if len(win) > 1 {
			var tagged []jsonField
			for _, f := range win {
				if f.tagged {
					tagged = append(tagged, f)
				}
			}
			win = tagged
		}
		if len(win) == 1 {
			out = append(out, win[0])
		}
	}
	return out
}
