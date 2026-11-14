package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDroploadCodeExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"rz91qlhqpp74", "rz91qlhqpp74"},
		{"https://dropload.co/rz91qlhqpp74", "rz91qlhqpp74"},
		{"https://dropload.co/e/rz91qlhqpp74", "rz91qlhqpp74"},
		{"https://dropload.co/rz91qlhqpp74.html", "rz91qlhqpp74"},
		{"https://dr0pstream.com/rz91qlhqpp74", "rz91qlhqpp74"},
		{"https://dropload.co/rz91qlhqpp74?autoplay=1", "rz91qlhqpp74"},
		{"", ""},
		{"abc", ""}, // too short (< 4 chars)
	}

	for _, cse := range cases {
		if got := droploadCode(cse.in); got != cse.want {
			t.Errorf("droploadCode(%q) = %q, want %q", cse.in, got, cse.want)
		}
	}
}

func TestDroploadUploadSuccess(t *testing.T) {
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
			// Return self as upload server
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200,
				"msg":    "OK",
				"result": "http://" + r.Host + "/upload/target",
			})
		case "/upload/target":
			uploadServerCalled = true
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.FormValue("key") != "test-key" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"status": 401, "msg": "Invalid key"})
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
				"files": []map[string]string{
					{
						"filecode": "rz91qlhqpp74",
						"filename": "test.mp4",
						"status":   "OK",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	oldBase := droploadAPIBase
	droploadAPIBase = ts.URL
	defer func() { droploadAPIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	if err := os.WriteFile(dummyVideo, []byte("fake video content"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := NewDroploadUploader("test-key")
	u.client = ts.Client()

	link, err := u.Upload(dummyVideo)
	if err != nil {
		t.Fatalf("unexpected upload error: %v", err)
	}
	if !uploadServerCalled {
		t.Error("upload server endpoint was not called")
	}
	want := droploadViewBase + "/rz91qlhqpp74"
	if link != want {
		t.Errorf("got link %q, want %q", link, want)
	}
}
