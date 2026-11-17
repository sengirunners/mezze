package team_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Read marks have no event, so a member's copy never holds them: a member's
// PUT with ?project= lands on the authority as the joined person, and its
// GET (named or merged) answers the authority's marks for that person only.
func TestMemberReadMarksLiveAtTheAuthority(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitReachable(t, r, true)
	get := func(c *client.Client, query string) []orchapi.ReadMark {
		t.Helper()
		var l orchapi.ReadMarkList
		if err := c.Do(ctx, http.MethodGet, "/v0/mail/read-marks"+query, nil, &l); err != nil {
			t.Fatalf("GET%s: %v", query, err)
		}
		return l.Marks
	}
	put := func(c *client.Client, ms ...orchapi.ReadMarkSet) []orchapi.ReadMark {
		t.Helper()
		var l orchapi.ReadMarkList
		if err := c.Do(ctx, http.MethodPut, "/v0/mail/read-marks?project=harbor", orchapi.ReadMarksRequest{Marks: ms}, &l); err != nil {
			t.Fatalf("PUT: %v", err)
		}
		return l.Marks
	}
	put(a.owner, orchapi.ReadMarkSet{Conv: "harbor", UptoMs: 500})
	beaMarks := []orchapi.ReadMark{{Project: "harbor", Conv: "human:mini-human", UptoMs: 200}}
	if got := put(r.owner, orchapi.ReadMarkSet{Conv: "human:mini-human", UptoMs: 200}); !slices.Equal(got, beaMarks) {
		t.Fatalf("bea's PUT from the member answered %+v", got)
	}
	// It is in the authority's database, as human:bea.
	db, err := a.dm.Deps.Store.Project(authz.All(), authz.Read, j.Remote.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var who string
	if err := db.QueryRowContext(ctx, `SELECT identity FROM mail_read_marks WHERE conv = 'human:mini-human'`).Scan(&who); err != nil || who != "human:bea" {
		t.Fatalf("authority row: %q %v", who, err)
	}
	for _, q := range []string{"?project=harbor", ""} {
		if got := get(r.owner, q); !slices.Equal(got, beaMarks) {
			t.Fatalf("member GET%s: %+v, want only bea's", q, got)
		}
	}
	if got := get(a.owner, "?project=harbor"); !slices.Equal(got, []orchapi.ReadMark{{Project: "harbor", Conv: "harbor", UptoMs: 500}}) {
		t.Fatalf("the authority's owner reads %+v", got)
	}
}
