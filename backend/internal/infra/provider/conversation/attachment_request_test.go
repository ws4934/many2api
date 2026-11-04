package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatAttachmentPolicyIsProviderOwned(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		policy        InputPolicy
		accepted      bool
	}{
		{"legacy-files", `{"type":"file","file":{"file_url":"https://example.test/report.pdf","filename":"report.pdf"}}`, InputPolicy{}, false},
		{"file-provider", `{"type":"file","file":{"file_url":"https://example.test/report.pdf","filename":"report.pdf"}}`, InputPolicy{Files: true}, true},
		{"file-only-rejects-audio", `{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`, InputPolicy{Files: true}, false},
		{"audio-provider", `{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`, InputPolicy{Audio: true}, true},
		{"malformed-file", `{"type":"file","file":[]}`, InputPolicy{Files: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},` + tc.content + `]}]}`)
			out, _, err := ConvertRequestWithOptions(body, "model", OperationChat, tc.policy)
			if (err == nil) != tc.accepted {
				t.Fatalf("err=%v output=%s", err, out)
			}
			if tc.accepted && (!json.Valid(out) || !strings.Contains(string(out), `"inspect"`)) {
				t.Fatalf("invalid conversion: %s", out)
			}
			if tc.name == "file-provider" && (!strings.Contains(string(out), `"type":"input_file"`) || !strings.Contains(string(out), `"filename":"report.pdf"`)) {
				t.Fatalf("file metadata lost: %s", out)
			}
		})
	}
}
