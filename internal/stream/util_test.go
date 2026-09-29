package stream_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// nonASCIIJavaScript is valid UTF-8 whose byte offsets can split multibyte
// sequences. Downloads must preserve those bytes rather than transcode text.
func nonASCIIJavaScript() []byte {
	line := []byte("const label = \"下载 café — 日本語\";\nexport function greet(){return label;}\n")
	var buf bytes.Buffer
	for buf.Len() < 400 {
		buf.Write(line)
	}
	return buf.Bytes()
}

func mustGzip(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(src); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

type headerCapture struct {
	mu      sync.Mutex
	headers []http.Header
}

func (c *headerCapture) add(h http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, h.Clone())
}

func (c *headerCapture) reset() {
	c.mu.Lock()
	c.headers = nil
	c.mu.Unlock()
}

func (c *headerCapture) snapshot() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]http.Header, len(c.headers))
	copy(out, c.headers)
	return out
}

func writeOriginalRange(w http.ResponseWriter, r *http.Request, src []byte) {
	start, end := 0, len(src)
	status := http.StatusOK
	if rhs := r.Header.Get("Range"); rhs != "" {
		ranges, err := http_range.ParseRange(rhs, int64(len(src)))
		if err != nil || len(ranges) != 1 {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		ra := ranges[0]
		start = int(ra.Start)
		end = int(ra.Start + ra.Length)
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", ra.ContentRange(int64(len(src))))
	}
	body := src[start:end]
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// newJavaScriptServer returns compressed bytes when the client negotiates gzip
// and the original file when Accept-Encoding is identity.
func newJavaScriptServer(t *testing.T, alwaysRaw bool) (original []byte, srv *httptest.Server, cap *headerCapture) {
	t.Helper()
	original = nonASCIIJavaScript()
	compressed := mustGzip(t, original)
	cap = &headerCapture{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r.Header)
		if !alwaysRaw && r.Header.Get("Accept-Encoding") != "identity" {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(compressed)
			return
		}
		writeOriginalRange(w, r, original)
	}))
	t.Cleanup(srv.Close)
	return original, srv, cap
}

func browserHeader() http.Header {
	h := make(http.Header)
	h.Set("Accept-Encoding", "gzip, deflate, br")
	h.Set("Authorization", "Bearer client-secret")
	h.Set("Cookie", "client=session")
	h.Set("User-Agent", "Mozilla/5.0")
	h.Set("X-Trace", "browser-trace")
	return h
}

func driverHeader() http.Header {
	h := make(http.Header)
	h.Set("Accept-Encoding", "gzip")
	h.Set("Cookie", "quark=driver-cookie")
	h.Set("Authorization", "Bearer driver-token")
	h.Set("User-Agent", "quark-ua")
	h.Set("Referer", "https://pan.quark.cn/")
	return h
}

func getWithEncoding(t *testing.T, url, encoding string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept-Encoding", encoding)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res.StatusCode, res.Header.Clone(), body
}

func TestFixtureReturnsGzipUnlessIdentity(t *testing.T) {
	original, srv, _ := newJavaScriptServer(t, false)

	status, header, body := getWithEncoding(t, srv.URL, "identity")
	if status != http.StatusOK {
		t.Fatalf("identity status = %d, want %d", status, http.StatusOK)
	}
	if header.Get("Content-Encoding") != "" {
		t.Fatalf("identity Content-Encoding = %q", header.Get("Content-Encoding"))
	}
	if !bytes.Equal(body, original) {
		t.Fatalf("identity body length = %d, want %d", len(body), len(original))
	}

	status, header, body = getWithEncoding(t, srv.URL, "gzip, deflate, br")
	if status != http.StatusOK {
		t.Fatalf("gzip status = %d, want %d", status, http.StatusOK)
	}
	if header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q", header.Get("Content-Encoding"))
	}
	if bytes.Equal(body, original) {
		t.Fatal("gzip response matched the original bytes")
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatal("gzip payload is not the original javascript")
	}
}

