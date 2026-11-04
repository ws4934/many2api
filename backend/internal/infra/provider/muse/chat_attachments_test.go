package muse

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func referenceWAV() []byte {
	b := make([]byte, 46)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 38)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 2)
	return b
}

func referenceDOCX(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, v := range []struct{ name, content string }{{"[Content_Types].xml", `<Types/>`}, {"word/document.xml", `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p/></w:body></w:document>`}} {
		f, err := w.Create(v.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, v.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestMuseReadsAudioAndDocumentsByContent(t *testing.T) {
	pdf := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF")
	for _, tc := range []struct {
		name string
		data []byte
		mime string
	}{
		{"wav", referenceWAV(), "audio/wav"},
		{"pdf", pdf, "application/pdf"},
		{"docx", referenceDOCX(t), "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"m4a", append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'M', '4', 'A', ' ', 0, 0, 0, 0}, []byte("M4A isom")...), "audio/mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{}
			got, err := a.readReferences(context.Background(), []attachmentReference{{URL: referenceURI("application/octet-stream", tc.data), Name: `C:\tmp\sample.fake`, Kind: mediaAny}})
			if err != nil || len(got) != 1 {
				t.Fatalf("read: %v", err)
			}
			if got[0].MIME != tc.mime || !bytes.Equal(got[0].Data, tc.data) || strings.ContainsAny(got[0].Name, `/\`) || strings.HasSuffix(got[0].Name, ".fake") {
				t.Fatalf("attachment=%+v", got[0])
			}
		})
	}
	a := &Adapter{}
	if _, err := a.readAttachment(context.Background(), referenceURI("audio/wav", pdf), mediaAudio); err == nil {
		t.Fatal("PDF passed as audio")
	}
	if _, err := a.readAttachment(context.Background(), referenceURI("application/pdf", []byte("<html>fake PDF</html>")), mediaAny); err == nil {
		t.Fatal("HTML passed as PDF")
	}
}

func TestMuseChatAttachmentsAcrossProtocols(t *testing.T) {
	wav := base64.StdEncoding.EncodeToString(referenceWAV())
	pdf := referenceURI("application/pdf", []byte("%PDF-1.7\n%%EOF"))
	for _, tc := range []struct {
		name, operation, body string
		want                  []string
	}{
		{"responses", conversation.OperationResponses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_file","filename":"report.pdf","file_data":"` + pdf + `"},{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"wav"}}]}]}`, []string{"application/pdf", "audio/wav"}},
		{"chat", conversation.OperationChat, `{"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"file","file":{"filename":"report.pdf","file_data":"` + pdf + `"}},{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"wav"}}]}]}`, []string{"application/pdf", "audio/wav"}},
		{"messages", conversation.OperationMessages, `{"max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + strings.SplitN(pdf, ",", 2)[1] + `"},"title":"report.pdf"}]}]}`, []string{"application/pdf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			fake := &fakeEngine{chat: func(turn chatTurn, onText func(string) error) (string, error) {
				calls++
				if turn.Prompt != "inspect" || len(turn.Inputs) != len(tc.want) {
					t.Fatalf("turn=%+v", turn)
				}
				for i, want := range tc.want {
					if turn.Inputs[i].MIME != want {
						t.Fatalf("input %d=%s", i, turn.Inputs[i].MIME)
					}
				}
				return "read", onText("read")
			}}
			a, c, _ := newTestAdapter(t, fake)
			r := provider.ResponseResourceRequest{Credential: c, Method: http.MethodPost, Path: "/responses", Model: modelChat, Operation: tc.operation, NormalizeBody: true, Body: []byte(tc.body)}
			for _, stream := range []bool{false, true} {
				r.Streaming = stream
				resp, err := a.ForwardResponse(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !bytes.Contains(body, []byte("read")) {
					t.Fatalf("response=%d %s %v", resp.StatusCode, body, err)
				}
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestMuseAttachmentHistoryAndInvalidSources(t *testing.T) {
	input := json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_url":"https://gone.test/old.pdf"}]},{"role":"assistant","content":"old"},{"role":"user","content":[{"type":"input_file","file_url":"https://current.test/new.pdf"}]}]`)
	prompt, refs, err := buildChatPrompt(responsesRequest{Input: input})
	if err != nil || len(refs) != 1 || refs[0].URL != "https://current.test/new.pdf" || !strings.Contains(prompt, "[file]") {
		t.Fatalf("history=%s %+v %v", prompt, refs, err)
	}
	for _, part := range []string{
		`{"type":"input_file","file_id":"external"}`, `{"type":"input_file"}`,
		`{"type":"input_file","file_url":"https://x.test/a","file_data":"AAAA"}`,
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"exe"}}`,
		`{"type":"input_audio"}`,
	} {
		if _, _, err := messageContent(json.RawMessage("[" + part + "]")); err == nil {
			t.Fatalf("invalid part accepted: %s", part)
		}
	}
}
