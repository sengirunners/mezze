package mail

// This is a one-shot read-only recovery reader, not a runtime Hub backend.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// ReadLegacyHub fetches only the manifest's exact IDs via GET /mail?id=N.
// Source project is supplied explicitly by each manifest entry; Hub has no
// canonical zm project. Feed previews and session inboxes are never consulted.
// Credentials are supplied by the operator and never persisted or returned.
func ReadLegacyHub(ctx context.Context, transport *http.Client, endpoint, token string, manifest orchapi.LegacyMailImportRequest) (orchapi.LegacyMailImportRequest, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/mail") || token == "" {
		return manifest, fmt.Errorf("explicit HTTPS /mail endpoint and source owner token required")
	}
	if len(manifest.SourceIDs) == 0 || len(manifest.SourceIDs) > 100 || len(manifest.Records) != len(manifest.SourceIDs) {
		return manifest, fmt.Errorf("reader requires 1..100 exact source IDs with explicit source project entries")
	}
	if transport == nil {
		transport = http.DefaultClient
	}
	client := *transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	allowed := map[string]bool{}
	for _, id := range manifest.SourceIDs {
		n, e := strconv.ParseUint(id, 10, 63)
		if e != nil || n == 0 || strconv.FormatUint(n, 10) != id || allowed[id] {
			return manifest, fmt.Errorf("source IDs must be unique positive canonical integers")
		}
		allowed[id] = true
	}
	out := manifest
	out.Records = nil
	total := 0
	for _, seed := range manifest.Records {
		if !allowed[seed.ID] || seed.Project == "" {
			return manifest, fmt.Errorf("each source ID requires one explicit source project")
		}
		delete(allowed, seed.ID)
		target := *u
		q := target.Query()
		q.Set("id", seed.ID)
		target.RawQuery = q.Encode()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if e != nil {
			return manifest, fmt.Errorf("invalid source request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, e := client.Do(req)
		if e != nil {
			return manifest, fmt.Errorf("source read failed for ID %s", seed.ID)
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, MaxImportBytes+1))
		resp.Body.Close()
		total += len(b)
		if e != nil || resp.StatusCode != http.StatusOK || total > MaxImportBytes {
			return manifest, fmt.Errorf("source ID %s unavailable or exceeds finite 1 MiB read budget", seed.ID)
		}
		var wire struct {
			Message *struct {
				ID              int64  `json:"id"`
				Sender          string `json:"sender"`
				Recipient       string `json:"recipient"`
				Kind            string `json:"kind"`
				Ref             string `json:"ref"`
				SentAt          int64  `json:"sent_at"`
				AckedAt         *int64 `json:"acked_at"`
				AckedBy         string `json:"acked_by"`
				DigestAt        *int64 `json:"digest_at"`
				LastDeliveredAt *int64 `json:"last_delivered_at"`
				DeadAt          *int64 `json:"dead_at"`
				DeliveredCount  int    `json:"delivered_count"`
				ReplyTo         *int64 `json:"reply_to"`
				Body            string `json:"body"`
				BodyLen         *int   `json:"body_len"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &wire) != nil || wire.Message == nil {
			return manifest, fmt.Errorf("source ID %s lacks a full message", seed.ID)
		}
		m := wire.Message
		if strconv.FormatInt(m.ID, 10) != seed.ID || m.BodyLen == nil || *m.BodyLen != utf8.RuneCountInString(m.Body) || !utf8.ValidString(m.Body) || len(m.Body) > MaxImportBodyBytes {
			return manifest, fmt.Errorf("source ID %s body is incomplete, mismatched or oversized", seed.ID)
		}
		ts := func(v *int64) *time.Time {
			if v == nil {
				return nil
			}
			t := time.UnixMilli(*v).UTC()
			return &t
		}
		r := orchapi.LegacyMailRecord{Kind: "mail", ID: seed.ID, Project: seed.Project, SourceKind: m.Kind, SourceRef: m.Ref, Sender: m.Sender, Recipient: m.Recipient, SentAt: time.UnixMilli(m.SentAt).UTC(), Body: m.Body, BodyComplete: true,
			Delivery: orchapi.LegacyDelivery{State: "source_reported", AckBy: m.AckedBy, AckAt: ts(m.AckedAt), DigestAt: ts(m.DigestAt), LastDeliveredAt: ts(m.LastDeliveredAt), DeadAt: ts(m.DeadAt), DeliveredCount: m.DeliveredCount}}
		if m.ReplyTo != nil {
			r.ParentID = strconv.FormatInt(*m.ReplyTo, 10)
		}
		out.Records = append(out.Records, r)
	}
	return out, nil
}
