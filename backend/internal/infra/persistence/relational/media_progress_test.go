package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaProgressAnchorMigrationAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTestDatabase(t)
	key := clientKeyModel{Name: "progress", Prefix: "progress", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := db.db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewMediaJobRepository(db)
	now := time.Now().UTC()
	job := testMediaJob("video_progress", 0, key.ID, media.StatusInProgress, now)
	job.Progress = 20
	if err := repo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Model the previous schema, then run the real startup migration.
	if err := db.db.Exec("ALTER TABLE media_jobs DROP COLUMN progress_started_at").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetMediaJob(ctx, job.ID, key.ID)
	if err != nil || stored.Progress != 20 || stored.ProgressStartedAt != nil {
		t.Fatalf("migrated job = %+v, %v", stored, err)
	}
	job.ProgressStartedAt = &now
	if err := repo.UpdateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err = repo.GetMediaJob(ctx, job.ID, key.ID)
	if err != nil || stored.ProgressStartedAt == nil || !stored.ProgressStartedAt.Equal(now) {
		t.Fatalf("stored anchor = %v, %v", stored.ProgressStartedAt, err)
	}
	jobs, _, err := repo.ListMediaJobs(ctx, repository.MediaJobListQuery{Page: repository.PageQuery{Limit: 10}})
	if err != nil || len(jobs) != 1 || jobs[0].ProgressStartedAt == nil || !jobs[0].ProgressStartedAt.Equal(now) {
		t.Fatalf("listed jobs = %+v, %v", jobs, err)
	}
	if got := jobs[0].DisplayProgress(now.Add(time.Minute)); got != 35 {
		t.Fatalf("listed estimate = %d", got)
	}
}
