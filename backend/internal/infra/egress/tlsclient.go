package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
	"golang.org/x/net/http2"
)

type browserClient struct{ inner tlsclient.HttpClient }

var chromeMajorPattern = regexp.MustCompile(`(?i)Chrome/(\d+)`)

func (l *Lease) DialWebSocket(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration) (*websocket.Conn, *fhttp.Response, error) {
	return l.dialWebSocket(ctx, endpoint, headers, handshakeTimeout, true)
}

// DialWebSocketDeferredForbidden leaves a 403 handshake response for the
// caller to classify before invalidating the browser-session Clearance.
func (l *Lease) DialWebSocketDeferredForbidden(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration) (*websocket.Conn, *fhttp.Response, error) {
	return l.dialWebSocket(ctx, endpoint, headers, handshakeTimeout, false)
}

func (l *Lease) dialWebSocket(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration, invalidateForbidden bool) (*websocket.Conn, *fhttp.Response, error) {
	if l == nil || l.browser == nil {
		return nil, nil, errors.New("当前出口客户端不支持浏览器 WebSocket")
	}
	for attempt := 0; ; attempt++ {
		dialer := &websocket.Dialer{
			HandshakeTimeout:  handshakeTimeout,
			NetDialTLSContext: l.browser.inner.GetTLSDialer(),
			NetDialContext:    l.browser.inner.GetDialer().DialContext,
		}
		connection, response, err := dialer.DialContext(ctx, endpoint, headers)
		if err == nil || !l.proxyPool || attempt >= proxyPoolRetryLimit || !safeProxyConnectionFailure(err, fhttpResponseAsHTTP(response)) {
			if l.proxyPool && safeProxyConnectionFailure(err, fhttpResponseAsHTTP(response)) {
				l.browser.CloseIdleConnections()
			}
			if invalidateForbidden && response != nil && response.StatusCode == http.StatusForbidden && l.clearanceManager != nil && l.clearanceKey != "" {
				l.clearanceManager.invalidateClearanceKey(l.clearanceKey, l.client)
			}
			return connection, response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		l.browser.CloseIdleConnections()
	}
}

func fhttpResponseAsHTTP(response *fhttp.Response) *http.Response {
	if response == nil {
		return nil
	}
	return &http.Response{StatusCode: response.StatusCode, Header: http.Header(response.Header), Body: response.Body}
}

func newBrowserClient(proxyURL, userAgent string) (*browserClient, error) {
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(7200),
		tlsclient.WithClientProfile(browserProfile(userAgent)),
		tlsclient.WithNotFollowRedirects(),
	}
	// tls-client's own SOCKS and HTTP proxy dialers ignore the request context
	// and dial under a client-wide lock, so one silent proxy would stall every
	// request on the client. These dialers end with the request and bound the
	// proxy handshake for HTTP and HTTPS proxies alike.
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	connect, dial, err := tunnelproxy.Route(proxyURL, direct)
	switch {
	case err != nil:
		return nil, fmt.Errorf("浏览器出口代理: %w", err)
	case dial != nil:
		options = append(options, tlsclient.WithDialContext(dial))
	case connect != nil:
		options = append(options, tlsclient.WithDialContext(ConnectDialContext(connect, direct)))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserClient{inner: client}, nil
}

// ConnectDialContext tunnels through an HTTP(S) proxy with CONNECT. The dial and
// the proxy handshake end with ctx and within direct.Timeout.
func ConnectDialContext(proxy *url.URL, direct *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	host := proxy.Host
	if proxy.Port() == "" {
		port := "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(proxy.Hostname(), port)
	}
	authorization := ""
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password))
	}
	return func(ctx context.Context, _, address string) (net.Conn, error) {
		conn, err := direct.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, err
		}
		// Capture the raw socket: TLS replaces conn below and cancellation must
		// not race that assignment.
		raw := conn
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		if err = conn.SetDeadline(time.Now().Add(direct.Timeout)); err == nil && proxy.Scheme == "https" {
			secure := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: proxy.Hostname(), NextProtos: []string{"h2", "http/1.1"}})
			err = secure.HandshakeContext(ctx)
			conn = secure
		}
		request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
		if authorization != "" {
			request.Header.Set("Proxy-Authorization", authorization)
		}
		if secure, ok := conn.(*tls.Conn); err == nil && ok && secure.ConnectionState().NegotiatedProtocol == "h2" {
			conn, err = connectHTTP2(conn, request)
		} else if err == nil {
			err = request.Write(conn)
			if err == nil {
				// Bound proxy response headers and preserve any coalesced target bytes.
				reader := bufio.NewReader(io.LimitReader(conn, 64<<10))
				var response *http.Response
				if response, err = http.ReadResponse(reader, request); err == nil && response.StatusCode != http.StatusOK {
					err = fmt.Errorf("代理 CONNECT 返回 %s", response.Status)
				}
				if err == nil {
					conn = &bufferedProxyConn{Conn: conn, reader: io.MultiReader(reader, conn)}
				}
			}
		}
		if !stop() {
			err = ctx.Err() // ctx ended during the handshake and closed the connection
		}
		if err == nil {
			err = conn.SetDeadline(time.Time{})
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			} else {
				_ = raw.Close()
			}
			return nil, err
		}
		return conn, nil
	}
}

type bufferedProxyConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedProxyConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *bufferedProxyConn) CloseWrite() error          { return closeWrite(c.Conn) }

// One HTTP/2 stream owns this proxy connection, so socket deadlines and closure
// apply to the whole tunnel just as they do for HTTP/1.1 CONNECT.
func connectHTTP2(conn net.Conn, request *http.Request) (net.Conn, error) {
	client, err := (&http2.Transport{}).NewClientConn(conn)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	request = request.Clone(context.Background())
	request.URL = &url.URL{Host: request.Host}
	request.Body = reader
	response, err := client.RoundTrip(request)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = client.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		_ = reader.Close()
		_ = writer.Close()
		_ = client.Close()
		return nil, fmt.Errorf("代理 CONNECT 返回 %s", response.Status)
	}
	return &http2ProxyConn{Conn: conn, writer: writer, reader: response.Body}, nil
}

type http2ProxyConn struct {
	net.Conn
	writer *io.PipeWriter
	reader io.ReadCloser
}

func (c *http2ProxyConn) Read(b []byte) (int, error)  { return c.reader.Read(b) }
func (c *http2ProxyConn) Write(b []byte) (int, error) { return c.writer.Write(b) }
func (c *http2ProxyConn) CloseWrite() error           { return c.writer.Close() }
func (c *http2ProxyConn) Close() error {
	return errors.Join(c.writer.Close(), c.reader.Close(), c.Conn.Close())
}

func browserProfile(userAgent string) profiles.ClientProfile {
	match := chromeMajorPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(match) == 2 {
		if profile, ok := profiles.MappedTLSClients["chrome_"+match[1]]; ok {
			return profile
		}
		major, err := strconv.Atoi(match[1])
		if err == nil {
			bestMajor, bestDistance := 0, int(^uint(0)>>1)
			for _, candidate := range []int{146, 144, 133, 131, 124, 120, 117} {
				distance := candidate - major
				if distance < 0 {
					distance = -distance
				}
				if distance < bestDistance {
					bestMajor, bestDistance = candidate, distance
				}
			}
			if profile, ok := profiles.MappedTLSClients[fmt.Sprintf("chrome_%d", bestMajor)]; ok {
				return profile
			}
		}
	}
	return profiles.Chrome_146
}

func (c *browserClient) Do(request *http.Request) (*http.Response, error) {
	frequest, err := toFHTTPRequest(request)
	if err != nil {
		return nil, err
	}
	fresponse, err := c.inner.Do(frequest)
	if err != nil {
		return nil, err
	}
	return fromFHTTPResponse(fresponse), nil
}

func fromFHTTPResponse(fresponse *fhttp.Response) *http.Response {
	header := http.Header(fresponse.Header).Clone()
	contentLength := fresponse.ContentLength
	if fresponse.Uncompressed {
		header.Del("Content-Encoding")
		header.Del("Content-Length")
		contentLength = -1
	}
	transferEncoding := append([]string(nil), fresponse.TransferEncoding...)
	return &http.Response{
		Status: fresponse.Status, StatusCode: fresponse.StatusCode, Proto: fresponse.Proto,
		ProtoMajor: fresponse.ProtoMajor, ProtoMinor: fresponse.ProtoMinor, Header: header,
		Body: fresponse.Body, ContentLength: contentLength, TransferEncoding: transferEncoding,
		// fhttp 在读取 Body 到 EOF 时原地填充 Trailer，因此这里必须保留共享 map。
		Close: fresponse.Close, Uncompressed: fresponse.Uncompressed, Trailer: http.Header(fresponse.Trailer),
	}
}

func (c *browserClient) CloseIdleConnections() {
	if c != nil && c.inner != nil {
		c.inner.CloseIdleConnections()
	}
}

func toFHTTPRequest(request *http.Request) (*fhttp.Request, error) {
	var body io.Reader
	if request.Body != nil {
		body = request.Body
	}
	result, err := fhttp.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), body)
	if err != nil {
		return nil, err
	}
	result.ContentLength = request.ContentLength
	result.TransferEncoding = append([]string(nil), request.TransferEncoding...)
	result.Close = request.Close
	if request.Host != "" {
		result.Host = request.Host
	}
	if request.GetBody != nil {
		result.GetBody = request.GetBody
	}
	result.Trailer = fhttp.Header(request.Trailer.Clone())
	for name, values := range request.Header {
		for _, value := range values {
			result.Header.Add(name, value)
		}
	}
	return result, nil
}
