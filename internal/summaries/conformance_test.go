package summaries

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Every future Repository driver runs this contract: one immutable retained
// result per project/day/config version, with project boundaries intact.
func repositoryConformance(t *testing.T, repository Repository) {
	t.Helper()
	ctx := context.Background()
	a := orchapi.ProjectSummary{ID: "one", Project: "project-a", Day: "2026-11-04", ConfigVersion: "config", Generated: time.Now(), Body: "Original digest"}
	first, err := repository.Put(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = "retry"
	a.Body = "Changed source cannot rewrite archive"
	again, err := repository.Put(ctx, a)
	if err != nil || again.ID != first.ID || again.Body != first.Body {
		t.Fatalf("immutable replay: %+v %v", again, err)
	}
	if _, err = repository.Get(ctx, "project-b", a.Day, a.ConfigVersion); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-project archive: %v", err)
	}
}

func TestSQLiteRepositoryConformance(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	repositoryConformance(t, s)
}
