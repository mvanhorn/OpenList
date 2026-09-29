package net

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
)

type closeTrackBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *closeTrackBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

// trackingTransport wraps the production HTTP transport so tests can see
// whether RequestHttp closed the upstream body.
type trackingTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies []*closeTrackBody
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	wrapped := &closeTrackBody{ReadCloser: resp.Body}
	resp.Body = wrapped
	t.mu.Lock()
	t.bodies = append(t.bodies, wrapped)
	t.mu.Unlock()
	return resp, nil
}

func (t *trackingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.bodies)
}

func (t *trackingTransport) lastBody() *closeTrackBody {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) == 0 {
		return nil
	}
	return t.bodies[len(t.bodies)-1]
}

func useTrackingTransport(t *testing.T) *trackingTransport {
	t.Helper()
	previousConf := conf.Conf
	conf.Conf = &conf.Config{}
	t.Cleanup(func() { conf.Conf = previousConf })

	client := HttpClient()
	previous := client.Transport
	tr := &trackingTransport{base: previous}
	client.Transport = tr
	t.Cleanup(func() {
		client.Transport = previous
	})
	return tr
}

func gzipBytes(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestRequestHttpGzipErrorBodies(t *testing.T) {
	tr := useTrackingTransport(t)
	validForbidden := []byte("denied 权限不足")
	validGateway := []byte("bad gateway 网关")
	truncatedPlain := bytes.Repeat([]byte("truncated-error-权限-"), 20)
	truncatedGzip := gzipBytes(t, truncatedPlain)
	truncatedGzip = truncatedGzip[:len(truncatedGzip)/2]

	cases := []struct {
		name     string
		path     string
		status   int
		body     []byte
		gzipBody bool
		wantText string
	}{
		{name: "gzip 403", path: "/forbidden", status: http.StatusForbidden, body: gzipBytes(t, validForbidden), gzipBody: true, wantText: string(validForbidden)},
		{name: "gzip 502", path: "/gateway", status: http.StatusBadGateway, body: gzipBytes(t, validGateway), gzipBody: true, wantText: string(validGateway)},
		{name: "invalid gzip", path: "/invalid", status: http.StatusInternalServerError, body: []byte("this is not gzip"), gzipBody: true, wantText: "gzip: invalid header"},
		{name: "empty gzip", path: "/empty", status: http.StatusNotFound, gzipBody: true, wantText: "response:EOF"},
		{name: "truncated gzip", path: "/truncated", status: http.StatusServiceUnavailable, body: truncatedGzip, gzipBody: true, wantText: "unexpected EOF"},
		{name: "plain error", path: "/plain", status: http.StatusBadRequest, body: []byte("quota exceeded: 你好"), wantText: "quota exceeded: 你好"},
	}
	handlers := make(map[string]struct {
		status   int
		body     []byte
		gzipBody bool
	}, len(cases))
	for _, tc := range cases {
		handlers[tc.path] = struct {
			status   int
			body     []byte
			gzipBody bool
		}{status: tc.status, body: tc.body, gzipBody: tc.gzipBody}
	}
	var seenMu sync.Mutex
	var seenAE []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seenAE = append(seenAE, r.Header.Get("Accept-Encoding"))
		seenMu.Unlock()
		spec, ok := handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if spec.gzipBody {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(spec.status)
		_, _ = w.Write(spec.body)
	}))
	t.Cleanup(srv.Close)

	// An explicit Accept-Encoding keeps the transport from decoding the body
	// before RequestHttp sees Content-Encoding.
	header := make(http.Header)
	header.Set("Accept-Encoding", "gzip")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tr.count()
			res, err := RequestHttp(context.Background(), http.MethodGet, header, srv.URL+tc.path)
			if res != nil {
				t.Fatalf("response = %#v, want nil", res)
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if tr.count() != before+1 {
				t.Fatalf("upstream responses = %d, want %d", tr.count(), before+1)
			}
			body := tr.lastBody()
			if body == nil || !body.closed.Load() {
				t.Fatal("upstream body was not closed")
			}
			var status HttpStatusCodeError
			if !errors.As(err, &status) {
				t.Fatalf("errors.As(HttpStatusCodeError) = false, err = %v", err)
			}
			if int(status) != tc.status {
				t.Fatalf("status = %d, want %d", int(status), tc.status)
			}
			unwrapped, ok := errs.UnwrapOrSelf(err).(HttpStatusCodeError)
			if !ok || int(unwrapped) != tc.status {
				t.Fatalf("UnwrapOrSelf = %#v, want HttpStatusCodeError(%d)", errs.UnwrapOrSelf(err), tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantText)
			}
			seenMu.Lock()
			gotAE := ""
			if len(seenAE) > 0 {
				gotAE = seenAE[len(seenAE)-1]
			}
			seenMu.Unlock()
			if gotAE != "gzip" {
				t.Fatalf("upstream Accept-Encoding = %q, want gzip", gotAE)
			}
			if header.Get("Accept-Encoding") != "gzip" {
				t.Fatalf("caller Accept-Encoding changed to %q", header.Get("Accept-Encoding"))
			}
		})
	}
}

func TestRequestHttpSuccessBodyStaysCallerOwned(t *testing.T) {
	tr := useTrackingTransport(t)
	plain := []byte("preview-ok-你好")
	compressed := gzipBytes(t, plain)
	var seenMu sync.Mutex
	var seenAE []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seenAE = append(seenAE, r.Header.Get("Accept-Encoding"))
		seenMu.Unlock()
		switch r.URL.Path {
		case "/plain":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write(plain)
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write(compressed)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	header := make(http.Header)
	header.Set("Accept-Encoding", "gzip")

	t.Run("plain", func(t *testing.T) {
		assertCallerOwnedBody(t, tr, &seenMu, &seenAE, header, srv.URL+"/plain", plain)
	})
	t.Run("gzip", func(t *testing.T) {
		assertCallerOwnedBody(t, tr, &seenMu, &seenAE, header, srv.URL+"/gzip", compressed)
	})
}

func assertCallerOwnedBody(t *testing.T, tr *trackingTransport, seenMu *sync.Mutex, seenAE *[]string, header http.Header, url string, want []byte) {
	t.Helper()
	before := tr.count()
	res, err := RequestHttp(context.Background(), http.MethodGet, header, url)
	if err != nil {
		t.Fatalf("RequestHttp: %v", err)
	}
	if res == nil || res.Body == nil {
		t.Fatal("missing response body")
	}
	if tr.count() != before+1 {
		t.Fatalf("upstream responses = %d, want %d", tr.count(), before+1)
	}
	body := tr.lastBody()
	if body == nil {
		t.Fatal("missing tracked body")
	}
	if body.closed.Load() {
		t.Fatal("response body closed before the caller read it")
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body.closed.Load() {
		t.Fatal("reading the body closed it")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("caller Close did not close the upstream body")
	}
	seenMu.Lock()
	gotAE := ""
	if len(*seenAE) > 0 {
		gotAE = (*seenAE)[len(*seenAE)-1]
	}
	seenMu.Unlock()
	if gotAE != "gzip" {
		t.Fatalf("upstream Accept-Encoding = %q, want caller-supplied gzip", gotAE)
	}
	if header.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("caller Accept-Encoding changed to %q", header.Get("Accept-Encoding"))
	}
}
