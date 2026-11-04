package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testAuthenticatedProxy(t *testing.T, username string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	forward := &httputil.ReverseProxy{Director: func(*http.Request) {}, Transport: transport}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":secret")) {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		calls.Add(1)
		if r.Method != http.MethodConnect {
			forward.ServeHTTP(w, r)
			return
		}
		// Independent upstream fixture: do not run the relay under test twice.
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.(*net.TCPConn).CloseWrite(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func testBrowserProxy(t *testing.T, ctx context.Context, upstream string) *BrowserProxyLease {
	t.Helper()
	relay, err := NewManager(nil, nil).AcquireBrowserProxy(ctx, &Lease{ProxyURL: upstream})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Release)
	if strings.Contains(relay.ProxyServer, "secret") || strings.Contains(relay.ProxyServer, "@") {
		t.Fatal("credentials exposed")
	}
	return relay
}

func testProxyClient(t *testing.T, relay *BrowserProxyLease) *http.Client {
	t.Helper()
	proxy, _ := url.Parse(relay.ProxyServer)
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // local target fixtures only
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestBrowserProxyNativeAndInvalidRoutes(t *testing.T) {
	manager := NewManager(nil, nil)
	// Existing native routes need no relay configuration.
	for raw, want := range map[string]string{"": "", "http://proxy:8080": "http://proxy:8080", "https://proxy:443": "https://proxy:443", "socks5h://warp:1080": "socks5://warp:1080", "socks4://warp:1080": "socks4://warp:1080"} {
		relay, err := manager.AcquireBrowserProxy(t.Context(), &Lease{ProxyURL: raw})
		if err != nil || relay.ProxyServer != want {
			t.Fatalf("%q: %v %v", raw, relay, err)
		}
		relay.Release()
		relay.Release()
	}
	manager.SetBrowserProxyConfig(BrowserProxyConfig{ListenHost: "unreachable.invalid"})
	for _, raw := range []string{"not-a-url", "ftp://user:secret@proxy:21", "vless://bad@proxy:443", "http://user:secret@proxy:80"} {
		if _, err := manager.AcquireBrowserProxy(t.Context(), &Lease{ProxyURL: raw}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid route accepted/leaked: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.AcquireBrowserProxy(ctx, &Lease{}); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestBrowserProxyHTTPAndCONNECTAccountIsolation(t *testing.T) {
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Hop") != "" {
			t.Error("proxy header leaked to target")
		}
		_, _ = io.Copy(w, r.Body)
	})
	plain := httptest.NewServer(target)
	defer plain.Close()
	secure := httptest.NewTLSServer(target)
	defer secure.Close()
	relays := make([]*BrowserProxyLease, 2)
	clients := make([]*http.Client, 2)
	counts := make([]*atomic.Int32, 2)
	for i, user := range []string{"account-one", "account-two"} {
		upstream, count := testAuthenticatedProxy(t, user)
		counts[i] = count
		relays[i] = testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://"+user+":secret@", 1))
		clients[i] = testProxyClient(t, relays[i])
	}
	if relays[0].ProxyServer == relays[1].ProxyServer {
		t.Fatal("shared relay port")
	}
	for round := range 2 {
		for i, client := range clients {
			for _, target := range []string{plain.URL, secure.URL} {
				request, _ := http.NewRequest("POST", target, strings.NewReader("payload"))
				if target == plain.URL {
					request.Header.Set("Proxy-Authorization", "browser-must-not-override-upstream")
					request.Header.Set("Connection", "X-Hop")
					request.Header.Set("X-Hop", "strip-me")
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || string(body) != "payload" {
					t.Fatalf("account %d round %d: %s %q", i, round, response.Status, body)
				}
			}
		}
	}
	for _, count := range counts {
		if count.Load() < 3 {
			t.Fatalf("proxy bypassed: %d", count.Load())
		}
	}
	relays[0].Release()
	if _, err := clients[0].Get(plain.URL); err == nil {
		t.Fatal("released relay still serves")
	}
	response, err := clients[1].Get(plain.URL)
	if err != nil {
		t.Fatal("other account interrupted", err)
	}
	response.Body.Close()
}

func TestBrowserProxyFailureNeverFallsBack(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, status := range []int{407, 502} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		relay := testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://bad:secret@", 1))
		client := testProxyClient(t, relay)
		response, err := client.Get(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != status {
			t.Fatal(response.Status)
		}
		proxy, _ := url.Parse(relay.ProxyServer)
		dial := ConnectDialContext(proxy, &net.Dialer{Timeout: time.Second})
		if conn, err := dial(t.Context(), "tcp", strings.TrimPrefix(target.URL, "http://")); err == nil {
			conn.Close()
			t.Fatal("failed CONNECT succeeded")
		}
		relay.Release()
		upstream.Close()
	}
	if hits.Load() != 0 {
		t.Fatal("fell back to direct")
	}
}

func TestBrowserProxyCancellationClosesTunnelAndListener(t *testing.T) {
	upstream, _ := testAuthenticatedProxy(t, "user")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := testBrowserProxy(t, ctx, strings.Replace(upstream.URL, "://", "://user:secret@", 1))
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	closed := make(chan struct{})
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}()
	proxy, _ := url.Parse(relay.ProxyServer)
	conn, err := ConnectDialContext(proxy, &net.Dialer{Timeout: time.Second})(ctx, "tcp", target.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream tunnel leaked")
	}
	relay.Release() // idempotent and waits for close completion
	if c, err := net.DialTimeout("tcp", proxy.Host, time.Second); err == nil {
		c.Close()
		t.Fatal("listener leaked")
	}
}

func TestBrowserProxyCancelsStalledHTTPDial(t *testing.T) {
	for _, method := range []string{"GET", "CONNECT"} {
		t.Run(method, func(t *testing.T) {
			upstream, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			accepted, closed := make(chan struct{}), make(chan struct{})
			go func() {
				c, err := upstream.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				close(accepted)
				_, _ = io.Copy(io.Discard, c)
				close(closed)
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			relay := testBrowserProxy(t, ctx, "socks5://user:secret@"+upstream.Addr().String())
			done := make(chan struct{})
			client := testProxyClient(t, relay)
			go func() {
				defer close(done)
				if method == "CONNECT" {
					proxy, _ := url.Parse(relay.ProxyServer)
					conn, _ := ConnectDialContext(proxy, &net.Dialer{Timeout: 3 * time.Second})(ctx, "tcp", "target.invalid:80")
					if conn != nil {
						conn.Close()
					}
					return
				}
				req, _ := http.NewRequest(method, "http://target.invalid:80", nil)
				response, _ := client.Do(req)
				if response != nil {
					response.Body.Close()
				}
			}()
			select {
			case <-accepted:
			case <-time.After(2 * time.Second):
				t.Fatal("no upstream connection")
			}
			cancel()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("stalled dial leaked")
			}
			<-done
		})
	}
}

// Minimal SOCKS fixture verifies remote hostname and RFC1929 authentication;
// production uses the shared socksproxy dialer, not a second implementation.
func TestBrowserProxyAuthenticatedSOCKS5(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		read := func(n int) []byte {
			b := make([]byte, n)
			_, err := io.ReadFull(conn, b)
			if err != nil {
				return nil
			}
			return b
		}
		h := read(2)
		if len(h) != 2 {
			result <- fmt.Errorf("greeting")
			return
		}
		read(int(h[1]))
		conn.Write([]byte{5, 2})
		h = read(2)
		if len(h) != 2 {
			result <- fmt.Errorf("auth header")
			return
		}
		user := read(int(h[1]))
		h = read(1)
		if len(h) != 1 {
			result <- fmt.Errorf("password length")
			return
		}
		password := read(int(h[0]))
		if string(user) != "user" || string(password) != "secret" {
			result <- fmt.Errorf("wrong credentials")
			return
		}
		conn.Write([]byte{1, 0})
		h = read(5)
		if len(h) != 5 || h[3] != 3 {
			result <- fmt.Errorf("expected remote DNS")
			return
		}
		host := read(int(h[4]))
		port := read(2)
		if string(host) != "target.invalid" || len(port) != 2 || binary.BigEndian.Uint16(port) != 80 {
			result <- fmt.Errorf("wrong destination")
			return
		}
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			result <- err
			return
		}
		req.Body.Close()
		_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nsocks-ok")
		result <- err
	}()
	relay := testBrowserProxy(t, t.Context(), "socks5://user:secret@"+listener.Addr().String())
	response, err := testProxyClient(t, relay).Get("http://target.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "socks-ok" {
		t.Fatalf("%q %v", body, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBrowserProxyWebSocketAndEventStream(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		conn.Write(r.Context(), kind, data)
		conn.Read(r.Context())
	}))
	defer target.Close()
	upstream, _ := testAuthenticatedProxy(t, "user")
	relay := testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://user:secret@", 1))
	client := testProxyClient(t, relay)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(target.URL, "http"), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("ws-payload")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := ws.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(data) != "ws-payload" {
		t.Fatalf("%d %q %v", kind, data, err)
	}
	response, err := client.Get(target.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: ready\n" {
		t.Fatalf("event not flushed: %q %v", line, err)
	}
	relay.Release()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("released websocket remains open")
	}
	closed := make(chan struct{})
	go func() { _, _ = io.ReadAll(response.Body); close(closed) }()
	select {
	case <-closed: // Either EOF or a truncated-body error is a closed stream.
	case <-time.After(time.Second):
		t.Fatal("released event stream remains open")
	}
}

