package grok

import (
	"cmp"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// grokVideoRatios are the aspect ratios the Grok video APIs take.
var grokVideoRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"}

// NormalizeVideoRequest fills the xAI defaults and maps other values onto
// the xAI ranges instead of refusing them, since clients written for other
// video APIs send theirs: generation defaults to 16:9, 720p and 8 s and takes
// the closest ratio and resolution and a 1–15 s duration, where only
// grok-imagine-video-1.5 without references renders 1080p; an edit takes no
// duration, aspect ratio or resolution; an extension runs 2–10 s, 6 s by
// default.
func NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	switch options.Operation {
	case provider.VideoOperationGenerate:
	case provider.VideoOperationEdit:
		options.AspectRatio, options.Resolution, options.Duration = "", "", 0
		return options, nil
	case provider.VideoOperationExtend:
		options.AspectRatio, options.Resolution = "", ""
		options.Duration = min(max(cmp.Or(options.Duration, 6), 2), 10)
		return options, nil
	default:
		return options, nil
	}
	options.AspectRatio = cmp.Or(provider.ClosestImageRatio(options.AspectRatio, grokVideoRatios), "16:9")
	options.Duration = min(max(cmp.Or(options.Duration, 8), 1), 15)
	tiers := []string{"480p", "720p"}
	if strings.TrimSpace(options.UpstreamModel) == "grok-imagine-video-1.5" && options.ReferenceImages == 0 && options.ReferenceAudios == 0 && !options.HasLastFrame && options.Keyframes == 0 {
		tiers = append(tiers, "1080p")
	}
	options.Resolution = cmp.Or(provider.ClosestImageTier(options.Resolution, tiers), "720p")
	return options, nil
}

// FitVideoInputs fits Build and classic Console's exclusive input modes:
// the first frame wins over references, otherwise at most maxReferences remain.
func FitVideoInputs(options provider.VideoRequestOptions, maxReferences int) provider.VideoRequestOptions {
	if options.HasImage {
		options.ReferenceImages, options.ReferenceAudios = 0, 0
	}
	options.ReferenceImages = min(options.ReferenceImages, maxReferences)
	return options
}
