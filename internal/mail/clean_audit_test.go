package mail

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"reflect"
	"strings"
	"testing"
)

func TestReview2MailWriteFieldAudit(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateIdle)
	c := f.as("", "harbor/solver")
	typ := reflect.TypeOf(orchapi.SendMailRequest{})
	excluded := map[string]string{"Audience": "validated routing enum", "ClientRequestID": "opaque payload-bound idempotency key; never display text", "To": "recipient routing", "Area": "authorization scope", "Ref": "work id", "ReplyTo": "mail id", "ID": "write id", "Kind": "routing enum", "Needs": "routing enum"}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if excluded[field.Name] != "" {
			continue
		}
		req := orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "body"}
		v := reflect.ValueOf(&req).Elem().Field(i)
		if v.Kind() != reflect.String {
			continue
		}
		v.SetString("x\u200by")
		if field.Name != "Body" {
			t.Fatalf("new mail field %s needs a stored-field assertion or a reasoned exclusion", field.Name)
		}
		out, err := orchclient.Send(ctx, c, req)
		if err != nil {
			t.Fatalf("%s: %v", field.Name, err)
		}
		if strings.Contains(out.Body, "\u200b") {
			t.Errorf("%s survived", field.Name)
		}
	}
}
