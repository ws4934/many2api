package muse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func referenceMP4(ticks uint32, brand string) []byte {
	box := func(kind string, data []byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(8+len(data)))
		return append(append(b, kind...), data...)
	}
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], ticks)
	return append(box("ftyp", append([]byte(brand), make([]byte, 4)...)), box("moov", box("mvhd", mvhd))...)
}

func referenceURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func TestReadReferenceVideo(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		name  string
		data  []byte
		mime  string
		valid bool
	}{
		{"mp4-boundary", referenceMP4(120000, "isom"), "video/mp4", true},
		{"quicktime", referenceMP4(6000, "qt  "), "video/quicktime", true},
		{"over-duration", referenceMP4(120001, "isom"), "", false},
		{"missing-duration", referenceMP4(0, "isom"), "", false},
		{"unknown-brand", referenceMP4(6000, "heic"), "", false},
		{"truncated", referenceMP4(6000, "isom")[:25], "", false},
		{"not-video", []byte("%PDF-1.5"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Declared MIME is not trusted; generic MIME still accepts valid bytes.
			got, err := a.readAttachment(context.Background(), referenceURI("application/octet-stream", tc.data), mediaVideo)
			if tc.valid {
				if err != nil || got.MIME != tc.mime || !bytes.Equal(got.Data, tc.data) {
					t.Fatalf("reference=%+v err=%v", got, err)
				}
			} else {
				var bad *museError
				if !errors.As(err, &bad) || bad.status != 400 || !bad.requestScoped {
					t.Fatalf("invalid video: %v", err)
				}
			}
		})
	}
	ref := media.InputReference(media.InputAssetIDPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 24)))
	data := referenceMP4(6000, "isom")
	ctx := provider.WithInputOpener(context.Background(), func(context.Context, string) (provider.InputFile, error) {
		return provider.InputFile{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), MIMEType: "application/octet-stream"}, nil
	})
	if got, err := a.readAttachment(ctx, ref, mediaVideo); err != nil || got.MIME != "video/mp4" {
		t.Fatalf("staged video=%+v %v", got, err)
	}
	a.fetchRemote = func(context.Context, string) ([]byte, error) { return data, nil }
	if _, err := a.readAttachment(ctx, "https://example.test/ref.mp4", mediaVideo); err != nil {
		t.Fatal(err)
	}
	a.fetchRemote = func(context.Context, string) ([]byte, error) { return nil, errors.New("blocked private address") }
	if _, err := a.readAttachment(ctx, "https://example.test/ref.mp4", mediaVideo); err == nil {
		t.Fatal("download failure ignored")
	}
}

func TestReferenceInputBudgets(t *testing.T) {
	a := &Adapter{}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	refs := make([]string, 10)
	for i := range refs {
		refs[i] = referenceURI("image/png", png)
	}
	if got, err := a.readInputs(context.Background(), refs, nil); err != nil || len(got) != 10 {
		t.Fatalf("ten images=%d %v", len(got), err)
	}
	if _, err := a.readInputs(context.Background(), append(refs, refs[0]), nil); err == nil {
		t.Fatal("eleven attachments accepted")
	}
	large := make([]byte, media.MaxInputAssetBytes+1)
	copy(large, png)
	a.fetchRemote = func(context.Context, string) ([]byte, error) { return large, nil }
	if _, err := a.readInputs(context.Background(), []string{"https://example.test/large.png"}, nil); err == nil {
		t.Fatal("oversized input accepted")
	}
	large = large[:media.MaxInputAssetBytes]
	for i := range refs {
		refs[i] = "https://example.test/image.png"
	}
	if got, err := a.readInputs(context.Background(), refs[:5], nil); err != nil || len(got) != 5 {
		t.Fatalf("100 MiB boundary=%d %v", len(got), err)
	}
	if _, err := a.readInputs(context.Background(), refs[:6], nil); err == nil {
		t.Fatal("over 100 MiB accepted")
	}
	if _, err := a.readAttachment(context.Background(), referenceURI("image/png", append(large, 0)), mediaImage); err == nil {
		t.Fatal("oversized data URI accepted")
	}
}

