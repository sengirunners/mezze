package mail

import (
	"encoding/json"
	"github.com/sengirunners/mezze/internal/orchclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLegacyReaderFullBodiesExactIDsNoWrites(t *testing.T) {
	f := newFixture(t)
	req := importArtifact(f)
	body := strings.Repeat("invented 漢 paragraph ", 160)
	requests := 0
	preview := false
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/mail" || r.URL.RawQuery != "id=18615" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected source request %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		got := body
		if preview {
			got = got[:2000]
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"id": 18615, "sender": "bea-mac/01a1a544", "recipient": "personivo", "kind": "report", "sent_at": req.Records[0].SentAt.UnixMilli(), "acked_by": "digest", "acked_at": req.Records[0].SentAt.UnixMilli(), "digest_at": nil, "delivered_count": 0, "body": got, "body_len": utf8.RuneCountInString(body)}})
	}))
	defer ts.Close()
	got, err := ReadLegacyHub(ctx, ts.Client(), ts.URL+"/mail", "fixture-token", req)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || got.Records[0].Body != body || !got.Records[0].BodyComplete || got.Records[0].SourceKind != "report" {
		t.Fatal("lost full message", got)
	}
	out := must(orchclient.ImportLegacyMail(ctx, f.as("", "harbor"), got))
	m := must(orchclient.GetMail(ctx, f.as("", "harbor"), out.Records[0].NativeID))
	if m.AckedAt != nil || m.LegacyDelivery.State != "digest_held" || m.Origin.SourceKind != "report" {
		t.Fatal(m)
	}
	preview = true
	if _, err := ReadLegacyHub(ctx, ts.Client(), ts.URL+"/mail", "fixture-token", req); err == nil {
		t.Fatal("accepted feed preview")
	}
	if _, err := ReadLegacyHub(ctx, ts.Client(), strings.Replace(ts.URL, "https:", "http:", 1)+"/mail", "fixture-token", req); err == nil {
		t.Fatal("sent source credential over HTTP")
	}
}
