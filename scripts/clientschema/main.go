// Command clientschema writes docs/schema/openapi.json: the client contract
// (docs/clients.md) as one OpenAPI 3.1 document, generated from the Go types
// the stable routes read and answer with and the dev.zm.* events carry.
// Its components are JSON Schema 2020-12 (schema.go says how a Go type maps).
//
//	go run ./scripts/clientschema   # from the repository root, after a shape change
//
// TestSchemaIsCurrent fails when the committed file differs from this
// output: a shape change is a diff of that file plus a "Clients" line in
// CHANGELOG.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// schemaFile is the committed document, from the repository root.
const schemaFile = "docs/schema/openapi.json"

// releasedFile is the schema's floor, as of the last time a release of the
// client contract was cut: TestNoNewRequiredField compares required lists
// against it (and against main's copy when git has main), so a field added
// within potluck/1 cannot become required. Refresh it only to cut a release
// (go run ./scripts/clientschema -released), never with a shape change.
const releasedFile = "docs/schema/openapi.released.json"

func main() {
	out := flag.String("o", schemaFile, "where to write the document")
	released := flag.Bool("released", false, "write the document as the last release's (releasedFile) instead")
	flag.Parse()
	if *released {
		*out = releasedFile
	}
	b, err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientschema:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "clientschema:", err)
		os.Exit(1)
	}
}

const docDescription = `The stable routes and the dev.zm.* event payloads of a zm daemon (docs/clients.md). ` +
	`Generated from the Go types by scripts/clientschema; never edit it by hand. ` +
	`In a response body a required field is always present, any other may be absent, and a field added within ` +
	`potluck/1 is never required. No schema forbids unknown properties or lists a string's values: a newer daemon ` +
	`adds both, so validate with any JSON Schema 2020-12 validator and ignore what you do not know. ` +
	`Event data (the ...Data schemas) and request bodies (...Request, ...Input) require nothing: an event may be ` +
	`older than a field or carry fewer keys for some readers, and an absent request field is its zero value. The ` +
	`daemon refuses a request field it does not know (400). Query parameters, headers and guards are in API.md. ` +
	`Each event's data is the schema named by its type under components.schemas.`

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

func generate() ([]byte, error) {
	g := newGen()
	paths := map[string]map[string]any{}
	for _, r := range routes {
		op := map[string]any{"operationId": operationID(r.method, r.path)}
		if r.note != "" {
			op["description"] = r.note
		}
		var params []any
		for _, m := range pathParam.FindAllStringSubmatch(r.path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": schema{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		if r.in != nil {
			op["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": g.ref(r.in, request)}}}
		}
		resps := map[string]any{"default": map[string]any{"$ref": "#/components/responses/Error"}}
		for status, ts := range r.ok {
			var alts []schema
			for _, rt := range ts {
				alts = append(alts, g.ref(rt, response))
			}
			body := alts[0]
			if len(alts) > 1 {
				body = schema{"anyOf": alts}
			}
			content := map[string]any{}
			switch {
			case r.stream && r.path == "/v0/events":
				content["text/event-stream"] = map[string]any{"schema": body}
			case r.stream:
				content["application/json"] = map[string]any{"schema": body}
				content["text/event-stream"] = map[string]any{"schema": schema{"description": agentLine}}
			default:
				content["application/json"] = map[string]any{"schema": body}
			}
			desc := http.StatusText(status)
			if status == http.StatusAccepted && r.pending != "" {
				desc = r.pending
			}
			resps[strconv.Itoa(status)] = map[string]any{"description": desc, "content": content}
		}
		for status, f := range r.refuse {
			errData := schema{} // none, or untyped
			var alts []schema
			for _, d := range f.data {
				alts = append(alts, g.ref(d, data))
			}
			switch len(alts) {
			case 0:
			case 1:
				errData = alts[0]
			default:
				errData = schema{"anyOf": alts}
			}
			body := schema{"allOf": []schema{{"$ref": "#/components/schemas/ErrorResponse"},
				{"properties": schema{"error": schema{"properties": schema{"data": errData}}}}}}
			resps[strconv.Itoa(status)] = map[string]any{"description": f.why,
				"content": map[string]any{"application/json": map[string]any{"schema": body}}}
		}
		op["responses"] = resps
		p := paths[r.path]
		if p == nil {
			p = map[string]any{}
			paths[r.path] = p
		}
		p[strings.ToLower(r.method)] = op
	}
	g.ref(of[orchapi.AgentEvent](), data) // what zm's own adapters write (agentLine)
	envelope := g.ref(of[api.Event](), response)
	for _, e := range events {
		payload := schema{}
		if e.data != nil {
			payload = g.ref(e.data, data)
			if nullData[e.typ] {
				payload = nullable(payload)
			}
		}
		s := schema{"allOf": []schema{envelope}, "properties": schema{"type": schema{"const": e.typ}, "data": payload}}
		if e.why != "" {
			s["description"] = e.why
		}
		if _, dup := g.defs[e.typ]; dup {
			return nil, fmt.Errorf("event %s listed twice", e.typ)
		}
		g.defs[e.typ] = s
	}
	// health's drivers section is added by the wiring module, not by a field of
	// orchapi.Health (its parts are zm's own): said here, as optional.
	if h, ok := g.defs["Health"]["properties"].(schema); ok {
		h["drivers"] = g.ref(of[orchapi.DriverNotice](), response)
	}
	errBody := g.ref(of[api.ErrorResponse](), response)
	if len(g.errs) > 0 {
		sort.Strings(g.errs)
		return nil, fmt.Errorf("%s", strings.Join(g.errs, "; "))
	}
	doc := map[string]any{
		"openapi":           "3.1.0",
		"jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema",
		"info": map[string]any{
			"title":       "zm client contract",
			"version":     api.WireProtocol,
			"description": docDescription,
		},
		"paths": paths,
		"components": map[string]any{
			"schemas": g.defs,
			"responses": map[string]any{"Error": map[string]any{
				"description": "An error: branch on the status and error.code (API.md \"Errors\"), never on the message",
				"content":     map[string]any{"application/json": map[string]any{"schema": errBody}},
			}},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// operationID is "post_v0_asks_id_choose" for POST /v0/asks/{id}/choose.
func operationID(method, path string) string {
	return strings.ToLower(method) + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(path)
}
