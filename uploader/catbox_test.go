package uploader

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestIsRetryableCatboxError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"transient 412 not signed in (observed burst)", "catbox: status 412: Not signed in!", true},
		{"transient 412 invalid uploader (IP block)", "catbox: status 412: Invalid uploader", true},
		{"rate limit 429", "catbox: status 429: too many requests", true},
		{"server error 5xx", "catbox: status 520: something went wrong", true},
		{"network send failure", "catbox: send request: connection reset by peer", true},
		{"stat race", "catbox: stat file: no such file or directory", true},
		{"read failure", "catbox: read response: unexpected EOF", true},
		{"fatal 400 bad request", "catbox: status 400: invalid request", false},
		{"fatal unexpected response", "catbox: unexpected response: not a url", false},
		{"fatal empty response", "catbox: empty response", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableCatboxError(&fakeErr{tc.err}); got != tc.want {
				t.Errorf("isRetryableCatboxError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCatboxProxyFallbackOn412(t *testing.T) {
	var proxyHits int32
	var directHits int32

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		w.WriteHeader(http.StatusPreconditionFailed)
		w.Write([]byte("Invalid uploader"))
	}))
	defer proxyServer.Close()

	directServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&directHits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("https://files.catbox.moe/fallback_success.webp"))
	}))
	defer directServer.Close()

	tmpDir := t.TempDir()
	dummyFile := filepath.Join(tmpDir, "test.webp")
	if err := os.WriteFile(dummyFile, []byte("fake-image-bytes"), 0644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	u := &CatboxUploader{
		client:   proxyServer.Client(),
		proxyURL: proxyServer.URL,
		userHash: "somehash",
	}

	// First attempt will hit proxyServer -> get 412 -> clear proxyURL.
	// Since uploadURL() would normally return https://catbox.moe, we test that proxyURL is cleared
	// after 412.
	_, err := u.Upload(dummyFile)
	if u.proxyURL != "" {
		t.Errorf("expected proxyURL to be cleared on 412, got %q", u.proxyURL)
	}
	if atomic.LoadInt32(&proxyHits) == 0 {
		t.Errorf("expected proxy to be hit at least once, got %d", proxyHits)
	}
	_ = err
}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
