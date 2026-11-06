package media

import (
	"testing"
	"time"
)

func TestVideoDisplayProgress(t *testing.T) {
	start := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		status  Status
		raw     int
		elapsed time.Duration
		want    int
	}{
		{"starts at one", StatusInProgress, 1, 0, 1},
		{"advances without upstream update", StatusInProgress, 1, 3 * time.Second, 6},
		{"stalled upstream caps headroom", StatusInProgress, 1, time.Hour, 16},
		{"never below measurement", StatusInProgress, 60, 3 * time.Second, 60},
		{"new measurement allows further estimate", StatusInProgress, 30, time.Minute, 45},
		{"processing is not completion", StatusInProgress, 100, time.Minute, 99},
		{"completed", StatusCompleted, 80, time.Minute, 100},
		{"queued does not advance", StatusQueued, 0, time.Hour, 0},
		{"failed does not advance", StatusFailed, 40, time.Hour, 40},
		{"uncertain does not advance", StatusUncertain, 40, time.Hour, 40},
		{"canceled does not advance", StatusCanceled, 0, time.Hour, 0},
		{"clock behind start", StatusInProgress, 1, -time.Minute, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := Job{Kind: KindVideo, Status: tt.status, Progress: tt.raw, ProgressStartedAt: &start}
			if got := job.DisplayProgress(start.Add(tt.elapsed)); got != tt.want {
				t.Fatalf("display = %d, want %d", got, tt.want)
			}
			if job.Progress != tt.raw {
				t.Fatal("display changed the upstream measurement")
			}
		})
	}
	old := Job{Kind: KindVideo, Status: StatusInProgress, Progress: 42}
	if old.DisplayProgress(start) != 42 {
		t.Fatal("legacy job without anchor must retain its measurement")
	}
	image := Job{Kind: KindImage, Status: StatusInProgress, Progress: 42, ProgressStartedAt: &start}
	if image.DisplayProgress(start.Add(time.Hour)) != 42 {
		t.Fatal("image progress changed")
	}
}

func TestVideoDisplayProgressMonotonicWithinAttemptAndResetOnRetry(t *testing.T) {
	start := time.Now().UTC()
	job := Job{Kind: KindVideo, Status: StatusInProgress, Progress: 1, ProgressStartedAt: &start}
	last := 0
	for second := 0; second < 100; second++ {
		if second%10 == 0 {
			job.Progress = min(99, job.Progress+8)
		}
		value := job.DisplayProgress(start.Add(time.Duration(second) * time.Second))
		if value < last || value > min(99, job.Progress+15) {
			t.Fatalf("progress %d -> %d, raw %d", last, value, job.Progress)
		}
		last = value
	}
	retryStart := start.Add(100 * time.Second)
	job.Progress, job.ProgressStartedAt = 1, &retryStart
	if got := job.DisplayProgress(retryStart.Add(3 * time.Second)); got != 6 {
		t.Fatalf("retry retained old estimate: %d", got)
	}
}
