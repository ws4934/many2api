package web

import (
	"strings"
	"testing"
)

func TestChatParserDoesNotHideMalformedFrame(t *testing.T) {
	_, err := consumeUpstream(strings.NewReader(`{"result":{"response":{"token":"partial"}}}{"result":}`), nil)
	if err == nil {
		t.Fatal("malformed frame was silently accepted")
	}
}
