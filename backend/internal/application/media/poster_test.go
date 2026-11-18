package media

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestVideoPosterCachedAndPrivateInputsExcluded(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required for decoding fixture")
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if msg, err := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x48", "-frames:v", "1", "-c:v", "mpeg4", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, msg)
	}
	clip, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, _, closeDB := newUploadTestService(t)
	defer closeDB()
	ctx := context.Background()
	asset, err := svc.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(clip))
	if err != nil {
		t.Fatal(err)
	}
	data, err := svc.VideoPoster(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 384 || cfg.Height != 288 {
		t.Fatalf("poster: %v %v", cfg, err)
	}
	t.Setenv("PATH", t.TempDir())
	if cached, err := svc.VideoPoster(ctx, asset.ID); err != nil || !bytes.Equal(data, cached) {
		t.Fatal("poster not cached", err)
	}
	input, err := svc.SaveInputVideo(ctx, "video/mp4", bytes.NewReader(clip))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.VideoPoster(ctx, input.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatal("input exposed", err)
	}
	if err := svc.assets.DeleteMediaAsset(ctx, asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.VideoPoster(ctx, asset.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatal("served deleted poster", err)
	}
}
