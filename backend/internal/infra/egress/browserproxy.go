package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

// BrowserProxyConfig is deployment-only: ListenHost must be an interface reachable
// only by trusted browsers. AdvertiseHost is that interface's name from Chrome.
// Empty values use loopback for routes Chrome cannot handle natively. Explicit
// hosts relay all proxies so Chrome need not reach the upstream proxy network.
// Each session receives its own ephemeral TCP port.
type BrowserProxyConfig struct{ ListenHost, AdvertiseHost string }

func (m *Manager) SetBrowserProxyConfig(cfg BrowserProxyConfig) {
	m.clientMu.Lock()
	m.browserProxyConfig = cfg
	m.clientMu.Unlock()
}

// BrowserProxyLease never contains upstream credentials. Release closes the
// listener, idle HTTP connections and active tunnels, independently of Chrome.
type BrowserProxyLease struct {
	ProxyServer string
	release     func()
}

func (l *BrowserProxyLease) Release() {
	if l != nil && l.release != nil {
		l.release()
	}
}

// AcquireBrowserProxy adapts a selected route for Chrome without reselecting the
// account's node. With a configured relay interface, all proxies are reached by
// the backend. Otherwise native unauthenticated routes retain Chrome's original
// network path; credentials and tunnel protocols use a loopback relay.
func (m *Manager) AcquireBrowserProxy(ctx context.Context, lease *Lease) (*BrowserProxyLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, errors.New("浏览器出口租约未初始化")
	}
	raw := strings.TrimSpace(lease.ProxyURL)
	if raw == "" {
		return &BrowserProxyLease{}, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return nil, errors.New("浏览器出口代理地址无效")
	}
	m.clientMu.RLock()
	cfg := m.browserProxyConfig
	m.clientMu.RUnlock()
	scheme := strings.ToLower(parsed.Scheme)
	if cfg.ListenHost == "" && cfg.AdvertiseHost == "" && parsed.User == nil {
		if scheme == "socks5h" {
			scheme = "socks5"
		}
		if scheme == "socks4a" {
			scheme = "socks4"
		}
		switch scheme {
		case "http", "https", "socks4", "socks5":
			return &BrowserProxyLease{ProxyServer: scheme + "://" + parsed.Host}, nil
		}
	}
	direct := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	connect, dial, err := tunnelproxy.Route(raw, direct)
	if err != nil {
		return nil, errors.New("浏览器出口代理配置无效或协议不支持")
	}
	if connect == nil && dial == nil {
		return nil, errors.New("浏览器出口代理未配置")
	}
	transport := &http.Transport{
		DialContext: direct.DialContext, MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second,
	}
	if connect != nil {
		transport.Proxy = http.ProxyURL(connect)
		dial = ConnectDialContext(connect, direct)
	} else {
		transport.DialContext = dial
	}
	return startBrowserRelay(ctx, cfg, transport, dial)
}

type browserRelay struct {
	ctx       context.Context
	transport *http.Transport
	dial      func(context.Context, string, string) (net.Conn, error)
	mu        sync.Mutex
	closed    bool
	conns     map[net.Conn]struct{}
}

