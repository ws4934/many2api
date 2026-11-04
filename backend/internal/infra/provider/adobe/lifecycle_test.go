package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollStatusTakesPrecedenceOverPartialOutputs(t *testing.T) {
	for _, tc := range []struct{ body, header, want string }{
		{"IN_PROGRESS", "", "IN_PROGRESS"}, {"FAILED", "", "FAILED"}, {"COMPLETED", "FAILED", "FAILED"}, {"COMPLETED", "IN_PROGRESS", "IN_PROGRESS"}, {"COMPLETED", "", "SUCCEEDED"},
	} {
		t.Run(tc.body+"/"+tc.header, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-task-status", tc.header)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": tc.body, "outputs": []any{map[string]any{"image": map[string]any{"presignedUrl": "https://assets.invalid/partial.png"}}}})
			}))
			defer server.Close()
			client := &client{do: server.Client()}
			result, err := client.poll(t.Context(), server.URL, "image")
			if err != nil || result.status != tc.want {
				t.Fatalf("status=%q err=%v, want %q", result.status, err, tc.want)
			}
		})
	}
}

func TestMintRejectsTruncatedHTTPResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		body, _ := json.Marshal(map[string]string{"token": validSherlockToken()})
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
	}))
	defer server.Close()
	if _, err := fetchSherlock(t.Context(), server.Client(), server.URL); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncation not reported: %v", err)
	}
}

func TestSherlockSwitchesSourceDuringInflightLoad(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	tokenA := validSherlockToken()
	tokenB := base64.StdEncoding.EncodeToString([]byte(`{"sid":"new","ark":"a","bfp":"b","ftr":"f"}`))
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tokenA})
	}))
	defer old.Close()
	var hits atomic.Int32
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tokenB})
	}))
	defer next.Close()
	adapter := NewAdapter(Config{SherlockMintAPI: old.URL}, nil, nil, nil, nil)
	finished := make(chan error, 1)
	go func() { _, err := adapter.sherlock.current(t.Context()); finished <- err }()
	<-started
	adapter.UpdateConfig(Config{SherlockMintAPI: next.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	token, err := adapter.sherlock.current(ctx)
	unblock()
	<-finished
	if err != nil || token != tokenB {
		t.Fatalf("new source joined stale load: token=%q err=%v", token, err)
	}
	if token, err := adapter.sherlock.current(t.Context()); err != nil || token != tokenB || hits.Load() != 1 {
		t.Fatalf("stale load replaced new cache: hits=%d err=%v", hits.Load(), err)
	}
	adapter.UpdateConfig(Config{})
	if _, err := adapter.sherlock.current(t.Context()); !errors.Is(err, errNoSherlockSource) {
		t.Fatalf("disabled mint still returned cached token: %v", err)
	}
}

func TestForcedMintWinsOverOverlappingCachedFetch(t *testing.T) {
	for _, normalFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(normalFirst), func(t *testing.T) {
			freshStarted, normalStarted := make(chan struct{}), make(chan struct{})
			freshGate, normalGate := make(chan struct{}), make(chan struct{})
			releaseFresh := sync.OnceFunc(func() { close(freshGate) })
			releaseNormal := sync.OnceFunc(func() { close(normalGate) })
			var normalCalls atomic.Int32
			oldToken := validSherlockToken()
			freshToken := base64.StdEncoding.EncodeToString([]byte(`{"sid":"fresh","ark":"a","bfp":"b","ftr":"f"}`))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := oldToken
				if r.URL.Query().Get("fresh") == "1" {
					close(freshStarted)
					<-freshGate
					token = freshToken
				} else if normalCalls.Add(1) > 1 {
					close(normalStarted)
					<-normalGate
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
			}))
			defer server.Close()
			defer releaseFresh()
			defer releaseNormal()
			service := newSherlockService(func() Config { return Config{SherlockMintAPI: server.URL} })
			if _, err := service.current(t.Context()); err != nil {
				t.Fatal(err)
			}
			freshDone, normalDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := service.forceMint(t.Context()); freshDone <- err }()
			<-freshStarted
			service.mu.Lock()
			service.mintedAt = time.Now().Add(-time.Hour)
			service.mu.Unlock()
			go func() { _, err := service.current(t.Context()); normalDone <- err }()
			<-normalStarted
			if normalFirst {
				releaseNormal()
				if err := <-normalDone; err != nil {
					t.Fatal(err)
				}
				releaseFresh()
				if err := <-freshDone; err != nil {
					t.Fatal(err)
				}
			} else {
				releaseFresh()
				if err := <-freshDone; err != nil {
					t.Fatal(err)
				}
				releaseNormal()
				if err := <-normalDone; err != nil {
					t.Fatal(err)
				}
			}
			if token, err := service.current(t.Context()); err != nil || token != freshToken {
				t.Fatalf("forced mint was overwritten: %q %v", token, err)
			}
		})
	}
}