func withEmptyConfig(t *testing.T) {
	t.Helper()
	previous := conf.Conf
	conf.Conf = &conf.Config{}
	t.Cleanup(func() { conf.Conf = previous })
}

func TestGetRangeReaderFromLinkPreservesOriginalBytes(t *testing.T) {
	withEmptyConfig(t)
	original, srv, cap := newJavaScriptServer(t, false)
	if len(original) <= 180 {
		t.Fatalf("fixture length = %d, want > 180", len(original))
	}
	size := len(original)
	ranges := []struct {
		name         string
		header       string
		want         []byte
		status       int
		contentRange string
	}{
		{
			name:   "full",
			want:   original,
			status: http.StatusOK,
		},
		{
			name:         "offset",
			header:       "bytes=50-179",
			want:         original[50:180],
			status:       http.StatusPartialContent,
			contentRange: "bytes 50-179/" + strconv.Itoa(size),
		},
		{
			name:         "suffix",
			header:       "bytes=-100",
			want:         original[size-100:],
			status:       http.StatusPartialContent,
			contentRange: "bytes " + strconv.Itoa(size-100) + "-" + strconv.Itoa(size-1) + "/" + strconv.Itoa(size),
		},
	}
	modes := []struct {
		name        string
		concurrency int
		partSize    int
		exactReqs   int
		minReqs     int
	}{
		{name: "direct", exactReqs: 1},
		{name: "quark-concurrency", concurrency: 3, partSize: 10 * utils.MB, exactReqs: 1},
		{name: "multiple-chunks", concurrency: 3, partSize: 64, minReqs: 2},
	}

	for _, mode := range modes {
		if mode.partSize > 0 && len(original) >= mode.partSize && mode.exactReqs == 1 {
			t.Fatalf("mode %s file length %d is not smaller than part size %d", mode.name, len(original), mode.partSize)
		}
		for _, rg := range ranges {
			t.Run(mode.name+"/"+rg.name, func(t *testing.T) {
				cap.reset()
				caller := browserHeader()
				driver := driverHeader()
				if rg.header != "" {
					caller.Set("Range", rg.header)
				}
				callerBefore := caller.Clone()
				driverBefore := driver.Clone()

				link := &model.Link{
					URL:         srv.URL,
					Header:      driver,
					Concurrency: mode.concurrency,
					PartSize:    mode.partSize,
				}
				req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
				req.Header = caller
				req = req.WithContext(context.WithValue(req.Context(), conf.RequestHeaderKey, caller))
				rr, err := stream.GetRangeReaderFromLink(int64(len(original)), link)
				if err != nil {
					t.Fatalf("GetRangeReaderFromLink: %v", err)
				}
				rec := httptest.NewRecorder()
				if err := net.ServeHTTP(rec, req, "app.js", time.Time{}, int64(len(original)), rr); err != nil {
					t.Fatalf("ServeHTTP: %v", err)
				}

				if rec.Code != rg.status {
					t.Fatalf("status = %d, want %d, body %q", rec.Code, rg.status, rec.Body.Bytes())
				}
				if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(rg.want)) {
					t.Fatalf("Content-Length = %q, want %d", got, len(rg.want))
				}
				if got := rec.Header().Get("Content-Range"); got != rg.contentRange {
					t.Fatalf("Content-Range = %q, want %q", got, rg.contentRange)
				}
				if !bytes.Equal(rec.Body.Bytes(), rg.want) {
					t.Fatalf("body mismatch: got %d bytes, want %d bytes", rec.Body.Len(), len(rg.want))
				}
				if !reflect.DeepEqual(caller, callerBefore) {
					t.Fatalf("caller headers changed: got %#v, want %#v", caller, callerBefore)
				}
				if !reflect.DeepEqual(driver, driverBefore) {
					t.Fatalf("driver headers changed: got %#v, want %#v", driver, driverBefore)
				}
				if got := driver.Get("Accept-Encoding"); got != "gzip" {
					t.Fatalf("driver Accept-Encoding = %q, want gzip", got)
				}

				reqs := cap.snapshot()
				if mode.exactReqs > 0 && len(reqs) != mode.exactReqs {
					t.Fatalf("upstream requests = %d, want %d", len(reqs), mode.exactReqs)
				}
				if len(reqs) < mode.minReqs {
					t.Fatalf("upstream requests = %d, want at least %d", len(reqs), mode.minReqs)
				}
				assertIdentityRequests(t, reqs)
			})
		}
	}
}

