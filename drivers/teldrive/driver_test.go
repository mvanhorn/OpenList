package teldrive

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/go-resty/resty/v2"
)

// An empty directory makes the Teldrive API report totalPages == 0. List used
// to size pagesData by that value and then write pagesData[0], which panicked
// with "index out of range [0] with length 0" on every empty folder.
func TestListEmptyDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"meta":{"count":0,"totalPages":0,"currentPage":1}}`))
	}))
	defer srv.Close()

	oldClient := base.RestyClient
	base.RestyClient = resty.New()
	defer func() { base.RestyClient = oldClient }()

	d := &Teldrive{}
	d.Address = srv.URL

	objs, err := d.List(context.Background(), &model.Object{Path: "/"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("expected no entries for an empty dir, got %d", len(objs))
	}
}

func TestPutPreservesModTime(t *testing.T) {
	modTime := time.Date(2001, time.February, 3, 4, 5, 6, 7000000, time.FixedZone("UTC-7", -7*60*60))
	tests := []struct {
		name        string
		contents    []byte
		modTime     time.Time
		wantModTime bool
	}{
		{
			name:        "uploaded file",
			contents:    []byte("contents"),
			modTime:     modTime,
			wantModTime: true,
		},
		{
			name:        "empty file",
			modTime:     modTime,
			wantModTime: true,
		},
		{
			name:     "zero modification time",
			contents: []byte("contents"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var createBody map[string]interface{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/files":
					_, _ = w.Write([]byte(`{"items":[]}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/uploads/fileId":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/uploads/"):
					_, _ = w.Write([]byte(`{"name":"part","partId":1,"partNo":1,"salt":"salt"}`))
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/uploads/"):
					_, _ = w.Write([]byte(`[{"name":"part","partId":1,"partNo":1,"salt":"salt"}]`))
				case r.Method == http.MethodPost && r.URL.Path == "/api/files":
					if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/uploads/"):
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer srv.Close()

			oldClient := base.RestyClient
			base.RestyClient = resty.New()
			defer func() { base.RestyClient = oldClient }()

			d := &Teldrive{}
			d.Address = srv.URL
			d.Cookie = "access_token=test"
			d.ChunkSize = 1
			file := &stream.FileStream{
				Ctx:    context.Background(),
				Obj:    &model.Object{Name: "source.txt", Size: int64(len(tt.contents)), Modified: tt.modTime},
				Reader: bytes.NewReader(tt.contents),
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Errorf("close file stream: %v", err)
				}
			}()

			if err := d.Put(context.Background(), &model.Object{Path: "/destination"}, file, func(float64) {}); err != nil {
				t.Fatalf("Put returned error: %v", err)
			}
			if createBody == nil {
				t.Fatal("expected a create-file request")
			}
			if got := createBody["name"]; got != "source.txt" {
				t.Errorf("name = %v, want source.txt", got)
			}
			if got := createBody["type"]; got != "file" {
				t.Errorf("type = %v, want file", got)
			}
			if got := createBody["path"]; got != "/destination" {
				t.Errorf("path = %v, want /destination", got)
			}

			gotModTime, hasModTime := createBody["updatedAt"]
			if tt.wantModTime {
				if !hasModTime {
					t.Fatal("create request omitted updatedAt")
				}
				if gotModTime != tt.modTime.Format(time.RFC3339Nano) {
					t.Errorf("updatedAt = %v, want %s", gotModTime, tt.modTime.Format(time.RFC3339Nano))
				}
			} else if hasModTime {
				t.Errorf("create request updatedAt = %v, want field omitted", gotModTime)
			}

			if len(tt.contents) > 0 {
				if got := createBody["size"]; got != float64(len(tt.contents)) {
					t.Errorf("size = %v, want %d", got, len(tt.contents))
				}
				parts, ok := createBody["parts"].([]interface{})
				if !ok || len(parts) != 1 {
					t.Fatalf("parts = %#v, want one part", createBody["parts"])
				}
				part, ok := parts[0].(map[string]interface{})
				if !ok || part["id"] != float64(1) || part["salt"] != "salt" {
					t.Errorf("part = %#v, want id 1 and salt salt", parts[0])
				}
			} else {
				if _, ok := createBody["size"]; ok {
					t.Errorf("empty-file create request size = %v, want field omitted", createBody["size"])
				}
				if _, ok := createBody["parts"]; ok {
					t.Errorf("empty-file create request parts = %v, want field omitted", createBody["parts"])
				}
			}
		})
	}
}
