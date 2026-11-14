package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVideroCodeExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"QUyserM28kXJ", "QUyserM28kXJ"},
		{"https://videro.my/d/QUyserM28kXJ", "QUyserM28kXJ"},
		{"https://videro.my/e/QUyserM28kXJ", "QUyserM28kXJ"},
		{"https://videro.my/d/QUyserM28kXJ?autoplay=1", "QUyserM28kXJ"},
		{"", ""},
		{"abc", ""}, // too short
	}

	for _, cse := range cases {
		if got := videroCode(cse.in); got != cse.want {
			t.Errorf("videroCode(%q) = %q, want %q", cse.in, got, cse.want)
		}
	}
}

func TestVideroUploadSuccess(t *testing.T) {
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
						"filecode":     "QUyserM28kXJ",
						"download_url": "https://videro.my/d/QUyserM28kXJ",
						"status":       200,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	oldBase := videroAPIBase
	videroAPIBase = ts.URL
	defer func() { videroAPIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	if err := os.WriteFile(dummyVideo, []byte("fake video content"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := NewVideroUploader("test-key")
	u.client = ts.Client()

	link, err := u.Upload(dummyVideo)
	if err != nil {
		t.Fatalf("unexpected upload error: %v", err)
	}
	if !uploadServerCalled {
		t.Error("upload server endpoint was not called")
	}
	want := videroViewBase + "/QUyserM28kXJ"
	if link != want {
		t.Errorf("got link %q, want %q", link, want)
	}
}
