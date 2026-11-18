package uploader

import (
	"errors"
	"strings"
	"testing"
)

// fullSendFakeHost returns a host upload func that reports the file at 100%
// through the progress callback and then fails, counting how many times the
// func is actually invoked.
func fullSendFakeHost(count *int, failWith error) uploaderFunc {
	return func(_ string, progress ProgressFunc) (string, error) {
		*count++
		if progress != nil {
			progress("TestHost", 1024, 1024)
		}
		return "", failWith
	}
}

// TestFullSendSkipsReUploadOnSubsequentAttempt: a host that fully transmitted
// the body but lost the response must NOT have the file re-streamed on the next
// UploadSelectedWithCallback call (stageUploadVideos re-runs it once per
// DoWithRetry attempt, reusing the same MultiHostUploader).
func TestFullSendSkipsReUploadOnSubsequentAttempt(t *testing.T) {
	var count int
	boom := errors.New("upload failed: unexpected response from host")
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": fullSendFakeHost(&count, boom),
		},
	}

	first := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(first) != 1 || first[0].Error == nil {
		t.Fatalf("first attempt should fail with a result")
	}
	if count != 1 {
		t.Fatalf("first attempt invoked host %d times, want 1", count)
	}
	if !u.hasFullSend("f.mp4", "TestHost") {
		t.Fatal("full-send marker not recorded after progress reached 100%")
	}

	// Simulate the next DoWithRetry attempt for the same file.
	second := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(second) != 1 || second[0].Error == nil {
		t.Fatalf("skipped attempt must still yield a failure result")
	}
	if count != 1 {
		t.Fatalf("re-upload must be skipped after full send; host invoked %d times, want 1", count)
	}
	if !strings.Contains(second[0].Error.Error(), "already fully transmitted") {
		t.Fatalf("skipped attempt error = %v, want errBodyFullySent wording", second[0].Error)
	}
}

// TestFullSendClearedOnSuccess: once the host succeeds, the full-send marker is
// cleared so a later (hypothetical) attempt is not skipped.
func TestFullSendClearedOnSuccess(t *testing.T) {
	var count int
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": func(_ string, progress ProgressFunc) (string, error) {
				count++
				if progress != nil {
					progress("TestHost", 1024, 1024)
				}
				return "https://example.test/video", nil
			},
		},
	}

	results := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(results) != 1 || results[0].Error != nil {
		t.Fatalf("expected success, got %#v", results)
	}
	if u.hasFullSend("f.mp4", "TestHost") {
		t.Fatal("full-send marker must be cleared on success")
	}
}

// TestFullSendMarkerIsScopedToItsFile is the regression guard for a full-send
// marker leaking across files: keyed by host alone, ONE lost response on ONE
// file banned that host for the rest of the process, skipping every later file
// with "already fully transmitted" even though those bytes were never sent —
// and since a skip reports a failure, the marker was never cleared.
func TestFullSendMarkerIsScopedToItsFile(t *testing.T) {
	var count int
	boom := errors.New("upload failed: unexpected response from host")
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": fullSendFakeHost(&count, boom),
		},
	}

	// File A: the body is fully transmitted, then the response is lost.
	if got := u.UploadSelected("a.mp4", []string{"TestHost"}); len(got) != 1 || got[0].Error == nil {
		t.Fatalf("first attempt on a.mp4 should fail, got %#v", got)
	}
	if count != 1 {
		t.Fatalf("a.mp4 invoked host %d times, want 1", count)
	}

	// File B must still be attempted: nothing of B was ever sent.  With the
	// host-keyed marker B was skipped without touching the socket, so count
	// stayed at 1 and the error wrongly read "already fully transmitted".
	got := u.UploadSelected("b.mp4", []string{"TestHost"})
	if len(got) != 1 || got[0].Error == nil {
		t.Fatalf("b.mp4 attempt should fail at the host, got %#v", got)
	}
	if count != 2 {
		t.Fatalf("b.mp4 must be attempted: host invoked %d times, want 2", count)
	}
	if strings.Contains(got[0].Error.Error(), "already fully transmitted") {
		t.Errorf("b.mp4 was skipped as already-transmitted; the full-send marker leaked from a.mp4: %v", got[0].Error)
	}
}
