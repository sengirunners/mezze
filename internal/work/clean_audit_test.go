package work

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"reflect"
	"strings"
	"testing"
)

func TestReview2WorkWriteFieldAudit(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	typ := reflect.TypeOf(orchapi.CreateWorkRequest{})
	excluded := map[string]string{"Area": "authorization scope"}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if excluded[f.Name] != "" {
			continue
		}
		req := orchapi.CreateWorkRequest{Title: "title", Area: "solver"}
		v := reflect.ValueOf(&req).Elem().Field(i)
		switch {
		case v.Kind() == reflect.String:
			v.SetString("x\u200by")
		case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String:
			v.Set(reflect.ValueOf([]string{"x\u200by"}))
		default:
			continue
		}
		if f.Name != "Title" && f.Name != "Body" && f.Name != "Labels" {
			t.Fatalf("new work field %s needs a stored-field assertion or a reasoned exclusion", f.Name)
		}
		it, err := orchclient.CreateWork(h.ctx, c, req)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if strings.Contains(it.Title, "\u200b") || strings.Contains(it.Body, "\u200b") || strings.Contains(strings.Join(it.Labels, ","), "\u200b") {
			t.Errorf("%s: %+v", f.Name, it)
		}
	}
}

func TestReview2CommentWriteFieldAudit(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	it, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "title", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(orchapi.CommentRequest{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		req := orchapi.CommentRequest{Body: "body"}
		v := reflect.ValueOf(&req).Elem().Field(i)
		if v.Kind() != reflect.String {
			continue
		}
		v.SetString("x\u200by")
		if f.Name != "Body" {
			t.Fatalf("new comment field %s needs a stored-field assertion or a reasoned exclusion", f.Name)
		}
		out, err := orchclient.Comment(h.ctx, c, it.ID, req)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if strings.Contains(out.Body, "\u200b") {
			t.Errorf("%s survived: %+v", f.Name, out)
		}
	}
}
