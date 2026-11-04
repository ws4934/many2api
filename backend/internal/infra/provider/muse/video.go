package muse

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoCheckpoint records the side chat of a sent video prompt, so a restart
// waits in that thread instead of spending usage on a second video.
type videoCheckpoint struct {
	Thread       string   `json:"thread,omitempty"`
	Submitting   bool     `json:"submitting,omitempty"`
	InputDigests []string `json:"input_digests,omitempty"`
}

// museVideoRatios are the shapes the agent renders.
var museVideoRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4"}

// NormalizeVideoRequest: the agent renders 720p and picks the final length
// itself; the requested duration and shape go into the prompt. Other values
// map to the closest it takes: a ratio it lacks, a duration outside 1–15 s,
// and image-only requests beyond 10 attachments keep the first ones. Mixed
// image/video requests must fit together; never silently drop a reference video.
// Reference audios (voice IDs) remain unsupported and are dropped.
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	if options.Operation != provider.VideoOperationGenerate {
		return options, nil
	}
	options.AspectRatio = cmp.Or(provider.ClosestImageRatio(options.AspectRatio, museVideoRatios), "16:9")
	options.Resolution = "720p"
	options.Duration = min(max(cmp.Or(options.Duration, 5), 1), 15)
	limit := maxInputAttachments
	if options.HasImage {
		limit--
	}
	options.ReferenceAudios = 0
	if options.ReferenceVideos > 0 {
		if options.ReferenceImages+options.ReferenceVideos > limit {
			return options, fmt.Errorf("%w: Muse 首帧、参考图片和参考视频合计不能超过 10 个", provider.ErrVideoParameterInvalid)
		}
	} else {
		options.ReferenceImages = min(options.ReferenceImages, limit)
	}
	return options, nil
}

func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	count := options.ReferenceImages + options.ReferenceVideos
	if options.HasImage {
		count++
	}
	if count > maxInputAttachments {
		return fmt.Errorf("%w: Muse 首帧、参考图片和参考视频合计不能超过 10 个", provider.ErrVideoParameterInvalid)
	}
	return nil
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	var checkpoint videoCheckpoint
	if strings.TrimSpace(request.Checkpoint) != "" {
		if err := json.Unmarshal([]byte(request.Checkpoint), &checkpoint); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
	}
	if checkpoint.Submitting && checkpoint.Thread == "" {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, fmt.Errorf("Muse 视频提示已发送但未记录会话，无法安全恢复"))
	}
	job := mediaJob{Kind: mediaVideo, Prompt: videoPrompt(request), Thread: checkpoint.Thread, InputDigests: checkpoint.InputDigests}
	sent := false
	if job.Thread == "" {
		// The first frame goes first: the prompt names it as the first image.
		refs := request.ReferenceURLs
		if request.ImageURL != "" {
			refs = append([]string{request.ImageURL}, refs...)
		}
		var err error
		job.Inputs, err = a.readInputs(ctx, refs, request.ReferenceVideoURLs)
		if err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
		job.InputDigests = inputDigests(job.Inputs)
		if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true, InputDigests: job.InputDigests}); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
		job.OnSent = func() { sent = true }
		job.OnThread = func(path string) error {
			job.Thread = path
			return provider.SaveVideoCheckpoint(request, videoCheckpoint{Thread: path, InputDigests: job.InputDigests})
		}
	}
	var file mediaFile
	err := a.withSession(ctx, request.Credential, func(ctx context.Context, s session) error {
		var err error
		file, err = a.engine.Generate(ctx, s, job)
		return err
	})
	if err != nil {
		err = classify(err)
		switch {
		case job.Thread == "" && !sent:
			// The prompt never left the composer, so nothing was spent: the
			// job can start over, on another account if the gateway picks one.
			if request.SaveCheckpoint != nil {
				_ = request.SaveCheckpoint("")
			}
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, 0, err)
		case job.Thread == "":
			// Sent but the thread is unknown: it cannot be resumed and must
			// not be sent again. The checkpoint stays "submitting".
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, err)
		}
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	asset, err := a.assets.SaveVideo(ctx, request.JobID, cmp.Or(file.MIME, "video/mp4"), bytes.NewReader(file.Data))
	if err != nil {
		return provider.VideoResult{}, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return provider.VideoResult{AssetID: asset.ID, ContentType: asset.MIMEType}, nil
}

// videoPrompt asks for a new video, one that starts from the attached frame,
// or one guided by reference images/videos. Only the explicit ImageURL pins
// a first frame; a reference image must stay a reference when videos accompany it.
func videoPrompt(request provider.VideoRequest) string {
	shape := map[string]string{"16:9": "16:9 landscape", "9:16": "9:16 portrait", "1:1": "1:1 square", "4:3": "4:3", "3:4": "3:4 portrait"}[request.AspectRatio]
	specs := " Aspect ratio: " + cmp.Or(shape, "16:9 landscape") + "."
	if request.Duration > 0 {
		specs += " Duration: " + strconv.Itoa(request.Duration) + " seconds."
	}
	lead := "Create a brand-new video from scratch based only on the description below (text to video; do not use earlier images)."
	switch {
	case len(request.ReferenceVideoURLs) > 0:
		lead = "Create a new video using the videos and images attached to this message as references for the subjects, action, scene and style. Use only these attachments, not earlier media. Do not return an uploaded reference as the result."
		if request.ImageURL != "" {
			lead += " Use the first attached image as the first frame, and the remaining attachments as references."
		}
	case request.ImageURL != "" && len(request.ReferenceURLs) > 0:
		lead = "Create a video that starts from the first image attached to this message as its first frame, uses the other attached images as references for the subjects, scene and style, and follows the description below. Use only the attached images."
	case request.ImageURL != "":
		lead = "Create a video that starts from the image attached to this message as its first frame and follows the description below. Use only the attached image."
	case len(request.ReferenceURLs) > 0:
		lead = "Create a video that uses the images attached to this message as references for the subjects, scene and style and follows the description below. Use only the attached images."
	}
	if len(request.ReferenceVideoURLs) > 0 {
		images := len(request.ReferenceURLs)
		if request.ImageURL != "" {
			images++
		}
		lead += fmt.Sprintf(" Attachment order: %d image(s), then %d video(s). [图N] means image N; [视频N] means video N (attachment %d+N).", images, len(request.ReferenceVideoURLs), images)
	}
	return lead + specs + " Reply with the video.\n\n" + request.Prompt
}
