package media

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
)

type videoPosters struct {
	cache *resultcache.Cache[string, []byte]
	slots chan struct{}
}

func newVideoPosters() videoPosters {
	return videoPosters{resultcache.New[string, []byte](128, time.Hour), make(chan struct{}, 2)}
}

// VideoPoster uses the same visibility checks as playback, including on cache hits.
// Nothing is written to the asset store. Missing decoder support never affects playback.
func (s *Service) VideoPoster(ctx context.Context, id string) ([]byte, error) {
	asset, err := s.publicVideo(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.posters.cache.Load(ctx, asset.ID, time.Now(), func() ([]byte, error) {
		select {
		case s.posters.slots <- struct{}{}:
			defer func() { <-s.posters.slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		f, err := s.objects.Open(ctx, asset.StorageKey)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		decode, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		// fd: allows seeking in the already-open video (moov may be at EOF).
		// Only decode local video containers: no URL, playlist or external references.
		cmd := exec.CommandContext(decode, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "fd", "-format_whitelist", "mov,matroska,webm", "-threads", "1", "-max_pixels", "33554432", "-fd", "0", "-i", "fd:", "-map", "0:v:0", "-frames:v", "1", "-vf", "scale=384:384:force_original_aspect_ratio=decrease", "-threads", "1", "-c:v", "mjpeg", "-q:v", "5", "-f", "image2pipe", "pipe:1")
		cmd.Stdin = f
		data, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errors.New("视频没有可用封面帧")
		}
		return data, nil
	})
}
