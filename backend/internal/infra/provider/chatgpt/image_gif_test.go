package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func animatedGIF(t *testing.T, frames int) []byte {
	t.Helper()
	value := &gif.GIF{LoopCount: 3}
	for i := 0; i < frames; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White})
		frame.Pix[i%len(frame.Pix)] = 1
		value.Image = append(value.Image, frame)
		value.Delay = append(value.Delay, 20+i)
		value.Disposal = append(value.Disposal, gif.DisposalBackground)
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, value); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestGIFOutputPreservesOriginalBytes(t *testing.T) {
	for _, frames := range []int{1, 3, 17} {
		raw := animatedGIF(t, frames)
		mime, decoded, err := decodeImageDownload(raw)
		if err != nil {
			t.Fatal(err)
		}
		output, mime, err := (ImageOutputOptions{OutputFormat: "gif", Background: "opaque"}).applyDecoded(downloadedImage{raw: raw, mime: mime, decoded: decoded})
		if err != nil || mime != "image/gif" || !bytes.Equal(output, raw) {
			t.Fatalf("upstream GIF with %d frames changed: %s %v", frames, mime, err)
		}
	}
	if _, _, err := (ImageOutputOptions{OutputFormat: "gif"}).applyDecoded(downloadedImage{raw: testPNG(), mime: "image/png"}); err == nil {
		t.Fatal("PNG passed as GIF")
	}
}

func TestGIFRequestContract(t *testing.T) {
	a := &Adapter{}
	n, delay, zero := 3, 230, 0
	base := provider.ImageRequestOptions{UpstreamModel: "gpt-image-2.6", OutputFormat: "gif", FrameCount: &n, FrameDurationMS: &delay, Complexity: "high"}
	for _, model := range []string{"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "codex-gpt-image-2.6-flare", "codex-gpt-image-2.6-sunburst"} {
		o := base
		o.UpstreamModel = model
		if err := a.ValidateImageRequest(o); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
	}
	for name, change := range map[string]func(*provider.ImageRequestOptions){
		"old model":    func(o *provider.ImageRequestOptions) { o.UpstreamModel = "gpt-image-2" },
		"stream":       func(o *provider.ImageRequestOptions) { o.Streaming = true },
		"partial":      func(o *provider.ImageRequestOptions) { o.PartialImages = 1 },
		"mask":         func(o *provider.ImageRequestOptions) { o.Edit = true; o.InputImages = 1; o.HasMask = true },
		"compression":  func(o *provider.ImageRequestOptions) { o.OutputCompression = &zero },
		"frames":       func(o *provider.ImageRequestOptions) { o.FrameCount = &zero },
		"delay":        func(o *provider.ImageRequestOptions) { o.FrameDurationMS = &zero },
		"still format": func(o *provider.ImageRequestOptions) { o.OutputFormat = "png" },
		"complexity":   func(o *provider.ImageRequestOptions) { o.Complexity = "maximum" },
		"size":         func(o *provider.ImageRequestOptions) { o.Size = "513x512" },
		"transparent":  func(o *provider.ImageRequestOptions) { o.Background = "transparent" },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			change(&o)
			if a.ValidateImageRequest(o) == nil {
				t.Fatal("unsupported GIF request accepted")
			}
		})
	}
}

func TestCodexImage26GIFGenerationAndEdit(t *testing.T) {
	raw := animatedGIF(t, 3)
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		payloads = append(payloads, body)
		if !strings.HasPrefix(r.URL.Path, "/backend-api/codex/images/") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(raw)}}})
	}))
	defer server.Close()
	a, assets := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	n, delay := 3, 230
	for _, model := range []string{"codex-gpt-image-2.6-flare", "codex-gpt-image-2.6-sunburst"} {
		response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: model, Prompt: "loop", OutputFormat: "gif", FrameCount: &n, FrameDurationMS: &delay, Complexity: "medium", Quality: "HD", Size: "128x128"})
		if err != nil || response == nil || response.StatusCode != 200 {
			t.Fatalf("generate: %+v %v", response, err)
		}
		body := decodeBody(t, response)
		data, err := base64.StdEncoding.DecodeString(body["data"].([]any)[0].(map[string]any)["b64_json"].(string))
		if err != nil || !bytes.Equal(data, raw) || body["output_format"] != "gif" {
			t.Fatalf("GIF response changed: %v", err)
		}
		payload := payloads[len(payloads)-1]
		if payload["model"] != strings.TrimPrefix(model, "codex-") || payload["frame_count"] != float64(n) || payload["frame_duration_ms"] != float64(delay) || payload["complexity"] != "medium" || payload["size"] != "128x128" || payload["quality"] != "high" {
			t.Fatalf("payload = %v", payload)
		}
	}
	response, err := a.EditImage(context.Background(), provider.ImageEditRequest{Credential: credential, Model: "codex-gpt-image-2.6-flare", Prompt: "animate", ImageURLs: []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG())}, OutputFormat: "gif", FrameCount: &n, FrameDurationMS: &delay, ResponseFormat: "url"})
	if err != nil || response == nil || response.StatusCode != 200 {
		t.Fatalf("edit: %+v %v", response, err)
	}
	body := decodeBody(t, response)
	if body["data"].([]any)[0].(map[string]any)["url"] == nil || assets.saved != 3 || payloads[2]["frame_count"] != float64(n) {
		t.Fatalf("edit = %v, payload %v", body, payloads[2])
	}
}

func TestWebImage26GIFSyncAndRecovery(t *testing.T) {
	var submits atomic.Int32
	base := imageUpstream(t, "", 0, &submits)
	raw := animatedGIF(t, 3)
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/files/file_abc123456/download" {
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write(raw)
			return
		}
		if r.URL.Path == "/backend-api/f/conversation" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			encoded, _ := json.Marshal(payload)
			prompts = append(prompts, string(encoded))
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	a, _ := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	n, delay := 3, 230
	response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2.6", Prompt: "loop", OutputFormat: "gif", FrameCount: &n, FrameDurationMS: &delay})
	if err != nil || response == nil || response.StatusCode != 200 {
		t.Fatalf("sync: %+v %v", response, err)
	}
	body := decodeBody(t, response)
	encoded := body["data"].([]any)[0].(map[string]any)["b64_json"].(string)
	if encoded != base64.StdEncoding.EncodeToString(raw) {
		t.Fatal("Web animation flattened")
	}
	request := provider.ImageJobRequest{Credential: credential, Model: "gpt-image-2.6-flare", Prompt: "loop", OutputFormat: "gif", FrameCount: &n, FrameDurationMS: &delay}
	cp, err := a.SubmitImage(context.Background(), request, func(provider.ImageCheckpoint) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cp, err = a.PollImage(context.Background(), request, cp)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.FetchImage(context.Background(), request, cp)
	if err != nil || !bytes.Equal(data, raw) || submits.Load() != 2 {
		t.Fatalf("recovery changed GIF or resubmitted: %v, %d", err, submits.Load())
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt, "animated GIF") || !strings.Contains(prompt, "frame_count: 3") || !strings.Contains(prompt, "frame_duration_ms: 230") || strings.Contains(prompt, "Output file format: PNG") {
			t.Fatalf("prompt = %s", prompt)
		}
	}
}
