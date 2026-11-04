package muse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// runtimePage exercises the real CDP transport and page wait/download loops.
// The fixture only supplies DOM observations and media bytes, not a Muse account.
func runtimePage(t *testing.T, eval func(string, []json.RawMessage) any) *page {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://localhost/cdp"}`)
	})
	mux.HandleFunc("/cdp", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, raw, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var req struct {
				ID     int64 `json:"id"`
				Params struct {
					Expression string `json:"expression"`
				} `json:"params"`
			}
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			expr := req.Params.Expression
			call := expr[strings.LastIndex(expr, ").")+2:]
			fn, args, _ := strings.Cut(call, "(")
			var params []json.RawMessage
			if err := json.Unmarshal([]byte("["+strings.TrimSuffix(args, ")")+"]"), &params); err != nil {
				t.Error(err)
				return
			}
			value := eval(fn, params)
			reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": value}}})
			if ws.Write(context.Background(), websocket.MessageText, reply) != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	conn, err := dialCDP(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.close)
	return &page{conn: conn, session: "fixture", log: slog.Default()}
}

func TestGeneratedMediaSkipsReferencesAndResumes(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint("resume=", resume), func(t *testing.T) {
			original := []byte("uploaded-reference-video")
			result := []byte("new-generated-video")
			job := mediaJob{Kind: mediaVideo, Inputs: []attachment{{Data: original}}}
			reported := 0
			job.OnThread = func(string) error { reported++; return nil }
			sentAt := time.Now()
			if resume {
				job.Inputs = nil
				job.InputDigests = inputDigests([]attachment{{Data: original}})
				sentAt = time.Time{}
			}
			var selected []byte
			p := runtimePage(t, func(fn string, args []json.RawMessage) any {
				switch fn {
				case "media":
					var baseline []string
					_ = json.Unmarshal(args[1], &baseline)
					src := "blob:echo-under-new-url"
					selected = original
					if slices.Contains(baseline, src) {
						src = "blob:generated"
						selected = result
					}
					return map[string]any{"md": "Your video is ready.", "ready": map[string]any{"src": src}, "path": "/thread/00000000-0000-0000-0000-000000000001"}
				case "grab":
					return map[string]any{"size": len(selected), "type": "video/mp4"}
				case "chunk":
					return base64.StdEncoding.EncodeToString(selected)
				case "drop":
					return true
				default:
					t.Errorf("unexpected helper %s", fn)
					return nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			file, err := p.waitGeneratedMedia(ctx, job, nil, sentAt)
			if err != nil || string(file.Data) != string(result) {
				t.Fatalf("delivered=%q err=%v", file.Data, err)
			}
			wantReports := 1
			if resume {
				wantReports = 0
			}
			if reported != wantReports {
				t.Fatalf("checkpoint updates=%d, want %d", reported, wantReports)
			}
		})
	}
}

func TestMediaReportWithoutAttachmentHonorsDeadline(t *testing.T) {
	p := runtimePage(t, func(fn string, _ []json.RawMessage) any {
		if fn != "media" {
			t.Errorf("unexpected helper %s", fn)
		}
		return map[string]any{"md": "Your video is ready.", "path": "/thread/00000000-0000-0000-0000-000000000001"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.waitGeneratedMedia(ctx, mediaJob{Kind: mediaVideo}, nil, time.Now())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing attachment: %v", err)
	}
}
