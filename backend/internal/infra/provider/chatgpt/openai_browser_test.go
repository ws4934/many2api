package chatgpt

import (
	"io"
	"net/http"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type browserWireStub struct {
	tlsclient.HttpClient
	request  *fhttp.Request
	response *fhttp.Response
}

func (s *browserWireStub) Do(req *fhttp.Request) (*fhttp.Response, error) {
	s.request = req
	return s.response, nil
}

type deferredTrailerBody struct{ response *fhttp.Response }

func (b deferredTrailerBody) Read([]byte) (int, error) {
	b.response.Trailer.Set("X-Usage", "42")
	return 0, io.EOF
}
func (deferredTrailerBody) Close() error { return nil }

func TestBrowserHTTPPreservesWireFraming(t *testing.T) {
	source := &fhttp.Response{StatusCode: 200, Body: http.NoBody}
	wire := &browserWireStub{response: source}
	browser := &browserHTTP{client: wire}
	req, _ := http.NewRequest("POST", "https://chatgpt.com/test", strings.NewReader("body"))
	req.TransferEncoding = []string{"chunked"}
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Final": {"done"}}
	response, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if wire.request.GetBody == nil || len(wire.request.TransferEncoding) != 1 || wire.request.Trailer.Get("X-Final") != "done" {
		t.Fatalf("request framing lost: GetBody=%t transfer=%v trailer=%v", wire.request.GetBody != nil, wire.request.TransferEncoding, wire.request.Trailer)
	}
	if len(wire.request.Header[fhttp.HeaderOrderKey]) == 0 {
		t.Fatal("browser header order lost")
	}
}

func TestBrowserHTTPPreservesDeferredTrailersAndDecompression(t *testing.T) {
	source := &fhttp.Response{StatusCode: 200, ContentLength: 128, Uncompressed: true,
		Header:  fhttp.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"128"}, fhttp.HeaderOrderKey: {"fake"}},
		Trailer: fhttp.Header{"X-Usage": nil}, TransferEncoding: []string{"chunked"}}
	source.Body = deferredTrailerBody{source}
	browser := &browserHTTP{client: &browserWireStub{response: source}}
	req, _ := http.NewRequest("GET", "https://chatgpt.com/test", nil)
	response, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.Trailer.Get("X-Usage") != "42" {
		t.Error("deferred response trailers lost")
	}
	if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" || response.Header.Get("Content-Encoding") != "" {
		t.Error("decoded body retains compressed framing")
	}
	if _, ok := response.Header[fhttp.HeaderOrderKey]; ok {
		t.Error("internal header order leaked")
	}
	source.TransferEncoding[0] = "changed"
	if response.TransferEncoding[0] != "chunked" {
		t.Error("transfer encoding not copied")
	}
}
