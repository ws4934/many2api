package inference

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

func TestGIFResponsesPersistOptionsAndAnimation(t *testing.T) {
	f := newImageToolFixture(t)
	var animation bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White})
	if err := gif.EncodeAll(&animation, &gif.GIF{Image: []*image.Paletted{frame, frame, frame}, Delay: []int{23, 23, 23}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	f.adapter.png = animation.Bytes()
	f.adapter.observed = make(chan provider.ImageJobRequest, 1)
	body := `{"model":"gpt-image-2.6","input":"loop","tools":[{"type":"image_generation","output_format":"gif","frame_count":3,"frame_duration_ms":230,"complexity":"medium"}],"background":true}`
	code, queued := f.call(t, "POST", "/v1/responses", body)
	if code != 200 || queued["status"] != "queued" {
		t.Fatalf("create = %d %v", code, queued)
	}
	id := queued["id"].(string)
	// Start after creation: the worker must reconstruct all options from the DB.
	f.startWorkers(t)
	select {
	case request := <-f.adapter.observed:
		if request.OutputFormat != "gif" || request.FrameCount == nil || *request.FrameCount != 3 || request.FrameDurationMS == nil || *request.FrameDurationMS != 230 || request.Complexity != "medium" {
			t.Fatalf("restored request = %+v", request)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not submit")
	}
	var final map[string]any
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		code, final = f.call(t, "GET", "/v1/responses/"+id, "")
		if code != 200 || final["status"] == "completed" || final["status"] == "failed" {
			break
		}
	}
	if code != 200 || final["status"] != "completed" {
		t.Fatalf("result = %d %v", code, final)
	}
	tool := final["tools"].([]any)[0].(map[string]any)
	if tool["frame_count"] != float64(3) || tool["frame_duration_ms"] != float64(230) || tool["complexity"] != "medium" {
		t.Fatalf("echoed tool = %v", tool)
	}
	item := final["output"].([]any)[0].(map[string]any)
	if item["output_format"] != "gif" || item["result"] != base64.StdEncoding.EncodeToString(animation.Bytes()) {
		t.Fatalf("GIF response = %v", item)
	}
}

func TestGIFStreamAndPartialRequestsFailBeforeSubmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))
	request := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2.6","prompt":"loop","output_format":"gif","partial_images":1}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial request = %d %s", w.Code, w.Body.String())
	}
	for _, flags := range []string{`,"stream":true`, ``} {
		tool := `{"type":"image_generation","output_format":"gif","frame_count":3`
		if flags == "" {
			tool += `,"partial_images":1`
		}
		data := `{"model":"gpt-image-2.6","input":"loop","tools":[` + tool + `}]` + flags + `}`
		if _, _, err := decodeImageResponse([]byte(data), true); err == nil {
			t.Fatalf("unsupported GIF response accepted: %s", data)
		}
	}
}

func TestImageGenerationGIFJSONOptions(t *testing.T) {
	var request imageGenerationRequest
	if err := decodeSingleJSON(strings.NewReader(`{"model":"gpt-image-2.6","prompt":"loop","output_format":"gif","frame_count":3,"frame_duration_ms":230,"complexity":"medium","output_compression":0}`), &request, false); err != nil {
		t.Fatal(err)
	}
	if request.FrameCount == nil || *request.FrameCount != 3 || request.FrameDurationMS == nil || *request.FrameDurationMS != 230 || request.Complexity != "medium" || request.OutputCompression == nil || *request.OutputCompression != 0 {
		t.Fatalf("decoded = %+v", request)
	}
}
