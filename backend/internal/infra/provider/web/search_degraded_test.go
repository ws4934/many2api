package web

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestPlaceholderSearchQueriesMatchOnlyObservedSignatures(t *testing.T) {
	for _, query := range []string{
		"current information and recent sources",
		"best expert recommendations and evidence",
		"latest updates and authoritative references",
		"  CURRENT\tINFORMATION and recent\n sources  ",
	} {
		if !isPlaceholderSearchQuery(query) {
			t.Errorf("missed signature %q", query)
		}
	}
	for _, query := range []string{"", "capital of Portugal", "Rust 1.90 release notes", "天空为什么是蓝色", "why the sky looks blue", "current information and recent sources about Rust"} {
		if isPlaceholderSearchQuery(query) {
			t.Errorf("rejected legitimate query %q", query)
		}
	}
}

const searchSessionFrame = `{"event":{"type":"session.created"}}` + "\n"
const searchAnswerFrame = `{"event":{"type":"response.chunk","chunk":{"text":{"text":"answer","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}` + "\n"

func searchCardFrame(query string) string {
	return `{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"search-1","web_search":{"args":{"query":"` + query + `"}}}}}}` + "\n"
}

func TestSearchPreflightRejectsBeforeClientHandoff(t *testing.T) {
	for _, frame := range []string{
		searchCardFrame("current information and recent sources"),
		`{"event":{"type":"response.grok.output","output":{"tool_usage_card":{"web_search":{"query":"best expert recommendations and evidence"}}}}}` + "\n",
	} {
		wire := searchSessionFrame + `{"event":{"type":"response.chunk","chunk":{"text":{"text":"thinking","channel":"CHANNEL_ANALYSIS"}}}}` + "\n" + frame + searchAnswerFrame
		body, err := preflightUpstream(io.NopCloser(strings.NewReader(wire)))
		if body != nil || !errors.Is(err, provider.ErrWebSearchPlaceholder) {
			t.Fatalf("body=%v err=%v", body, err)
		}
		var diagnostic provider.UpstreamDiagnosticError
		if !errors.As(err, &diagnostic) {
			t.Fatalf("missing upstream evidence: %v", err)
		}
		_, evidence := diagnostic.UpstreamDiagnostic()
		if evidence.StatusCode != 200 || !strings.Contains(string(evidence.Body), "web_search") || strings.Contains(string(evidence.Body), "answer") {
			t.Fatalf("diagnostic must preserve the real offending frame: %+v", evidence)
		}
	}
}

func TestSearchPreflightKeepsHealthyStreamAndDoesNotWaitForCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	wire := searchSessionFrame + searchCardFrame("capital of Portugal") + searchAnswerFrame
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, wire)
		written <- err
		// Deliberately keep the upstream open. Preflight must return at the
		// first answer instead of waiting for response.done or EOF.
	}()
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	replay, err := preflightUpstream(reader)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("preflight returned before consuming the first answer")
	}
	_ = writer.Close()
	data, err := io.ReadAll(replay)
	_ = replay.Close()
	if err != nil || string(data) != wire {
		t.Fatalf("replay changed stream: %s, err=%v", data, err)
	}
}

func TestSearchGuardCoversBufferedAndLateStreamingQueries(t *testing.T) {
	wire := searchAnswerFrame + searchCardFrame("latest updates and authoritative references") + searchAnswerFrame
	var visible strings.Builder
	_, err := consumeUpstream(strings.NewReader(wire), func(kind, delta string) error {
		if kind == "text" {
			visible.WriteString(delta)
		}
		return nil
	})
	if !errors.Is(err, provider.ErrWebSearchPlaceholder) || visible.String() != "answer" {
		t.Fatalf("late signature not stopped: output=%q err=%v", visible.String(), err)
	}
	if _, err := consumeUpstream(strings.NewReader(wire), nil); !errors.Is(err, provider.ErrWebSearchPlaceholder) {
		t.Fatalf("buffered response accepted: %v", err)
	}
}

func TestSearchGuardDoesNotInspectAnswerTextOrXSearch(t *testing.T) {
	for _, frame := range []string{
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"current information and recent sources","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"x_search":{"query":"current information and recent sources"}}}}}`,
	} {
		if _, _, err := parseUpstreamFrame([]byte(frame), &parsedChat{}); err != nil {
			t.Fatalf("unrelated content rejected: %v", err)
		}
	}
}