func startBrowserRelay(parent context.Context, cfg BrowserProxyConfig, transport *http.Transport, dial func(context.Context, string, string) (net.Conn, error)) (*BrowserProxyLease, error) {
	host := strings.TrimSpace(cfg.ListenHost)
	if host == "" {
		host = "127.0.0.1"
	}
	advertised := strings.TrimSpace(cfg.AdvertiseHost)
	if advertised == "" {
		advertised = "127.0.0.1"
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(parent, "tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, errors.New("浏览器代理中转监听失败，请检查 browserProxy.listenHost")
	}
	if addr, ok := listener.Addr().(*net.TCPAddr); !ok || addr.IP.IsUnspecified() {
		_ = listener.Close()
		return nil, errors.New("浏览器代理中转必须绑定受信任网络接口")
	}
	ctx, cancel := context.WithCancel(parent)
	relay := &browserRelay{ctx: ctx, transport: transport, conns: make(map[net.Conn]struct{})}
	relay.dial = relay.ownedDial(dial)
	transport.DialContext = relay.ownedDial(transport.DialContext)
	server := &http.Server{
		Handler: relay, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateNew {
				relay.track(c)
			}
			if state == http.StateClosed {
				relay.untrack(c)
			}
		},
	}
	var once sync.Once
	closeRelay := func() {
		once.Do(func() {
			cancel()
			relay.mu.Lock()
			relay.closed = true
			conns := make([]net.Conn, 0, len(relay.conns))
			for c := range relay.conns {
				conns = append(conns, c)
			}
			relay.mu.Unlock()
			for _, c := range conns {
				_ = c.Close()
			}
			_ = listener.Close() // also closes if cancellation wins before Serve starts
			_ = server.Close()
			transport.CloseIdleConnections()
		})
	}
	stop := context.AfterFunc(ctx, closeRelay)
	go func() { _ = server.Serve(listener); closeRelay() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return &BrowserProxyLease{
		ProxyServer: "http://" + net.JoinHostPort(advertised, port),
		release:     func() { stop(); closeRelay() },
	}, nil
}

// http.Transport detaches request cancellation while opening pooled connections.
// Bind those dials and returned sockets to the relay, including TLS handshakes.
func (p *browserRelay) ownedDial(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(p.ctx, cancel)
		defer stop()
		defer cancel()
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		owned := &relayConn{Conn: conn, owner: p}
		p.track(owned)
		return owned, nil
	}
}

type relayConn struct {
	net.Conn
	owner *browserRelay
}

func (c *relayConn) Close() error { c.owner.untrack(c); return c.Conn.Close() }

func (p *browserRelay) track(c net.Conn) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}
func (p *browserRelay) untrack(c net.Conn) { p.mu.Lock(); delete(p.conns, c); p.mu.Unlock() }

func (p *browserRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" || r.URL.User != nil {
		http.Error(w, "invalid proxy request", http.StatusBadRequest)
		return
	}
	req := r.Clone(r.Context())
	req.RequestURI = ""
	upgrade := req.Header.Get("Upgrade")
	stripProxyHopHeaders(req.Header)
	if upgrade != "" {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", upgrade)
	}
	response, err := p.transport.RoundTrip(req)
	if err != nil {
		http.Error(w, "upstream proxy unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		upstream, ok := response.Body.(io.ReadWriteCloser)
		if !ok {
			http.Error(w, "upstream upgrade unavailable", http.StatusBadGateway)
			return
		}
		downstream, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer downstream.Close()
		defer p.untrack(downstream)
		stop := context.AfterFunc(p.ctx, func() { _ = upstream.Close() })
		defer stop()
		// Write only the upgrade headers; Response.Write would consume the tunnel.
		response.Body = nil
		response.Header.Del("Proxy-Authenticate")
		response.Header.Del("Proxy-Authorization")
		if err := response.Write(buffered); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		relayStreams(downstream, buffered, upstream)
		return
	}
	stripProxyHopHeaders(response.Header)
	for k, values := range response.Header {
		w.Header()[k] = values
	}
	w.WriteHeader(response.StatusCode)
	// Flush each copy chunk: event streams must not wait for a 4 KiB buffer.
	_, _ = io.Copy(flushingWriter{w}, response.Body)
}

func (p *browserRelay) connect(w http.ResponseWriter, r *http.Request) {
	if _, port, err := net.SplitHostPort(r.Host); err != nil || port == "" {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "upstream proxy unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(p.ctx, func() { _ = upstream.Close() })
	defer stop()
	downstream, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer downstream.Close()
	defer p.untrack(downstream)
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	relayStreams(downstream, buffered, upstream)
}

// Closing either side terminates both copies, including browser WebSockets.
func relayStreams(downstream net.Conn, reader io.Reader, upstream io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close(); _ = downstream.Close(); close(done) }()
	_, _ = io.Copy(downstream, upstream)
	_ = upstream.Close()
	_ = downstream.Close()
	<-done
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func stripProxyHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
