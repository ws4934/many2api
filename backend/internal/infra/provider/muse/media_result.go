package muse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"
)

// Content identity is needed because Muse may echo an uploaded reference under
// a different blob URL. Persist these small identifiers, not the source bytes,
// so recovery never needs to fetch the client's original URL again.
func inputDigests(inputs []attachment) []string {
	var out []string
	for _, input := range inputs {
		out = append(out, mediaDigest(input.Data))
	}
	return out
}

func mediaDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *page) waitGeneratedMedia(ctx context.Context, job mediaJob, baseline []string, sentAt time.Time) (mediaFile, error) {
	digests := job.InputDigests
	if len(digests) == 0 {
		digests = inputDigests(job.Inputs)
	}
	if onThread := job.OnThread; onThread != nil {
		reported := false
		job.OnThread = func(path string) error {
			if reported {
				return nil
			}
			err := onThread(path)
			reported = err == nil
			return err
		}
	}
	for {
		found, err := p.waitMedia(ctx, job, baseline, sentAt)
		if err != nil {
			return mediaFile{}, err
		}
		file, err := p.download(ctx, found.Src)
		if err != nil {
			return mediaFile{}, err
		}
		if len(digests) > 0 && slices.Contains(digests, mediaDigest(file.Data)) {
			baseline = append(baseline, found.Src)
			// Keep the original context/deadline and submission. Finding an
			// echoed original never starts another generation or timeout.
			continue
		}
		file.Width, file.Height = found.W, found.H
		return file, nil
	}
}
