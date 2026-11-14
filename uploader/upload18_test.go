package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpload18CodeExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"cd90ffd5ba95", "cd90ffd5ba95"},
		{"https://upload18.org/play/index/cd90ffd5ba95", "cd90ffd5ba95"},
		{"https://upload18.org/play/index/cd90ffd5ba95/", "cd90ffd5ba95"},
		{"https://upload18.org/play/index/cd90ffd5ba95?autoplay=1", "cd90ffd5ba95"},
		{"https://upload18.org/play/index/cd90ffd5ba95#t=10", "cd90ffd5ba95"},
		{"", ""},
		{"abc", ""}, // too short
	}

	for _, cse := range cases {
		if got := Upload18Code(cse.in); got != cse.want {
			t.Errorf("Upload18Code(%q) = %q, want %q", cse.in, got, cse.want)
		}
	}
}

func TestUpload18UploadSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload/server" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.FormValue("api_key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(upload18Response{
				Status:    "error",
				Msg:       "Unauthorized.",
				ErrorCode: "invalid_api_key",
			})
			return
		}
		if r.FormValue("cid") != "15" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(upload18Response{
				Status:    "error",
				Msg:       "CID is not correct.",
				ErrorCode: "invalid_cid",
			})
			return
		}
		file, _, err := r.FormFile("video")
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(upload18Response{
				Status:    "error",
				Msg:       "No video file was uploaded.",
				ErrorCode: "invalid_upload",
			})
			return
		}
		file.Close()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(upload18Response{
			Status: "success",
			Msg:    "Video upload complete",
			Vid:    "cd90ffd5ba95",
			Did:    12345,
			Fid:    18,
		})
	}))
	defer ts.Close()

	oldBase := upload18APIBase
	upload18APIBase = ts.URL
	defer func() { upload18APIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	if err := os.WriteFile(dummyVideo, []byte("fake video content"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := NewUpload18Uploader("test-key")
	u.client = ts.Client()

	link, err := u.Upload(dummyVideo)
	if err != nil {
		t.Fatalf("unexpected upload error: %v", err)
	}

	wantLink := "https://upload18.org/play/index/cd90ffd5ba95"
	if link != wantLink {
		t.Errorf("got link %q, want %q", link, wantLink)
	}
}

func TestUpload18KeyRotationOnAuthFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(10 << 20)
		key := r.FormValue("api_key")
		if key == "bad-key" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(upload18Response{
				Status:    "error",
				Msg:       "Unauthorized.",
				ErrorCode: "invalid_api_key",
			})
			return
		}
		json.NewEncoder(w).Encode(upload18Response{
			Status: "success",
			Msg:    "Video upload complete",
			Vid:    "rotated12345",
		})
	}))
	defer ts.Close()

	oldBase := upload18APIBase
	upload18APIBase = ts.URL
	defer func() { upload18APIBase = oldBase }()

	tmpDir := t.TempDir()
	dummyVideo := filepath.Join(tmpDir, "test.mp4")
	os.WriteFile(dummyVideo, []byte("fake video"), 0o644)

	u := NewUpload18Uploader("bad-key,good-key")
	u.client = ts.Client()

	link, err := u.Upload(dummyVideo)
	if err != nil {
		t.Fatalf("expected successful rotation, got err: %v", err)
	}
	if link != "https://upload18.org/play/index/rotated12345" {
		t.Errorf("got %q", link)
	}
}
