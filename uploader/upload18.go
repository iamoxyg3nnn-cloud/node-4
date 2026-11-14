package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// upload18APIBase is a var so tests can stub it.
var upload18APIBase = "https://upload18.org/api"

// upload18CodeRe is an allowlist for plausible Upload18 video codes (alphanumeric).
var upload18CodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,}$`)

// upload18ViewBase is the public site base used to build play and embed links.
var upload18ViewBase = "https://upload18.org"

// defaultUpload18CID is the default Category ID: 15 (Videos).
const defaultUpload18CID = 15

// Upload18Uploader handles uploading video files to upload18.org.
type Upload18Uploader struct {
	keys   *keyRing
	client *http.Client
	cid    int
}

// NewUpload18Uploader creates a new Upload18 uploader instance. The API key
// may be a comma-separated list ("key1,key2,key3") to enable rotation when a
// key is invalidated or its storage quota is exhausted.
func NewUpload18Uploader(apiKey string) *Upload18Uploader {
	ring := buildRingFromEnv("UPLOAD18_KEY", apiKey)
	ring.host = "Upload18"

	cid := defaultUpload18CID
	if envCID := os.Getenv("UPLOAD18_CID"); envCID != "" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(envCID)); err == nil && parsed > 0 {
			cid = parsed
		}
	}

	return &Upload18Uploader{
		keys: ring,
		cid:  cid,
		client: &http.Client{
			Timeout: 120 * time.Minute, // Long timeout for large video uploads
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
				DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			},
		},
	}
}

type upload18Response struct {
	Status    string `json:"status"`
	Msg       string `json:"msg"`
	ErrorCode string `json:"error_code"`
	Vid       string `json:"vid"`
	Did       int64  `json:"did"`
	Fid       int    `json:"fid"`
	RequestID string `json:"request_id"`
}

// Upload uploads a file to Upload18 and returns the play link.
func (u *Upload18Uploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

// UploadWithProgress uploads a file to Upload18 and reports progress through fn.
func (u *Upload18Uploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("Upload18 API key not configured")
	}

	release, ok := acquireHostSem("Upload18")
	if !ok {
		return "", fmt.Errorf("upload18: upload slot busy — host saturated, skipped this attempt (deadline exceeded)")
	}
	defer release()

	var bodyFullySent bool
	wrapped := func(host string, current, total int64) {
		if total > 0 && current >= total {
			bodyFullySent = true
		}
		if progress != nil {
			progress(host, current, total)
		}
	}

	var lastErr error

	keyAttempts := u.keys.count()
	if keyAttempts < 1 {
		keyAttempts = 1
	}
	maxAttempts := 3 // per-key upload retries (backoff on rate-limit)

	for k := 0; k < keyAttempts; k++ {
		key := u.keys.current()

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt > 1 {
				time.Sleep(uploadBackoff(attempt-2, lastErr))
			}

			downloadLink, err := u.uploadFile(filePath, key, wrapped)
			if err != nil {
				lastErr = fmt.Errorf("upload file: %w", err)
				if isUpload18QuotaError(err) {
					u.keys.rotateReason("storage quota full")
					lastErr = nil
					bodyFullySent = false
					break
				}
				if isUpload18AuthError(err) {
					u.keys.rotateReason("authentication rejected")
					lastErr = nil
					bodyFullySent = false
					break
				}
				if bodyFullySent {
					return "", lastErr
				}
				if isUploadRateLimited(err) {
					time.Sleep(uploadBackoff(attempt, err))
					lastErr = nil
					continue
				}
				continue
			}

			return downloadLink, nil
		}

		if lastErr != nil && k < keyAttempts-1 {
			u.keys.rotate()
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("upload18: %w", lastErr)
	}
	return "", fmt.Errorf("upload18: all %d attempts failed", maxAttempts)
}

func (u *Upload18Uploader) uploadFile(filePath, apiKey string, progress ProgressFunc) (string, error) {
	fields := map[string]string{
		"api_key": apiKey,
		"cid":     strconv.Itoa(u.cid),
	}

	body, length, contentType, file, err := multipartStreamWithProgress(
		fields,
		"video",
		filePath,
		"Upload18",
		progress,
	)
	if err != nil {
		return "", fmt.Errorf("prepare multipart stream: %w", err)
	}
	defer file.Close()

	uploadURL := upload18APIBase + "/upload/server"
	req, err := http.NewRequest(http.MethodPost, uploadURL, body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("X-API-Key", apiKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post file: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var res upload18Response
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", fmt.Errorf("unmarshal response (HTTP %d): %s", resp.StatusCode, snippet(respBytes))
	}

	if res.Status != "success" || res.Vid == "" {
		if res.Msg != "" {
			return "", fmt.Errorf("Upload18: %s (error_code: %s)", res.Msg, res.ErrorCode)
		}
		return "", fmt.Errorf("Upload18: upload failed (HTTP %d): %s", resp.StatusCode, snippet(respBytes))
	}

	code := Upload18Code(res.Vid)
	if code == "" {
		return "", fmt.Errorf("Upload18: invalid vid returned %q", res.Vid)
	}

	return upload18ViewBase + "/play/index/" + code, nil
}

// Upload18Code extracts an Upload18 video code from a plain code, a play link
// ("https://upload18.org/play/index/CODE"), or an embed link.
func Upload18Code(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimRight(v, "/")
	if v == "" {
		return ""
	}
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	if !upload18CodeRe.MatchString(v) {
		return ""
	}
	return v
}

func isUpload18AuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_api_key") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "missing_api_key")
}

func isUpload18QuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "quota") ||
		strings.Contains(msg, "disk full") ||
		strings.Contains(msg, "limit exceeded") ||
		strings.Contains(msg, "storage full")
}
