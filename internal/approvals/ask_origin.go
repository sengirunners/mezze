package approvals

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

const (
	maxOriginTitle = 200 // runes: the asking thread's title or the work item's
	maxPRURL       = 300
)

// prPath is a pull request's path on a forge: /{owner}/{repo}/pull/{n}.
var prPath = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+$`)

var askOriginMigrations = []store.Migration{
	// What an ask is about, for a client to show beside its card.
	{Kind: store.KindRegistry, Name: "approvals/0012_ask_origin", Script: `
ALTER TABLE asks ADD COLUMN work_title TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN thread_title TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN pr_url TEXT NOT NULL DEFAULT '';
`},
}

// forgeHosts are the hosts a pr_url may name: github.com, git.cafe, and the
// comma-separated hosts of ZM_FORGE_HOSTS in the daemon's environment.
func forgeHosts() []string {
	hosts := []string{"github.com", "git.cafe"}
	for _, h := range strings.Split(os.Getenv("ZM_FORGE_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func oneLine(s string) string { return strings.Join(strings.Fields(textclean.Clean(s)), " ") }

// cleanOrigin checks the asker's thread title and pull request URL. Both are
// display fields a client renders (the URL as a link beside an approval
// card), so the URL must be a pull request of a known forge, in plain ASCII.
func cleanOrigin(title, prURL string) (string, string, error) {
	if strings.ContainsAny(title, "\n\r  ") {
		return "", "", api.Errorf(api.CodeBadRequest, "thread_title is one plain line")
	}
	title = oneLine(title)
	if unsafePresentation(title) {
		return "", "", api.Errorf(api.CodeBadRequest, "thread_title is one plain line")
	}
	if utf8.RuneCountInString(title) > maxOriginTitle {
		return "", "", api.Errorf(api.CodeBadRequest, "thread_title is at most %d characters", maxOriginTitle)
	}
	prURL = strings.TrimSpace(prURL)
	if prURL == "" {
		return title, "", nil
	}
	bad := api.Errorf(api.CodeBadRequest, "pr_url is the https URL of a pull request on github.com or a forge host: https://<host>/<owner>/<repo>/pull/<n>")
	u, err := url.Parse(prURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery ||
		u.Port() != "" || u.Host == "" || u.Host != u.Hostname() || !prPath.MatchString(u.Path) {
		return "", "", bad
	}
	host := strings.ToLower(u.Host)
	known := false
	for _, h := range forgeHosts() {
		known = known || h == host
	}
	if !known {
		return "", "", bad
	}
	out := "https://" + host + u.Path
	if len(out) > maxPRURL {
		return "", "", api.Errorf(api.CodeBadRequest, "pr_url is at most %d characters", maxPRURL)
	}
	return title, out, nil
}

// workTitle is the title of work item work as sub reads it now, as one plain
// line; "" when the work module cannot say or the title reads as a decision
// marker (the ask is never refused over a headline).
func (s *Service) workTitle(ctx context.Context, sub *authz.Subject, project, work string) string {
	wd, ok := orchcore.Work(s.d).(orchcore.WorkDescriber)
	if !ok {
		return ""
	}
	title, _, _, err := wd.ItemText(ctx, sub, project, work)
	if err != nil {
		s.d.Log.Debug("approvals: work title lookup", "work", work, "err", err)
		return ""
	}
	title = oneLine(title)
	if unsafePresentation(title) {
		return ""
	}
	if r := []rune(title); len(r) > maxOriginTitle {
		title = string(r[:maxOriginTitle])
	}
	return title
}
