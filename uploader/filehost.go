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

// fileHostAPIBase is a var so tests can point it at a fake server.
var fileHostAPIBase = "https://filehost.net/api"

// fileHostCodeRe is an allowlist for plausible FileHost file codes.
var fileHostCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,}$`)

// fileHostViewBase is the public site base used to build canonical view links.
var fileHostViewBase = "https://filehost.net"

// FileHostUploader handles uploading files to filehost.net.
type FileHostUploader struct {
	keys   *keyRing
	client *http.Client
}

// NewFileHostUploader creates a new FileHost uploader instance. The API key
// may be a comma-separated list ("key1,key2,key3") to enable rotation when a
// key is invalidated or its storage quota is exhausted.
func NewFileHostUploader(apiKey string) *FileHostUploader {
	ring := buildRingFromEnv("FILEHOST_KEY", apiKey)
	if ring.count() == 0 {
		ring = buildRingFromEnv("FILEHOST_API_KEY", apiKey)
	}
	ring.host = "FileHost"
	return &FileHostUploader{
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

type fileHostServerResponse struct {
	Msg    string          `json:"msg"`
	Status int             `json:"status"`
	Result json.RawMessage `json:"result"`
}

func (r fileHostServerResponse) uploadServer() string {
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

type fileHostFile struct {
	Filecode    string `json:"filecode"`
	DownloadURL string `json:"download_url"`
	Title       string `json:"title"`
	Status      any    `json:"status"`
	Msg         string `json:"msg"`
}

type fileHostUploadResponse struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Result json.RawMessage `json:"result"`
}

func (r fileHostUploadResponse) fileCode() string {
	if len(r.Result) > 0 {
		var arr []fileHostFile
		if json.Unmarshal(r.Result, &arr) == nil {
			for _, f := range arr {
				if c := fileHostCode(f.Filecode); c != "" {
					return c
				}
				if c := fileHostCode(f.DownloadURL); c != "" {
					return c
				}
			}
		}
		var one fileHostFile
		if json.Unmarshal(r.Result, &one) == nil {
			if c := fileHostCode(one.Filecode); c != "" {
				return c
			}
			if c := fileHostCode(one.DownloadURL); c != "" {
				return c
			}
		}
		var s string
		if json.Unmarshal(r.Result, &s) == nil {
			return fileHostCode(s)
		}
	}
	return ""
}

func fileHostCode(v string) string {
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
	if !fileHostCodeRe.MatchString(v) {
		return ""
	}
	return v
}

func (u *FileHostUploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

func (u *FileHostUploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("FileHost API key not configured")
	}

	release, ok := acquireHostSem("FileHost")
	if !ok {
		return "", fmt.Errorf("filehost: upload slot busy — host saturated, skipped this attempt (deadline exceeded)")
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
				if isFileHostQuotaError(err) {
					bodyFullySent = false
					u.keys.rotateReason("storage quota full")
					lastErr = nil
					break
				}
				if isFileHostAuthError(err) {
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
	return "", fmt.Errorf("all FileHost attempts failed")
}

func isFileHostAuthError(err error) bool {
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

func isFileHostQuotaError(err error) bool {
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

func (u *FileHostUploader) getUploadServer(key string) (string, error) {
	req, err := http.NewRequest("GET", fileHostAPIBase+"/upload/server?key="+url.QueryEscape(key), nil)
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

	var serverResp fileHostServerResponse
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

func (u *FileHostUploader) uploadFile(filePath, key string, progress ProgressFunc) (string, error) {
	uploadServer, err := u.getUploadServer(key)
	if err != nil {
		return "", fmt.Errorf("get upload server: %w", err)
	}

	body, contentLen, contentType, file, err := multipartStreamWithProgress(
		map[string]string{"key": key, "api_key": key},
		"file", filePath, "FileHost", progress,
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

	var uploadResp fileHostUploadResponse
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

	// The site's canonical view URL is /v/<code> (the bare /<code> form 404s
	// since FileHost changed their routing — verified live 2026-11-15).
	return fileHostViewBase + "/v/" + code, nil
}
