package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"

	"github.com/sengirunners/potluck/kit/secrets"
)

// Publisher carries a recorded decision to where the pull request lives.
// The daemon calls it after the decision is committed; a failure is logged
// and never undoes the decision. Tests use a fake.
type Publisher interface {
	Publish(ctx context.Context, ask orchapi.Ask, d orchapi.Decision) error
}

// GitHubBot posts a commit status (context "zm/approval") on the ask's
// head SHA and a pull-request comment carrying the signed receipt. It uses a
// least-privilege bot or GitHub App token the daemon holds for that project
// ($ZM_HOME/approvals/github/<project ULID>.token, mode 0600; statuses
// and pull-request comments only), never the owner's own login or gh. With
// no token file for the project nothing is posted.
type GitHubBot struct {
	Dir     string       // holds <project ULID>.token files
	BaseURL string       // default https://api.github.com
	HTTP    *http.Client // default: 20 s timeout
}

// ErrNoCredential means the project has no bot token: nothing was posted.
var ErrNoCredential = errors.New("no GitHub bot token for this project")

func (g *GitHubBot) token(project string) (string, error) {
	v, err := secrets.Get(context.Background(), secrets.File(filepath.Join(g.Dir, project+".token")))
	if errors.Is(err, secrets.ErrNotFound) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", err
	}
	if v.IsZero() {
		return "", ErrNoCredential
	}
	return v.Reveal(), nil
}

// Publish implements Publisher.
func (g *GitHubBot) Publish(ctx context.Context, ask orchapi.Ask, d orchapi.Decision) error {
	host, rest, _ := strings.Cut(ask.Subject.Repo, "/")
	if host != "github.com" {
		return nil // only github.com repos have a bot here
	}
	tok, err := g.token(ask.Project)
	if err != nil {
		return err
	}
	state, verb := "failure", "declined"
	if d.Decision == orchapi.DecisionApprove {
		state, verb = "success", "approved"
	}
	status := map[string]string{"state": state, "context": "zm/approval",
		"description": clip(fmt.Sprintf("%s by %s (signed zm receipt)", verb, d.Approver), 140)}
	if err := g.post(ctx, tok, "/repos/"+rest+"/statuses/"+ask.Subject.HeadSHA, status); err != nil {
		return err
	}
	rj, _ := json.MarshalIndent(d.Receipt, "", "  ")
	body := fmt.Sprintf("zm decision: **%s** by approver `%s` for head `%s`.\n\n```json\n%s\n```\n\n"+
		"Verify offline: `zm receipt verify - --key <published key> --repo %s --pr %d --head %s --decision approve`\n",
		d.Decision, d.Approver, ask.Subject.HeadSHA, rj, ask.Subject.Repo, ask.Subject.PR, ask.Subject.HeadSHA)
	return g.post(ctx, tok, fmt.Sprintf("/repos/%s/issues/%d/comments", rest, ask.Subject.PR), map[string]string{"body": body})
}

func (g *GitHubBot) post(ctx context.Context, tok, path string, v any) error {
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	b, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s: %s %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
