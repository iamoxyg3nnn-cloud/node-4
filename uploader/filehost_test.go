package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileHostCodeExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc1234", "abc1234"},
		{"https://filehost.net/abc1234", "abc1234"},
		{"https://filehost.net/v/abc1234", "abc1234"},
		{"https://filehost.net/e/abc1234", "abc1234"},
		{"https://filehost.net/abc1234?autoplay=1", "abc1234"},
		{"", ""},
		{"abc", ""}, // too short
	}

	for _, cse := range cases {
		if got := fileHostCode(cse.in); got != cse.want {
			t.Errorf("fileHostCode(%q) = %q, want %q", cse.in, got, cse.want)
		}
	}
}

func TestFileHostUploadSuccess(t *testing.T) {
	var uploadServerCalled bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/server":
			if r.URL.Query().Get("key") != "test-key" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"status": 401, "msg": "Invalid API key"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200,
				"msg":    "OK",
				"result": "http://" + r.Host + "/upload/target?key=test-key",
			})
		case "/upload/target":
			uploadServerCalled = true
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"status": 400, "msg": "No file uploaded"})
				return
			}
			file.Close()

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200,
				"msg":    "OK",
				"result": []map[string]any{
					{
						"filecode":     "abc1234",
						"download_url": "https://filehost.net/abc1234",
						"status":       200,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	oldBase := fileHostAPIBase
	fileHostAPIBase = ts.URL
	defer func() { fileHostAPIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	if err := os.WriteFile(dummyVideo, []byte("fake video content"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := NewFileHostUploader("test-key")
	u.client = ts.Client()

	link, err := u.Upload(dummyVideo)
	if err != nil {
		t.Fatalf("unexpected upload error: %v", err)
	}
	if !uploadServerCalled {
		t.Error("upload server endpoint was not called")
	}
	want := fileHostViewBase + "/v/abc1234"
	if link != want {
		t.Errorf("got link %q, want %q", link, want)
	}
}

func TestFileHostCloudflareSizeLimitGuard(t *testing.T) {
	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "huge.mp4")
	f, err := os.Create(dummyVideo)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to 105MB without writing bytes to disk
	if err := f.Truncate(105 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	u := NewFileHostUploader("test-key")
	_, err = u.Upload(dummyVideo)
	if err == nil {
		t.Fatal("expected size guard error for 105MB file, got nil")
	}
	if !isFileHostSizeLimitError(err) {
		t.Errorf("expected isFileHostSizeLimitError to match %v", err)
	}
}

func TestFileHostNonRetryable413(t *testing.T) {
	var targetCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/server":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200,
				"msg":    "OK",
				"result": "http://" + r.Host + "/upload/target",
			})
		case "/upload/target":
			targetCalls++
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte("<html><title>413 Request Entity Too Large</title></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	oldBase := fileHostAPIBase
	fileHostAPIBase = ts.URL
	defer func() { fileHostAPIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	os.WriteFile(dummyVideo, []byte("fake video"), 0o644)

	u := NewFileHostUploader("test-key")
	u.client = ts.Client()

	_, err := u.Upload(dummyVideo)
	if err == nil {
		t.Fatal("expected 413 error, got nil")
	}
	if targetCalls != 1 {
		t.Errorf("expected exactly 1 call before failing fast on 413, got %d calls", targetCalls)
	}
}
