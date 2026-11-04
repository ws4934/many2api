package muse

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/gabriel-vasile/mimetype"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
)

type attachmentReference struct {
	URL, Name string
	Kind      mediaKind
}

var audioFormats = map[string]string{
	"mp3": "audio/mpeg", "wav": "audio/wav", "m4a": "audio/mp4",
	"ogg": "audio/ogg", "flac": "audio/flac", "aac": "audio/aac", "opus": "audio/ogg",
}

var referenceExtensions = map[string]string{
	"audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/mp4": ".m4a", "audio/ogg": ".ogg",
	"audio/flac": ".flac", "audio/aac": ".aac",
	"application/pdf": ".pdf", "application/msword": ".doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
}

func attachmentLabels(refs []attachmentReference) string {
	labels := make([]string, 0, len(refs))
	for _, ref := range refs {
		labels = append(labels, "["+string(ref.Kind)+"]")
	}
	return strings.Join(labels, " ")
}

const (
	maxInputAttachments = 10
	maxInputTotalBytes  = 100 << 20
)

func (a *Adapter) readInput(ctx context.Context, ref string) (attachment, error) {
	return a.readAttachment(ctx, ref, mediaImage)
}

// readInputs validates the whole prompt before opening a browser or spending usage.
// Keep the gateway's 20 MiB per-file ceiling; Muse's 100 MiB total is additional.
func (a *Adapter) readInputs(ctx context.Context, images, videos []string) ([]attachment, error) {
	refs := make([]attachmentReference, 0, len(images)+len(videos))
	for _, group := range []struct {
		refs []string
		kind mediaKind
	}{{images, mediaImage}, {videos, mediaVideo}} {
		for _, ref := range group.refs {
			refs = append(refs, attachmentReference{URL: ref, Kind: group.kind})
		}
	}
	return a.readReferences(ctx, refs)
}

func (a *Adapter) readReferences(ctx context.Context, refs []attachmentReference) ([]attachment, error) {
	if len(refs) > maxInputAttachments {
		return nil, badRequest("Muse 每条消息最多支持 10 个输入附件")
	}
	inputs := make([]attachment, 0, len(refs))
	total := 0
	for _, ref := range refs {
		input, err := a.readAttachment(ctx, ref.URL, ref.Kind)
		if err != nil {
			return nil, err
		}
		total += len(input.Data)
		if total > maxInputTotalBytes {
			return nil, badRequest("Muse 输入附件总大小不能超过 100 MiB")
		}
		if name := strings.TrimSpace(ref.Name); name != "" {
			name = path.Base(strings.ReplaceAll(name, `\`, "/"))
			name = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, name)
			if name != "" && name != "." && name != "/" {
				input.Name = strings.TrimSuffix(name, path.Ext(name)) + path.Ext(input.Name)
			}
		}
		input.Name = fmt.Sprintf("reference-%d-%s", len(inputs)+1, input.Name)
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// readAttachment loads a staged job input, a data URI or
// an https URL.
func (a *Adapter) readAttachment(ctx context.Context, ref string, kind mediaKind) (attachment, error) {
	ref = strings.TrimSpace(ref)
	var data []byte
	switch meta, payload, _ := strings.Cut(ref, ","); {
	case provider.IsInputReference(ref):
		raw, _, err := provider.ReadInput(ctx, ref)
		if err != nil {
			return attachment{}, err
		}
		data = raw
	case strings.HasPrefix(meta, "data:") && strings.HasSuffix(meta, ";base64"):
		if len(payload) > base64.StdEncoding.EncodedLen(media.MaxInputAssetBytes) {
			return attachment{}, badRequest("单个输入素材不能超过 20 MiB")
		}
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return attachment{}, badRequest("输入素材的 data URI 不是有效的 Base64")
		}
		data = raw
	case strings.HasPrefix(ref, "https://") && a.fetchRemote != nil:
		raw, err := a.fetchRemote(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return attachment{}, ctx.Err()
			}
			return attachment{}, badRequest("无法下载输入素材（需可公网访问的 https 地址）")
		}
		data = raw
	default:
		return attachment{}, badRequest("输入素材必须是 Base64 data URI 或 https URL")
	}
	if len(data) == 0 || len(data) > media.MaxInputAssetBytes {
		return attachment{}, badRequest("输入素材不能为空，单个不能超过 20 MiB")
	}
	if kind == mediaAudio || kind == mediaAny {
		detected := mimetype.Detect(data).String()
		if detected == "audio/x-m4a" {
			detected = "audio/mp4"
		}
		if ext := referenceExtensions[detected]; ext != "" && (kind == mediaAny || strings.HasPrefix(detected, "audio/")) {
			return attachment{Name: "attachment" + ext, MIME: detected, Data: data}, nil
		}
		if kind == mediaAudio {
			return attachment{}, badRequest("输入音频只支持 MP3、WAV、M4A、Ogg/Opus、FLAC 或 AAC")
		}
	}
	if kind == mediaVideo || (kind == mediaAny && mediafile.VideoType(data) != "") {
		mime := mediafile.VideoType(data)
		ext, ok := mediafile.VideoExtension(mime)
		if !ok {
			return attachment{}, badRequest("参考视频只支持 MP4、MOV 或 WebM")
		}
		seconds, ok := mediafile.VideoDuration(data)
		if !ok {
			return attachment{}, badRequest("无法读取参考视频时长，请提供包含有效时长信息的视频")
		}
		if seconds > 120 {
			return attachment{}, badRequest("参考视频时长不能超过 120 秒")
		}
		return attachment{Name: "video" + ext, MIME: mime, Data: data}, nil
	}
	mime := http.DetectContentType(data)
	extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[mime]
	if extension == "" {
		if kind == mediaAny {
			return attachment{}, badRequest("输入文件只支持图片、MP4/MOV/WebM、音频或 PDF/DOC/DOCX，文件名和声明类型不能代替实际内容")
		}
		return attachment{}, badRequest("输入素材只支持 PNG、JPEG、WebP 或 GIF")
	}
	return attachment{Name: "image." + extension, MIME: mime, Data: data}, nil
}
