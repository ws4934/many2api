package muse

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type proxyAccountRepository struct {
	egressRepositoryStub
	nodes map[uint64]egressdomain.Node
}

func (r proxyAccountRepository) GetEgressNode(_ context.Context, id uint64) (egressdomain.Node, error) {
	return r.nodes[id], nil
}

// Run only against a disposable Chrome launched with --ignore-certificate-errors.
// No Muse account, Internet target or paid generation is used. In Docker set
// MUSE_TEST_RELAY_HOST to this test container's private network alias.
func TestLiveAuthenticatedBrowserProxy(t *testing.T) {
	if os.Getenv("MUSE_LIVE_PROXY_FIXTURE") != "1" {
		t.Skip("MUSE_LIVE_PROXY_FIXTURE not set")
	}
	engine := liveEngine(t)
	adapter, credential, _ := newTestAdapter(t, &fakeEngine{})
	nodes := make(map[uint64]egressdomain.Node)
	credentials := make([]account.Credential, 2)
	counts := make([]*atomic.Int32, 2)
	for i, label := range []string{"one", "two"} {
		var calls atomic.Int32
		counts[i] = &calls
		target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Proxy-Authorization") != "" {
				t.Error("upstream credentials leaked")
			}
			cookie, err := r.Cookie("hatch_sess")
			if err != nil || cookie.Value != label {
				t.Errorf("cookie isolation: %v %v", cookie, err)
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><textarea></textarea><div id="account">%s</div></body></html>`, label)
		}))
		t.Cleanup(target.Close)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(label+":secret")) {
				w.WriteHeader(407)
				return
			}
			if r.Method != "CONNECT" || r.Host != "muse.ai:443" {
				w.WriteHeader(502)
				return
			}
			calls.Add(1)
			out, err := net.Dial("tcp", target.Listener.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer out.Close()
			in, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer in.Close()
			io.WriteString(in, "HTTP/1.1 200 Connection Established\r\n\r\n")
			done := make(chan struct{})
			go func() { io.Copy(out, buffered); out.Close(); close(done) }()
			io.Copy(in, out)
			in.Close()
			<-done
		}))
		t.Cleanup(upstream.Close)
		proxy := strings.Replace(upstream.URL, "://", "://"+label+":secret@", 1)
		encrypted, err := adapter.cipher.Encrypt(proxy)
		if err != nil {
			t.Fatal(err)
		}
		id := uint64(i + 1)
		nodes[id] = egressdomain.Node{ID: id, Scope: scope, Enabled: true, Health: 1, EncryptedProxyURL: encrypted}
		credentials[i] = credential
		credentials[i].ID = id
		credentials[i].EgressNodeID = id
		credentials[i].EncryptedAccessToken, err = adapter.encryptJar([]cookie{{Name: "hatch_sess", Value: label, Domain: ".muse.ai", Path: "/", Secure: true}})
		if err != nil {
			t.Fatal(err)
		}
	}
	manager := infraegress.NewManager(proxyAccountRepository{nodes: nodes}, adapter.cipher)
	host := os.Getenv("MUSE_TEST_RELAY_HOST")
	manager.SetBrowserProxyConfig(infraegress.BrowserProxyConfig{ListenHost: host, AdvertiseHost: host})
	adapter.egress = manager
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 2)
	addresses := make(chan string, 2)
	for i, credential := range credentials {
		go func() {
			done <- adapter.withSession(ctx, credential, func(ctx context.Context, s session) error {
				open := s.OpenProxy
				s.OpenProxy = func(ctx context.Context) (string, func(), error) {
					server, release, err := open(ctx)
					if err == nil {
						addresses <- server
						if strings.Contains(server, "secret") || strings.Contains(server, "@") {
							t.Error("credential in browser URL")
						}
					}
					return server, release, err
				}
				return engine.withPage(ctx, 20*time.Second, s, "/thread/new", func(ctx context.Context, p *page) error {
					var result struct {
						Result struct {
							Value string `json:"value"`
						} `json:"result"`
					}
					if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": "document.querySelector('#account').textContent", "returnByValue": true}, &result); err != nil {
						return err
					}
					if result.Result.Value != []string{"one", "two"}[i] {
						return fmt.Errorf("wrong proxy account: %s", result.Result.Value)
					}
					return nil
				})
			})
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, count := range counts {
		if count.Load() == 0 {
			t.Fatal("proxy bypassed")
		}
	}
	a, b := <-addresses, <-addresses
	if a == b {
		t.Fatal("shared relay")
	}
	for _, address := range []string{a, b} {
		parsed, _ := url.Parse(address)
		if conn, err := net.DialTimeout("tcp", parsed.Host, time.Second); err == nil {
			conn.Close()
			t.Fatal("relay not released")
		}
	}
	t.Log("two Chrome contexts reached their own authenticated upstreams through private relay DNS; cookies isolated; listeners released")
}

func TestBrowserProxyAllocatedOnlyAfterPageSlot(t *testing.T) {
	adapter, credential, _ := newTestAdapter(t, &fakeEngine{})
	encrypted, err := adapter.cipher.Encrypt("http://user:secret@127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	credential.EgressNodeID = 1
	manager := infraegress.NewManager(proxyAccountRepository{nodes: map[uint64]egressdomain.Node{1: {ID: 1, Scope: scope, Enabled: true, Health: 1, EncryptedProxyURL: encrypted}}}, adapter.cipher)
	adapter.egress = manager
	chrome := newFakeChrome(t) // connects but rejects context creation
	engine := newBrowserEngine(func() Config { return Config{BrowserURL: chrome.URL, MaxPages: 1} }, adapter.log)
	t.Cleanup(func() {
		if engine.conn != nil {
			engine.conn.close()
		}
	})
	var opened, released atomic.Int32
	var endpoint string
	run := func(ctx context.Context) error {
		return adapter.withSession(ctx, credential, func(ctx context.Context, s session) error {
			open := s.OpenProxy
			s.OpenProxy = func(ctx context.Context) (string, func(), error) {
				opened.Add(1)
				server, release, err := open(ctx)
				if err != nil {
					return "", nil, err
				}
				endpoint = server
				return server, func() { release(); released.Add(1) }, nil
			}
			return engine.withPage(ctx, time.Second, s, "/thread/new", func(context.Context, *page) error { t.Error("unexpected page"); return nil })
		})
	}
	// Saturated page capacity must not open any relays for queued requests.
	if err := engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 64)
	for range 64 {
		go func() { done <- run(ctx) }()
	}
	for range 64 {
		if err := <-done; err == nil {
			t.Fatal("queued request unexpectedly ran")
		}
	}
	if opened.Load() != 0 {
		t.Fatalf("queued requests opened %d relays", opened.Load())
	}
	engine.release()

	// A free slot opens a relay; a CDP failure still releases it and the slot.
	if err := run(t.Context()); err == nil {
		t.Fatal("expected CDP failure")
	}
	if opened.Load() != 1 || released.Load() != 1 || engine.active != 0 {
		t.Fatalf("opened=%d released=%d active=%d", opened.Load(), released.Load(), engine.active)
	}
	parsed, _ := url.Parse(endpoint)
	if conn, err := net.DialTimeout("tcp", parsed.Host, time.Second); err == nil {
		conn.Close()
		t.Fatal("failed page leaked relay")
	}

	manager.SetBrowserProxyConfig(infraegress.BrowserProxyConfig{ListenHost: "0.0.0.0"})
	err = run(t.Context())
	known, ok := err.(*museError)
	if !ok || known.status != 503 || !known.requestScoped || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad deployment error: %v", err)
	}
	if engine.active != 0 {
		t.Fatal("failed proxy setup leaked page slot")
	}
}