func TestGetRangeReaderFromLinkUncompressedBaseline(t *testing.T) {
	withEmptyConfig(t)
	original, srv, cap := newJavaScriptServer(t, true)
	size := len(original)
	cases := []struct {
		name         string
		header       string
		want         []byte
		status       int
		contentRange string
	}{
		{name: "full", want: original, status: http.StatusOK},
		{
			name:         "offset",
			header:       "bytes=50-179",
			want:         original[50:180],
			status:       http.StatusPartialContent,
			contentRange: "bytes 50-179/" + strconv.Itoa(size),
		},
	}
	for _, rg := range cases {
		t.Run(rg.name, func(t *testing.T) {
			cap.reset()
			caller := browserHeader()
			driver := driverHeader()
			if rg.header != "" {
				caller.Set("Range", rg.header)
			}
			callerBefore := caller.Clone()
			driverBefore := driver.Clone()
			link := &model.Link{URL: srv.URL, Header: driver}
			req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
			req.Header = caller
			req = req.WithContext(context.WithValue(req.Context(), conf.RequestHeaderKey, caller))
			rr, err := stream.GetRangeReaderFromLink(int64(len(original)), link)
			if err != nil {
				t.Fatalf("GetRangeReaderFromLink: %v", err)
			}
			rec := httptest.NewRecorder()
			if err := net.ServeHTTP(rec, req, "app.js", time.Time{}, int64(len(original)), rr); err != nil {
				t.Fatalf("ServeHTTP: %v", err)
			}
			if rec.Code != rg.status {
				t.Fatalf("status = %d, want %d", rec.Code, rg.status)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(rg.want)) {
				t.Fatalf("Content-Length = %q, want %d", got, len(rg.want))
			}
			if got := rec.Header().Get("Content-Range"); got != rg.contentRange {
				t.Fatalf("Content-Range = %q, want %q", got, rg.contentRange)
			}
			if !bytes.Equal(rec.Body.Bytes(), rg.want) {
				t.Fatal("uncompressed body mismatch")
			}
			if !reflect.DeepEqual(caller, callerBefore) || !reflect.DeepEqual(driver, driverBefore) {
				t.Fatal("caller or driver headers changed")
			}
			assertIdentityRequests(t, cap.snapshot())
		})
	}
}

func assertIdentityRequests(t *testing.T, reqs []http.Header) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("upstream received no requests")
	}
	for i, h := range reqs {
		if got := h.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("request %d Accept-Encoding = %q, want identity", i, got)
		}
		if got := h.Get("Cookie"); got != "quark=driver-cookie" {
			t.Errorf("request %d Cookie = %q, want driver cookie", i, got)
		}
		if got := h.Get("Authorization"); got != "Bearer driver-token" {
			t.Errorf("request %d Authorization = %q, want driver token", i, got)
		}
		if got := h.Get("User-Agent"); got != "quark-ua" {
			t.Errorf("request %d User-Agent = %q, want quark-ua", i, got)
		}
		if got := h.Get("Referer"); got != "https://pan.quark.cn/" {
			t.Errorf("request %d Referer = %q", i, got)
		}
		if got := h.Get("X-Trace"); got != "browser-trace" {
			t.Errorf("request %d X-Trace = %q, want browser-trace", i, got)
		}
	}
}