func TestConnectProxyPreservesEarlyTargetBytes(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\nearly-target-greeting")
	}))
	defer proxy.Close()
	parsed, _ := url.Parse(proxy.URL)
	conn, err := ConnectDialContext(parsed, &net.Dialer{Timeout: time.Second})(t.Context(), "tcp", "target.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, err := io.ReadAll(conn)
	if err != nil || string(data) != "early-target-greeting" {
		t.Fatalf("lost tunnel bytes: %q %v", data, err)
	}
}

func TestBrowserProxyHTTPSRejectsUntrustedCertificate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	relay := testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://user:secret@", 1))
	proxy, _ := url.Parse(relay.ProxyServer)
	conn, err := ConnectDialContext(proxy, &net.Dialer{Timeout: time.Second})(t.Context(), "tcp", "target.invalid:443")
	if err == nil {
		conn.Close()
		t.Fatal("untrusted HTTPS proxy accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("sent credentials through untrusted TLS")
	}
}

func TestBrowserProxyConfiguredInterfaceRelaysNativeRoutes(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "backend-network") }))
	defer target.Close()
	manager := NewManager(nil, nil)
	manager.SetBrowserProxyConfig(BrowserProxyConfig{ListenHost: "127.0.0.1", AdvertiseHost: "localhost"})
	// The proxy endpoint itself is a fixture, not reachable by its target hostname.
	relay, err := manager.AcquireBrowserProxy(t.Context(), &Lease{ProxyURL: target.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Release()
	if relay.ProxyServer == target.URL {
		t.Fatal("native proxy bypassed configured relay")
	}
	response, err := testProxyClient(t, relay).Get("http://only-proxy-can-reach.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "backend-network" {
		t.Fatal(string(body))
	}
}

func TestBrowserProxyTrustedHTTPS(t *testing.T) {
	if upstream := os.Getenv("BROWSER_PROXY_TEST_HTTPS_URL"); upstream != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(os.Getenv("BROWSER_PROXY_TEST_CA"))) {
			t.Fatal("bad fixture CA")
		}
		// Isolated subprocess: no global trust changes leak to other tests.
		t.Setenv("GODEBUG", "x509usefallbackroots=1")
		x509.SetFallbackRoots(roots)
		upstreamURL, _ := url.Parse(upstream)
		dial := ConnectDialContext(upstreamURL, &net.Dialer{Timeout: time.Second})
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if conn, err := dial(ctx, "tcp", "stall.invalid:443"); err == nil {
			conn.Close()
			t.Fatal("stalled CONNECT succeeded")
		}
		if conn, err := dial(t.Context(), "tcp", "denied.invalid:443"); err == nil {
			conn.Close()
			t.Fatal("rejected CONNECT succeeded")
		}

		relay := testBrowserProxy(t, t.Context(), upstream)
		proxy, _ := url.Parse(relay.ProxyServer)
		conn, err := ConnectDialContext(proxy, &net.Dialer{Timeout: 3 * time.Second})(t.Context(), "tcp", "target.invalid:443")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "tunnel-request"); err != nil {
			t.Fatal(err)
		}
		if err := closeWrite(conn); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(conn)
		if err != nil || string(data) != "trusted-https" {
			t.Fatalf("%q %v", data, err)
		}
		return
	}
	for _, protocol := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", protocol), func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != protocol || r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpzZWNyZXQ=" {
					w.WriteHeader(407)
					return
				}
				switch r.Host {
				case "stall.invalid:443":
					<-r.Context().Done()
					return
				case "denied.invalid:443":
					w.WriteHeader(407)
					return
				case "target.invalid:443":
				default:
					w.WriteHeader(502)
					return
				}
				if protocol == 2 {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					data, err := io.ReadAll(r.Body)
					if err != nil || string(data) != "tunnel-request" {
						t.Errorf("h2 tunnel request: %q %v", data, err)
						return
					}
					_, _ = io.WriteString(w, "trusted-https")
					return
				}
				conn, reader, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
				data, err := io.ReadAll(reader)
				if err != nil || string(data) != "tunnel-request" {
					t.Errorf("h1 tunnel request: %q %v", data, err)
					return
				}
				_, _ = io.WriteString(conn, "trusted-https")
			}))
			upstream.EnableHTTP2 = protocol == 2
			upstream.StartTLS()
			defer upstream.Close()
			cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowserProxyTrustedHTTPS$", "-test.timeout=10s")
			command.Env = append(os.Environ(), "BROWSER_PROXY_TEST_HTTPS_URL="+strings.Replace(upstream.URL, "://", "://user:secret@", 1), "BROWSER_PROXY_TEST_CA="+string(cert))
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("HTTPS relay: %v\n%s", err, out)
			}
		})
	}
}

func TestBrowserProxyDoesNotMaskTruncatedResponse(t *testing.T) {
	// Write malformed chunked framing independently of the relay implementation.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
	}))
	defer upstream.Close()
	relay := testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://user:secret@", 1))
	response, err := testProxyClient(t, relay).Get("http://target.invalid/file")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if string(body) != "hello" || err == nil {
		t.Fatalf("truncation was masked: body=%q err=%v", body, err)
	}
}

func TestBrowserProxyPreservesTCPHalfClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, reader, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		body, _ := io.ReadAll(reader)
		if string(body) == "request" {
			_, _ = io.WriteString(conn, "reply")
		}
	}))
	defer upstream.Close()
	relay := testBrowserProxy(t, t.Context(), strings.Replace(upstream.URL, "://", "://user:secret@", 1))
	proxy, _ := url.Parse(relay.ProxyServer)
	conn, err := ConnectDialContext(proxy, &net.Dialer{Timeout: time.Second})(t.Context(), "tcp", "target.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, "request")
	if err := closeWrite(conn); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(conn)
	if string(body) != "reply" || err != nil {
		t.Fatalf("reply lost after half-close: %q %v", body, err)
	}
}
