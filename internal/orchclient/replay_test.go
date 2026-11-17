package orchclient_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestAttachEmbedExplicitProtocolSelection(t *testing.T) {
	for _, selected := range []string{orchapi.AttachProtocol, orchapi.AttachReplayProtocol, "unknown"} {
		var count atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			if r.Header.Get(orchapi.HeaderReplaySurface) != orchapi.FreshReplaySurface || r.URL.Query().Get("replay") != "all" || r.URL.Query().Get("input") != "0" || !strings.Contains(r.Header.Get("Upgrade"), orchapi.AttachProtocol) || !strings.Contains(r.Header.Get("Upgrade"), orchapi.AttachReplayProtocol) {
				t.Error("missing negotiation/scope fields")
			}
			if r.Header.Get("Authorization") != "" || r.Header.Get(api.HeaderAs) != "" {
				t.Error("unexpected identity override")
			}
			c, b, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			fmt.Fprintf(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: %s\r\n\r\n", selected, api.HeaderProtocol, api.Protocol)
			b.Flush()
		}))
		c := client.NewRemote(srv.URL, nil)
		stream, err := orchclient.AttachEmbed(context.Background(), c, "fixture", true)
		if selected == "unknown" {
			if err == nil {
				stream.Close()
				t.Fatal("unknown protocol accepted")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if stream.Framed != (selected == orchapi.AttachReplayProtocol) {
				t.Fatal("selection guessed")
			}
			stream.Close()
		}
		srv.Close()
		if count.Load() != 1 {
			t.Fatal("fallback made a second request")
		}
	}
}
func TestAttachEmbedErrorsNeverTriggerFallback(t *testing.T) {
	for _, code := range []string{api.CodeUnauthenticated, api.CodeForbidden, api.CodeNotFound, api.CodeBadRequest, api.CodeConflict} {
		var count atomic.Int32
		c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			w.Header().Set(api.HeaderProtocol, api.Protocol)
			api.WriteError(w, api.Errorf(code, "fixture refusal"))
		}))
		if _, err := orchclient.AttachEmbed(context.Background(), c, "fixture", false); !api.IsCode(err, code) {
			t.Fatalf("error changed %s %v", code, err)
		}
		if count.Load() != 1 {
			t.Fatal("unauthorized fallback")
		}
	}
}
