package issues

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// Every new text-bearing request field must pass this audit or name why it is
// an identifier, scope, or concurrency token that cannot be rewritten.
func TestReview2MutationFieldAudit(t *testing.T) {
	exclusions := map[string]string{"ID": "caller-minted id", "Area": "authorization scope", "Parent": "issue id", "Deps": "issue ids", "Base": "version token"}
	cases := []struct {
		name     string
		value    any
		mutation func(any) istore.Mutation
	}{
		{"create", &orchapi.NewIssueRequest{}, func(v any) istore.Mutation { return istore.Mutation{Create: v.(*orchapi.NewIssueRequest)} }},
		{"update", &orchapi.UpdateIssueRequest{}, func(v any) istore.Mutation { return istore.Mutation{Update: v.(*orchapi.UpdateIssueRequest)} }},
		{"comment", &orchapi.IssueCommentRequest{}, func(v any) istore.Mutation { return istore.Mutation{Comment: v.(*orchapi.IssueCommentRequest)} }},
		{"comment_edit", &istore.CommentEdit{}, func(v any) istore.Mutation { return istore.Mutation{CommentEdit: v.(*istore.CommentEdit)} }},
		{"close", &orchapi.CloseIssueRequest{}, func(v any) istore.Mutation { return istore.Mutation{Close: v.(*orchapi.CloseIssueRequest)} }},
		{"reopen", &orchapi.ReopenIssueRequest{}, func(v any) istore.Mutation { return istore.Mutation{Reopen: v.(*orchapi.ReopenIssueRequest)} }},
	}
	for _, tc := range cases {
		typ := reflect.TypeOf(tc.value).Elem()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if reason := exclusions[f.Name]; reason != "" {
				continue
			}
			v := reflect.New(typ)
			field := v.Elem().Field(i)
			if !field.CanSet() {
				continue
			}
			switch {
			case field.Type() == reflect.TypeOf(json.RawMessage{}):
				field.SetBytes([]byte(`{"a\u200bb":"x\u200by"}`))
			case field.Kind() == reflect.String:
				field.SetString("x\u200by")
			case field.Kind() == reflect.Pointer && field.Type().Elem().Kind() == reflect.String:
				p := reflect.New(field.Type().Elem())
				p.Elem().SetString("x\u200by")
				field.Set(p)
			case field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.String:
				field.Set(reflect.ValueOf([]string{"x\u200by"}))
			case textless(field.Type()):
				continue
			default:
				t.Errorf("%s.%s: %s is not audited; clean it here or name why it is exempt", tc.name, f.Name, field.Type())
				continue
			}
			if tc.name == "create" && f.Name != "Title" {
				v.Elem().FieldByName("Title").SetString("title")
			}
			m := tc.mutation(v.Interface())
			if err := cleanMutation(&m); err != nil {
				t.Fatalf("%s.%s: %v", tc.name, f.Name, err)
			}
			raw, err := json.Marshal(v.Interface())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "\u200b") || strings.Contains(string(raw), `\\u200b`) {
				t.Errorf("%s.%s survives: %s", tc.name, f.Name, raw)
			}
		}
	}
}

// textless types carry no human-readable text.
func textless(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64:
		return true
	}
	return t == reflect.TypeOf(time.Time{})
}
