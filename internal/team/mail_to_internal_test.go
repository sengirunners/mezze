package team

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// The replica checks the asks-only rule against the recipient the authority
// will pick (work.mailTarget), or a refused note is queued as sent.
func TestItemMailToMatchesAuthority(t *testing.T) {
	lease := &orchapi.Lease{Holder: "mini/aaaaaaaa"}
	for _, c := range []struct {
		name string
		it   orchapi.WorkItem
		h    orchapi.HolderResponse
		want string
	}{
		{"held", orchapi.WorkItem{Source: "bd", Owner: "human:ivo"}, orchapi.HolderResponse{Lease: lease, MailTo: "mini/aaaaaaaa"}, "mini/aaaaaaaa"},
		{"tracker owner before creator", orchapi.WorkItem{Source: "bd", Owner: "human:ivo", CreatedBy: "mini/bbbbbbbb"}, orchapi.HolderResponse{MailTo: "mini/bbbbbbbb"}, "human:ivo"},
		{"local item: creator", orchapi.WorkItem{Source: "local", Owner: "human:ivo", CreatedBy: "mini/bbbbbbbb"}, orchapi.HolderResponse{MailTo: "mini/bbbbbbbb"}, "mini/bbbbbbbb"},
		{"owner not an address", orchapi.WorkItem{Source: "bd", Owner: "Ivo C", CreatedBy: "human:bea"}, orchapi.HolderResponse{MailTo: "human:bea"}, "human:bea"},
		{"bare login owner is skipped", orchapi.WorkItem{Source: "bd", Owner: "ivogh", CreatedBy: "human:bea"}, orchapi.HolderResponse{MailTo: "human:bea"}, "human:bea"},
		{"owners is skipped", orchapi.WorkItem{Source: "bd", Owner: "owners", CreatedBy: "mini/bbbbbbbb"}, orchapi.HolderResponse{MailTo: "mini/bbbbbbbb"}, "mini/bbbbbbbb"},
		{"nobody", orchapi.WorkItem{Source: "bd"}, orchapi.HolderResponse{}, "bd:x"},
	} {
		if got := itemMailTo(c.it, c.h, "bd:x"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
