package main

import (
	"bytes"
	"encoding/json"
	"github.com/sengirunners/mezze/internal/orchapi"
	"reflect"
	"strings"
	"testing"
)

func TestReview2LegacyOutput(t *testing.T) {
	it := orchapi.Issue{Title: "a\u200bb"}
	if strings.Contains(listLine(it), "\u200b") || strings.Contains(flatLine(it), "\u200b") {
		t.Fatal("hidden character in list")
	}
	var b bytes.Buffer
	if err := writeJSON(&b, it, false); err != nil || strings.Contains(b.String(), "\u200b") {
		t.Fatalf("json %s %v", b.String(), err)
	}
}

func TestReview2JSONFieldAudit(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(orchapi.Issue{}), reflect.TypeOf(orchapi.IssueComment{}), reflect.TypeOf(orchapi.IssueDep{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			v := reflect.New(typ)
			field := v.Elem().Field(i)
			if !field.CanSet() {
				continue
			}
			switch {
			case field.Type() == reflect.TypeOf(json.RawMessage{}):
				field.SetBytes([]byte(`{"x\u200by":"x\u200by"}`))
			case field.Kind() == reflect.String:
				field.SetString("x\u200by")
			case field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.String:
				field.Set(reflect.ValueOf([]string{"x\u200by"}))
			case field.Kind() == reflect.Map && field.Type() == reflect.TypeOf(map[string]any{}):
				field.Set(reflect.ValueOf(map[string]any{"x\u200by": "x\u200by"}))
			default:
				continue
			}
			var out bytes.Buffer
			if err := writeJSON(&out, v.Interface(), false); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "\u200b") || strings.Contains(out.String(), `\u200b`) {
				t.Errorf("%s.%s survives: %s", typ.Name(), f.Name, out.String())
			}
		}
	}
}
