package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// droploadAPIBase is a var so tests can point it at a fake server.
var droploadAPIBase = "https://dropload.co/api"

// droploadCodeRe is an allowlist for plausible Dropload file codes.
var droploadCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,}$`)

// droploadViewBase is the public site base used to build canonical view links.
var droploadViewBase = "https://dropload.co"

// DroploadUploader handles uploading files to dropload.co.
type DroploadUploader struct {
	keys   *keyRing
	client *http.Client
}

// NewDroploadUploader creates a new Dropload uploader instance. The API key
// may be a comma-separated list ("key1,key2,key3") to enable rotation when a
// key is invalidated or its storage quota is exhausted.
func NewDroploadUploader(apiKey string) *DroploadUploader {
	ring := buildRingFromEnv("DROPLOAD_KEY", apiKey)
	if ring.count() == 0 {
		ring = buildRingFromEnv("DROPLOAD_API_KEY", apiKey)
	}
	ring.host = "Dropload"
	return &DroploadUploader{
		keys: ring,
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

type droploadServerResponse struct {
	Msg    string          `json:"msg"`
	Status int             `json:"status"`
	Result json.RawMessage `json:"result"`
}

func (r droploadServerResponse) uploadServer() string {
	var s string
	if json.Unmarshal(r.Result, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		UploadServer string `json:"upload_server"`
		URL          string `json:"url"`
	}
	if json.Unmarshal(r.Result, &obj) == nil {
		if v := strings.TrimSpace(obj.UploadServer); v != "" {
			return v
		}
		return strings.TrimSpace(obj.URL)
	}
	return ""
}

type droploadFile struct {
	Filecode string `json:"filecode"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Msg      string `json:"msg"`
	URL      string `json:"url"`
}

type droploadUploadResponse struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Files  []droploadFile  `json:"files"`
	Result json.RawMessage `json:"result"`
}

func (r droploadUploadResponse) fileCode() string {
	for _, f := range r.Files {
		if c := droploadCode(f.Filecode); c != "" {
			return c
		}
		if c := droploadCode(f.URL); c != "" {
			return c
		}
	}
	if len(r.Result) > 0 {
		var arr []droploadFile
		if json.Unmarshal(r.Result, &arr) == nil {
			for _, f := range arr {
				if c := droploadCode(f.Filecode); c != "" {
					return c
				}
				if c := droploadCode(f.URL); c != "" {
					return c
				}
			}
		}
		var one droploadFile
		if json.Unmarshal(r.Result, &one) == nil {
			if c := droploadCode(one.Filecode); c != "" {
				return c
			}
			if c := droploadCode(one.URL); c != "" {
				return c
			}
		}
		var s string
		if json.Unmarshal(r.Result, &s) == nil {
			return droploadCode(s)
		}
	}
	return ""
}

func droploadCode(v string) string {
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
	v = strings.TrimSuffix(v, ".html")
	if !droploadCodeRe.MatchString(v) {
		return ""
	}
	return v
}

func (u *DroploadUploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

func (u *DroploadUploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("Dropload API key not configured")
	}

	release, ok := acquireHostSem("Dropload")
	if !ok {
		return "", fmt.Errorf("dropload: upload slot busy — host saturated, skipped this attempt (deadline exceeded)")
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
	maxAttempts := 3

	for k := 0; k < keyAttempts; k++ {
		key := u.keys.current()

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt > 1 {
				time.Sleep(uploadBackoff(attempt-2, lastErr))
			}

			downloadLink, err := u.uploadFile(filePath, key, wrapped)
			if err != nil {
				lastErr = fmt.Errorf("upload file: %w", err)
				if isDroploadQuotaError(err) {
					bodyFullySent = false
					u.keys.rotateReason("storage quota full")
					lastErr = nil
					break
				}
				if isDroploadAuthError(err) {
					bodyFullySent = false
					u.keys.rotateReason("key rejected (auth failed)")
					lastErr = nil
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
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("all Dropload attempts failed")
}

func isDroploadAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid key") ||
		strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "invalid_api_key") ||
		strings.Contains(msg, "api key is invalid") ||
		strings.Contains(msg, "key is invalid") ||
		strings.Contains(msg, "wrong api key") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "not allowed to upload")
}

func isDroploadQuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "storage") ||
		strings.Contains(msg, "disk") ||
		strings.Contains(msg, "quota") ||
		strings.Contains(msg, "not enough space") ||
		strings.Contains(msg, "no space left") ||
		strings.Contains(msg, "upload limit") ||
		strings.Contains(msg, "limit reached") ||
		strings.Contains(msg, "maximum upload") ||
		strings.Contains(msg, "over capacity")
}

func (u *DroploadUploader) getUploadServer(key string) (string, error) {
	req, err := http.NewRequest("GET", droploadAPIBase+"/upload/server?key="+url.QueryEscape(key), nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request upload server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", fmt.Errorf("get upload server failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var serverResp droploadServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("decode server response: %w", err)
	}

	server := serverResp.uploadServer()
	if serverResp.Status != 200 || server == "" {
		msg := strings.TrimSpace(serverResp.Msg)
		if msg == "" {
			msg = "empty upload server"
		}
		return "", fmt.Errorf("server status not ok: %d (msg: %s)", serverResp.Status, msg)
	}

	return server, nil
}

func droploadRejectionDetail(r droploadUploadResponse) string {
	for _, f := range r.Files {
		if s := strings.TrimSpace(f.Status); s != "" && !strings.EqualFold(s, "OK") {
			return s
		}
		if s := strings.TrimSpace(f.Msg); s != "" && !strings.EqualFold(s, "OK") {
			return s
		}
	}
	if msg := strings.TrimSpace(r.Msg); msg != "" {
		return msg
	}
	return "no file code in response"
}

func (u *DroploadUploader) uploadFile(filePath, key string, progress ProgressFunc) (string, error) {
	uploadServer, err := u.getUploadServer(key)
	if err != nil {
		return "", fmt.Errorf("get upload server: %w", err)
	}

	body, contentLen, contentType, file, err := multipartStreamWithProgress(
		map[string]string{"key": key},
		"file", filePath, "Dropload", progress,
	)
	if err != nil {
		return "", fmt.Errorf("multipart stream: %w", err)
	}
	defer file.Close()

	req, err := http.NewRequest("POST", uploadServer, body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Referer", droploadViewBase+"/")
	req.ContentLength = contentLen

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var uploadResp droploadUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	code := uploadResp.fileCode()
	if code == "" {
		return "", fmt.Errorf("upload rejected: status %d (msg: %s)", uploadResp.Status, droploadRejectionDetail(uploadResp))
	}
	if uploadResp.Status != 200 {
		msg := strings.TrimSpace(uploadResp.Msg)
		if msg == "" {
			msg = "unexpected status"
		}
		return "", fmt.Errorf("upload failed: status %d (msg: %s)", uploadResp.Status, msg)
	}

	return droploadViewBase + "/" + code, nil
}
