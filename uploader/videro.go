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

// videroAPIBase is a var so tests can point it at a fake server.
var videroAPIBase = "https://videro.my/api/v1"

// videroCodeRe is an allowlist for plausible Videro file codes.
var videroCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,}$`)

// videroViewBase is the public site base used to build canonical view links.
var videroViewBase = "https://videro.my/d"

// VideroUploader handles uploading files to videro.my.
type VideroUploader struct {
	keys   *keyRing
	client *http.Client
}

// NewVideroUploader creates a new Videro uploader instance. The API key
// may be a comma-separated list ("key1,key2,key3") to enable rotation when a
// key is invalidated or its storage quota is exhausted.
func NewVideroUploader(apiKey string) *VideroUploader {
	ring := buildRingFromEnv("VIDERO_KEY", apiKey)
	if ring.count() == 0 {
		ring = buildRingFromEnv("VIDERO_API_KEY", apiKey)
	}
	ring.host = "Videro"
	return &VideroUploader{
		keys: ring,
		client: &http.Client{
			Timeout: 120 * time.Minute,
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

type videroServerResponse struct {
	Msg    string          `json:"msg"`
	Status int             `json:"status"`
	Result json.RawMessage `json:"result"`
}

func (r videroServerResponse) uploadServer() string {
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

type videroFile struct {
	Filecode    string `json:"filecode"`
	DownloadURL string `json:"download_url"`
	Title       string `json:"title"`
	Status      any    `json:"status"`
	Msg         string `json:"msg"`
}

type videroUploadResponse struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Result json.RawMessage `json:"result"`
}

func (r videroUploadResponse) fileCode() string {
	if len(r.Result) > 0 {
		var arr []videroFile
		if json.Unmarshal(r.Result, &arr) == nil {
			for _, f := range arr {
				if c := videroCode(f.Filecode); c != "" {
					return c
				}
				if c := videroCode(f.DownloadURL); c != "" {
					return c
				}
			}
		}
		var one videroFile
		if json.Unmarshal(r.Result, &one) == nil {
			if c := videroCode(one.Filecode); c != "" {
				return c
			}
			if c := videroCode(one.DownloadURL); c != "" {
				return c
			}
		}
		var s string
		if json.Unmarshal(r.Result, &s) == nil {
			return videroCode(s)
		}
	}
	return ""
}

func videroCode(v string) string {
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
	if !videroCodeRe.MatchString(v) {
		return ""
	}
	return v
}

func (u *VideroUploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

func (u *VideroUploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("Videro API key not configured")
	}

	release, ok := acquireHostSem("Videro")
	if !ok {
		return "", fmt.Errorf("videro: upload slot busy — host saturated, skipped this attempt (deadline exceeded)")
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
				if isVideroQuotaError(err) {
					bodyFullySent = false
					u.keys.rotateReason("storage quota full")
					lastErr = nil
					break
				}
				if isVideroAuthError(err) {
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
	return "", fmt.Errorf("all Videro attempts failed")
}

func isVideroAuthError(err error) bool {
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
		strings.Contains(msg, "unauthorized")
}

func isVideroQuotaError(err error) bool {
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
		strings.Contains(msg, "over capacity")
}

func (u *VideroUploader) getUploadServer(key string) (string, error) {
	req, err := http.NewRequest("GET", videroAPIBase+"/upload/server?key="+url.QueryEscape(key), nil)
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

	var serverResp videroServerResponse
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

func (u *VideroUploader) uploadFile(filePath, key string, progress ProgressFunc) (string, error) {
	uploadServer, err := u.getUploadServer(key)
	if err != nil {
		return "", fmt.Errorf("get upload server: %w", err)
	}

	body, contentLen, contentType, file, err := multipartStreamWithProgress(
		nil,
		"file", filePath, "Videro", progress,
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

	var uploadResp videroUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	code := uploadResp.fileCode()
	if code == "" {
		msg := strings.TrimSpace(uploadResp.Msg)
		if msg == "" {
			msg = "no file code in response"
		}
		return "", fmt.Errorf("upload rejected: status %d (msg: %s)", uploadResp.Status, msg)
	}
	if uploadResp.Status != 200 {
		msg := strings.TrimSpace(uploadResp.Msg)
		if msg == "" {
			msg = "unexpected status"
		}
		return "", fmt.Errorf("upload failed: status %d (msg: %s)", uploadResp.Status, msg)
	}

	return videroViewBase + "/" + code, nil
}