func TestMixedVideoReferencesAndRecovery(t *testing.T) {
	var jobs []mediaJob
	fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
		jobs = append(jobs, job)
		if job.Thread == "" {
			job.OnSent()
			if err := job.OnThread("/thread/reference-test"); err != nil {
				return mediaFile{}, err
			}
		}
		return mediaFile{Data: []byte("generated-video"), MIME: "video/mp4"}, nil
	}}
	a, credential, _ := newTestAdapter(t, fake)
	png := referenceURI("image/png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	mp4 := referenceURI("video/mp4", referenceMP4(6000, "isom"))
	var checkpoint string
	r := provider.VideoRequest{Credential: credential, JobID: "mixed", Prompt: "follow [视频1] with the character from [图1]", ReferenceURLs: []string{png}, ReferenceVideoURLs: []string{mp4}, SaveCheckpoint: func(s string) error { checkpoint = s; return nil }}
	if _, err := a.GenerateVideo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	job := jobs[0]
	if len(job.Inputs) != 2 || job.Inputs[0].MIME != "image/png" || job.Inputs[1].MIME != "video/mp4" {
		t.Fatalf("inputs=%+v", job.Inputs)
	}
	if strings.Contains(job.Prompt, "first frame") || !strings.Contains(job.Prompt, "1 image(s), then 1 video(s)") || !strings.Contains(job.Prompt, "[视频N]") {
		t.Fatalf("prompt=%s", job.Prompt)
	}
	var saved videoCheckpoint
	if err := json.Unmarshal([]byte(checkpoint), &saved); err != nil || len(saved.InputDigests) != 2 {
		t.Fatalf("checkpoint=%s %v", checkpoint, err)
	}
	r.Checkpoint = checkpoint
	// A resumed job must not re-fetch its original inputs or re-submit the prompt.
	r.ReferenceURLs = []string{"https://gone.test/input.png"}
	r.ReferenceVideoURLs = []string{"https://gone.test/input.mp4"}
	if _, err := a.GenerateVideo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(jobs[1].Inputs) != 0 || len(jobs[1].InputDigests) != 2 || jobs[1].OnSent != nil {
		t.Fatalf("resume=%+v", jobs[1])
	}
	r.Checkpoint = ""
	r.ImageURL = png
	r.ReferenceURLs = nil
	r.ReferenceVideoURLs = []string{mp4}
	if _, err := a.GenerateVideo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jobs[2].Prompt, "first attached image as the first frame") {
		t.Fatalf("explicit frame lost: %s", jobs[2].Prompt)
	}
	r.ReferenceVideoURLs = []string{referenceURI("video/mp4", referenceMP4(120001, "isom"))}
	checkpoint = "unchanged"
	if _, err := a.GenerateVideo(context.Background(), r); err == nil {
		t.Fatal("overlong video accepted")
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStagePrepare {
		t.Fatalf("stage=%s", stage)
	}
	if checkpoint != "unchanged" || len(jobs) != 3 {
		t.Fatal("invalid reference submitted or checkpoint overwritten")
	}
}

func TestMuseReferenceCapabilities(t *testing.T) {
	a := &Adapter{}
	registry := provider.NewRegistry(a)
	for _, tc := range []struct {
		images, videos int
		frame, valid   bool
	}{
		{0, 10, false, true}, {9, 1, false, true}, {8, 1, true, true}, {9, 1, true, false}, {0, 11, false, false},
	} {
		o := provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, ReferenceImages: tc.images, ReferenceVideos: tc.videos, HasImage: tc.frame}
		got, err := registry.NormalizeVideoRequest(a.Provider(), o)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v -> %+v %v", o, got, err)
		}
		if tc.valid && (got.ReferenceImages != tc.images || got.ReferenceVideos != tc.videos) {
			t.Fatalf("references silently dropped: %+v", got)
		}
	}
	got, err := registry.NormalizeImageRequest(a.Provider(), provider.ImageRequestOptions{Edit: true, InputImages: 12})
	if err != nil || got.InputImages != 10 {
		t.Fatalf("image normalization=%+v %v", got, err)
	}
}

func TestEditImageKeepsTenReferences(t *testing.T) {
	var seen int
	fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
		seen = len(job.Inputs)
		return mediaFile{Data: []byte("image"), MIME: "image/png"}, nil
	}}
	a, c, _ := newTestAdapter(t, fake)
	refs := make([]string, 12)
	for i := range refs {
		refs[i] = referenceURI("image/png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	}
	resp, err := a.EditImage(context.Background(), provider.ImageEditRequest{Credential: c, ImageURLs: refs, Prompt: "combine"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || seen != 10 {
		t.Fatalf("status=%d inputs=%d", resp.StatusCode, seen)
	}
}
