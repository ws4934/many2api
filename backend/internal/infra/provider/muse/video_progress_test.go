package muse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestVideoProgressMilestonesAndResume(t *testing.T) {
	for _, checkpoint := range []string{"", `{"thread":"/thread/v1"}`} {
		t.Run(checkpoint, func(t *testing.T) {
			var values []int
			fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
				if job.Thread == "" {
					job.OnSent()
					if err := job.OnThread("/thread/v1"); err != nil {
						return mediaFile{}, err
					}
				} else if job.OnSent != nil {
					t.Fatal("resume must not resend")
				}
				for _, value := range []int{20, 40, 40, 20, 60, 40, 60} {
					job.reportProgress(value)
				}
				return mediaFile{Data: []byte("mp4"), MIME: "video/mp4"}, nil
			}}
			adapter, credential, assets := newTestAdapter(t, fake)
			_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, JobID: "job", Prompt: "animate", Checkpoint: checkpoint,
				Progress: func(value int) {
					if len(assets.videos) != 0 {
						t.Fatal("download milestone must precede storage")
					}
					values = append(values, value)
				},
			})
			if err != nil || !slices.Equal(values, []int{20, 40, 60, 90}) || len(assets.videos) != 1 {
				t.Fatalf("progress=%v videos=%d err=%v", values, len(assets.videos), err)
			}
		})
	}
}

func TestVideoFailureDoesNotReportDownloaded(t *testing.T) {
	var values []int
	fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
		job.OnSent()
		job.reportProgress(20)
		job.reportProgress(40)
		return mediaFile{}, errTimeout
	}}
	adapter, credential, _ := newTestAdapter(t, fake)
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, JobID: "job", Prompt: "animate", Progress: func(value int) { values = append(values, value) }})
	if err == nil || !slices.Equal(values, []int{20, 40}) {
		t.Fatalf("progress=%v err=%v", values, err)
	}
}

func TestMediaProgressFollowsObservedState(t *testing.T) {
	var values []int
	observations := 0
	p := runtimePage(t, func(fn string, _ []json.RawMessage) any {
		switch fn {
		case "media":
			observations++
			switch observations {
			case 1:
				return map[string]any{"stop": true, "stuck": true}
			case 2:
				return map[string]any{"md": "Generating your video."}
			case 3:
				return map[string]any{"md": "Your video is ready.", "pending": 1}
			default:
				return map[string]any{"ready": map[string]any{"src": "blob:result"}}
			}
		case "grab":
			return map[string]any{"size": 3, "type": "video/mp4"}
		case "chunk":
			return base64.StdEncoding.EncodeToString([]byte("mp4"))
		case "drop":
			return true
		default:
			t.Errorf("unexpected helper %s", fn)
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.waitGeneratedMedia(ctx, mediaJob{Kind: mediaVideo, OnProgress: func(value int) {
		if observations == 1 {
			t.Error("still-sending state advanced progress")
		}
		values = append(values, value)
	}}, nil, time.Now())
	if err != nil || !slices.Equal(values, []int{40, 60, 60}) {
		t.Fatalf("observations=%d progress=%v err=%v", observations, values, err)
	}
}

func TestMediaSuccessTextDoesNotReportFileProgress(t *testing.T) {
	var values []int
	p := runtimePage(t, func(string, []json.RawMessage) any { return map[string]any{"md": "Your video is ready."} })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.waitGeneratedMedia(ctx, mediaJob{Kind: mediaVideo, OnProgress: func(value int) { values = append(values, value) }}, nil, time.Now())
	if !errors.Is(err, context.DeadlineExceeded) || !slices.Equal(values, []int{40}) {
		t.Fatalf("progress=%v err=%v", values, err)
	}
}
