package media

import "time"

// DisplayProgress projects video progress at read time, without timers or
// periodic database writes. Progress retains the reported upstream percentage
// or provider milestone.
// Like Grok's initial estimate, the clock advances at 100 points per minute,
// but may lead the last reported value by at most 15 points. An upstream stall
// therefore stops the estimate rather than pretending the job is nearly done.
func (j Job) DisplayProgress(now time.Time) int {
	if j.Kind == KindImage {
		return j.Progress
	}
	if j.Status == StatusCompleted {
		return 100
	}
	progress := min(99, max(0, j.Progress))
	if j.Status != StatusInProgress || j.ProgressStartedAt == nil {
		return progress
	}
	elapsed := max(0, now.Sub(*j.ProgressStartedAt).Seconds())
	estimate := 1 + elapsed*100/60
	return int(min(99, float64(progress+15), max(float64(progress), estimate)))
}
