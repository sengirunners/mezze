package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestFinalReviewCachedPrivateResultAudience(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	token := readToken(t, j.Remote.TokenFile)
	req := api.WritesRequest{Project: j.Remote.Project.ID, Writes: []api.QueuedWrite{{ID: ids.New(), Method: http.MethodPost, Path: "/v0/mail", Body: json.RawMessage(`{"to":"human:mini-human","body":"invented-final-private-canary","audience":"participants"}`)}}}
	send := func(capable bool) api.WritesResponse {
		b, _ := json.Marshal(req)
		hr, _ := http.NewRequest(http.MethodPost, "http://potluck-mini:7433/v0/team/writes", bytes.NewReader(b))
		hr.Header.Set(api.HeaderAuthorization, "Bearer "+token)
		hr.Header.Set(api.HeaderProtocol, api.Protocol)
		if capable {
			hr.Header.Set(api.HeaderPersonalMail, api.PersonalMailProtocol)
		}
		resp, err := tailnetClientAs(t, tn, r.cfg.Machine).Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out api.WritesResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := send(true)
	if len(first.Results) != 1 || first.Results[0].Status/100 != 2 {
		t.Fatalf("initial write: %+v", first)
	}
	req.Writes[0].As = "bea-mac/1234abcd"
	second := send(true)
	if len(second.Results) != 0 || second.Stopped == "" {
		t.Fatal("changed actor must hold the write pending", second)
	}
	if strings.Contains(reviewJSON(second), "invented-final-private-canary") {
		t.Errorf("nonparticipant cached result disclosed private body: %s", reviewJSON(second))
	}
	req.Writes[0].As = ""
	third := send(false)
	if len(third.Results) != 0 || third.Stopped == "" {
		t.Fatal("unaware replica must hold the write pending", third)
	}
	if strings.Contains(reviewJSON(third), "invented-final-private-canary") {
		t.Errorf("unaware replica cached result disclosed private body: %s", reviewJSON(third))
	}
}

func TestFinalReviewLivePrivateResultAudienceAndRescope(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	hdr := map[string]string{api.HeaderWriteID: ids.New(), api.HeaderProject: j.Remote.Project.ID, api.HeaderPersonalMail: api.PersonalMailProtocol}
	req := orchapi.SendMailRequest{To: "human:mini-human", Area: "billing", Body: "invented-live-private-canary", Audience: orchapi.MailAudienceParticipants}
	send := func() (int, string) {
		return rawCall(t, hc, "POST", "http://potluck-mini:7433/v0/mail", token, req, hdr)
	}
	st, body := send()
	if st/100 != 2 {
		t.Fatalf("initial=%d %s", st, body)
	}
	st2, body2 := send()
	if st2 != st || body2 != body {
		t.Fatalf("same actor replay changed %d %s", st2, body2)
	}
	hdr[api.HeaderAs] = "bea-mac/1234abcd"
	_, got := send()
	if strings.Contains(got, req.Body) {
		t.Error("live cached body disclosed to nonparticipant session")
	}
	delete(hdr, api.HeaderAs)
	delete(hdr, api.HeaderPersonalMail)
	_, got = send()
	if strings.Contains(got, req.Body) {
		t.Error("live cached body disclosed to unaware replica")
	}
	hdr[api.HeaderPersonalMail] = api.PersonalMailProtocol
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: "solver", Role: "guest"}))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	_, got = send()
	if strings.Contains(got, req.Body) {
		t.Error("live cached billing body disclosed after scope narrowed to solver")
	}
}

func reviewJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
