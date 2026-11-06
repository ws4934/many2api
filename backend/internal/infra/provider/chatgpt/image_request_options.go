package chatgpt

import (
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Codex forwards these options; Web conveys them as image-tool prompt hints.
func validateImageOutputRequest(o provider.ImageRequestOptions) error {
	if o.Complexity != "" {
		switch o.Complexity {
		case "low", "medium", "high":
		default:
			return invalidRequest("complexity must be low, medium, or high")
		}
	}
	if !strings.EqualFold(strings.TrimSpace(o.OutputFormat), "gif") {
		if o.FrameCount != nil || o.FrameDurationMS != nil {
			return invalidRequest("frame_count and frame_duration_ms require output_format=gif")
		}
		return nil
	}
	if o.Streaming || o.PartialImages != 0 || o.HasMask || o.OutputCompression != nil {
		return invalidRequest("GIF output does not support streaming, partial_images, masks, or output_compression")
	}
	if o.Background == "transparent" {
		return invalidRequest("background=transparent requires output_format=png or webp")
	}
	// Web routes are aliases for a conversation image tool. Its prompt hints
	// are not subject to the native Images API's model and numeric limits.
	engine, native := codexImageEngine(o.UpstreamModel)
	if !native {
		return nil
	}
	switch engine {
	case "gpt-image-2.6-flare", "gpt-image-2.6-sunburst":
	default:
		return invalidRequest("GIF output requires a GPT Image 2.6 Codex route")
	}
	if o.FrameCount != nil && (*o.FrameCount < 2 || *o.FrameCount > 16) {
		return invalidRequest("frame_count must be between 2 and 16")
	}
	if o.FrameDurationMS != nil && *o.FrameDurationMS < 20 {
		return invalidRequest("frame_duration_ms must be at least 20")
	}
	if o.Size != "" && o.Size != "auto" {
		var w, h int
		_, err := fmt.Sscanf(o.Size, "%dx%d", &w, &h)
		if err != nil || fmt.Sprintf("%dx%d", w, h) != o.Size || w <= 0 || h <= 0 || w%16 != 0 || h%16 != 0 {
			return invalidRequest("GIF size must be auto or positive WIDTHxHEIGHT dimensions divisible by 16")
		}
	}
	return nil
}
