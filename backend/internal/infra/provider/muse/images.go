package muse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (*Adapter) NormalizeImageRequest(options provider.ImageRequestOptions) (provider.ImageRequestOptions, error) {
	options.InputImages = min(options.InputImages, maxInputAttachments)
	return options, nil
}

func (a *Adapter) ValidateImageRequest(options provider.ImageRequestOptions) error {
	switch {
	case options.HasMask:
		return badRequest("Muse 不支持蒙版编辑")
	case options.Streaming || options.PartialImages > 0:
		return badRequest("Muse 不支持流式出图")
	case options.ResponseFormat != "" && options.ResponseFormat != "url" && options.ResponseFormat != "b64_json":
		return badRequest("response_format 只支持 url 或 b64_json")
	case options.Edit && options.InputImages == 0:
		return badRequest("图片编辑需要输入图片")
	}
	return nil
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	prompt := "Create a brand-new image from scratch based only on the description below. Do not use or ask for earlier or reference images. Reply with the image."
	return a.drawImage(ctx, request.Credential, withShape(prompt, request.AspectRatio, request.Size, request.Resolution)+"\n\n"+request.Prompt, nil, request.ResponseFormat)
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	// Keep the first images when a caller sends more than the composer takes.
	refs := request.ImageURLs[:min(len(request.ImageURLs), maxInputAttachments)]
	inputs, err := a.readInputs(ctx, refs, nil)
	if err != nil {
		return failureResponse("", err)
	}
	prompt := "Edit the image attached to this message as described below. Use only the attached images, not earlier pictures. Reply with the edited image."
	return a.drawImage(ctx, request.Credential, withShape(prompt, request.AspectRatio, request.Size, request.Resolution)+"\n\n"+request.Prompt, inputs, request.ResponseFormat)
}

// drawImage asks for one image. Once the prompt is sent the account spent
// usage, so later failures stay errors: the gateway does not draw again.
func (a *Adapter) drawImage(ctx context.Context, credential account.Credential, prompt string, inputs []attachment, responseFormat string) (*provider.Response, error) {
	sent := false
	var file mediaFile
	err := a.withSession(ctx, credential, func(ctx context.Context, s session) error {
		var err error
		file, err = a.engine.Generate(ctx, s, mediaJob{Kind: mediaImage, Prompt: prompt, Inputs: inputs, OnSent: func() { sent = true }})
		return err
	})
	if err != nil {
		return mediaFailure(err, sent)
	}
	item := map[string]any{}
	if responseFormat == "url" && a.assets != nil {
		asset, err := a.assets.SaveImage(ctx, file.Data)
		if err != nil {
			return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
		}
		item["url"] = a.assets.PublicImageURL(asset.ID)
	} else {
		item["b64_json"] = base64.StdEncoding.EncodeToString(file.Data)
	}
	response := jsonResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []any{item}})
	response.QuotaUnits = 1
	return response, nil
}

// mediaFailure hands the gateway what it can act on. Before the prompt is
// sent every classified failure is a response (refresh, quota, failover);
// after it only request-scoped and account-state ones are.
func mediaFailure(err error, sent bool) (*provider.Response, error) {
	var known *museError
	if !errors.As(classify(err), &known) {
		return nil, err
	}
	if sent && !known.requestScoped && known.status != http.StatusUnauthorized && known.status != http.StatusTooManyRequests {
		return nil, known
	}
	return errorResponse("", known.status, known.code, known.message), nil
}

// withShape states the requested aspect ratio and resolution; the agent takes
// them from the prompt, so a size that is no WIDTHxHEIGHT is left out.
func withShape(prompt, aspectRatio, size, resolution string) string {
	label := strings.TrimSpace(aspectRatio)
	if label == "" || strings.EqualFold(label, "auto") {
		label = aspectLabel(size)
	}
	if label != "" {
		prompt += " Aspect ratio: " + label + "."
	}
	if resolution = strings.TrimSpace(resolution); resolution != "" && !strings.EqualFold(resolution, "auto") {
		prompt += " Resolution: about " + strings.ToUpper(resolution) + "."
	}
	return prompt
}

// aspectLabel names the shape of a WIDTHxHEIGHT size, "" when there is none.
func aspectLabel(size string) string {
	width, height, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if !ok || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return ""
	}
	divisor := gcd(w, h)
	shape := "square"
	if w > h {
		shape = "landscape"
	} else if w < h {
		shape = "portrait"
	}
	return fmt.Sprintf("%d:%d (%s)", w/divisor, h/divisor, shape)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
