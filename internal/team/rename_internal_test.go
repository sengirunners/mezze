package team

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// Rename step 1: zm's team code reads the Potluck-* name of
// an aliased header as kit does (new first, then Zm-*), and what it sends
// stays under the Zm-* name only.

// dropProject leaves no project behind under either header name.
func TestDropProjectBothNames(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v0/mail?project=harbor&x=1", nil)
	for _, h := range []string{api.HeaderProject, api.HeaderProjectFrom} {
		r.Header.Set(h, "old")
		r.Header.Set(api.HeaderNew(h), "new")
	}
	dropProject(r)
	for _, h := range []string{api.HeaderProject, api.HeaderProjectFrom} {
		if v := r.Header.Values(h); len(v) != 0 {
			t.Errorf("%s left: %q", h, v)
		}
		if v := r.Header.Values(api.HeaderNew(h)); len(v) != 0 {
			t.Errorf("%s left: %q", api.HeaderNew(h), v)
		}
	}
	if r.URL.RawQuery != "x=1" {
		t.Errorf("query %q", r.URL.RawQuery)
	}
}

// A queued write runs with the replica's protocol and scope version from
// whichever name the batch carried them under, written as Potluck-* (what
// this build sends, never also as Zm-*); Zm-Personal-Mail is zm's own and has
// no other name.
func TestDispatchCarriesHeadersUnderNewNames(t *testing.T) {
	var got http.Header
	s := &Service{d: &core.Deps{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		api.WriteJSON(w, http.StatusOK, map[string]string{})
	})}}
	pnew, svnew := api.HeaderNew(api.HeaderProtocol), api.HeaderNew(api.HeaderScopeVersion)
	for _, c := range []struct {
		name         string
		outer        http.Header
		proto, scope string
		personal     string
	}{
		{"old names", http.Header{api.HeaderProtocol: {api.Protocol}, api.HeaderScopeVersion: {"7"}, api.HeaderPersonalMail: {api.PersonalMailProtocol}},
			api.Protocol, "7", api.PersonalMailProtocol},
		{"new names", http.Header{pnew: {api.ProtocolNext}, svnew: {"8"}},
			api.ProtocolNext, "8", ""},
		{"both: new first", http.Header{api.HeaderProtocol: {api.Protocol}, pnew: {api.ProtocolNext}, api.HeaderScopeVersion: {"7"}, svnew: {"8"}},
			api.ProtocolNext, "8", ""},
		{"new name empty", http.Header{api.HeaderProtocol: {api.Protocol}, pnew: {""}, api.HeaderScopeVersion: {"7"}, svnew: {""}},
			api.Protocol, "7", ""},
		{"personal mail only by its own name", http.Header{api.HeaderNew(api.HeaderPersonalMail): {api.PersonalMailProtocol}},
			"", "", ""},
	} {
		outer := httptest.NewRequest(http.MethodPost, "/v0/team/writes", nil)
		outer.Header = c.outer
		outer.Header.Set(api.HeaderAuthorization, "Bearer zm_x")
		got = nil
		var minted string
		st, _ := s.dispatch(context.Background(), outer, "01J9Z3K0QX7Y8AZ5V2N4B6C8D0",
			api.QueuedWrite{ID: "01J9Z3K0QX7Y8AZ5V2N4B6C8D1", Method: http.MethodPost, Path: "/v0/mail", As: "bea-mac/1234abcd"}, "01J9Z3K0QX7Y8AZ5V2N4B6C8D2", &minted)
		if st != http.StatusOK || got == nil {
			t.Fatalf("%s: dispatch %d", c.name, st)
		}
		want := map[string]string{
			api.HeaderAuthorization: "Bearer zm_x",
			api.HeaderProtocol:      c.proto,
			api.HeaderScopeVersion:  c.scope,
			api.HeaderPersonalMail:  c.personal,
			api.HeaderProject:       "01J9Z3K0QX7Y8AZ5V2N4B6C8D0",
			api.HeaderProjectFrom:   "flag",
			api.HeaderWriteID:       "01J9Z3K0QX7Y8AZ5V2N4B6C8D1",
			api.HeaderBase:          "01J9Z3K0QX7Y8AZ5V2N4B6C8D2",
			api.HeaderAs:            "bea-mac/1234abcd",
		}
		for h, v := range want {
			name := api.HeaderNew(h) // Authorization and Zm-Personal-Mail keep theirs
			if h == api.HeaderPersonalMail {
				name = h
			}
			if g := got.Get(name); g != v {
				t.Errorf("%s: %s = %q, want %q", c.name, name, g, v)
			}
		}
		// zm's own header never travels under a Potluck name, in any case.
		if v := got.Values(api.HeaderNew(api.HeaderPersonalMail)); len(v) != 0 {
			t.Errorf("%s: dispatched %s: %q", c.name, api.HeaderNew(api.HeaderPersonalMail), v)
		}
		for h := range got {
			if strings.HasPrefix(strings.ToLower(h), "zm-") && h != api.HeaderPersonalMail {
				t.Errorf("%s: dispatched an old name: %s", c.name, h)
			}
		}
	}
}
