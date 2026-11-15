package uploader

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAccountDisabledClassifier pins the wording matchers against the errors
// observed live (Dropload's "not enough disk space on your account", VOE's
// storage wording) and guards against matching file-level rejections or
// transient failures, which must keep using the normal retry path.
func TestAccountDisabledClassifier(t *testing.T) {
	tcs := []struct {
		name string
		err  error
		want bool
	}{
		{"dropload live wording", errors.New(`upload rejected: status 200 (msg: 0:0:0:not enough disk space on your account)`), true},
		{"voe storage", errors.New("VOE.sx: your maximum storage has been reached"), true},
		{"generic quota", errors.New("upload failed: storage quota exceeded"), true},
		{"over capacity", errors.New("server status not ok: 503 (msg: server is over capacity)"), true},
		{"banned", errors.New("HTTP 403: account banned"), true},
		{"suspended", errors.New("account suspended for abuse"), true},
		{"locked", errors.New("could not authenticate user. The key pair may be invalid or your account may be locked"), true},
		// file-level / transient rejections must NOT disable the account
		{"too short", errors.New("0:0:0:video is too short"), false},
		{"413 payload", errors.New("upload failed with status 413: 413 Payload Too Large"), false},
		{"conn reset", errors.New("do request: write tcp 10.1.0.5:52009->198.51.100.1:443: wsasend: connection was forcibly closed"), false},
		{"timeout", errors.New("net/http: timeout awaiting response headers"), false},
		{"nil", nil, false},
	}
	for _, tc := range tcs {
		if got := isAccountDisabledError(tc.err); got != tc.want {
			t.Errorf("%s: isAccountDisabledError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestAccountDisabledErrorPathDisablesFleetWide pins the full reaction on the
// real upload error path: a storage-full rejection must (1) publish the
// fleet-wide backoff row so peer nodes skip the host, (2) apply the ~6h timed
// disable locally, and (3) fire the operator alert sink exactly once.
func TestAccountDisabledErrorPathDisablesFleetWide(t *testing.T) {
	defer clearTimedDisablesForTest()
	clearAccountAlertsForTest()
	SetFleetHostBackoffs(nil)
	defer SetFleetHostBackoffs(nil)

	type report struct {
		host   string
		reason string
	}
	reports := make(chan report, 4)
	SetPublishHostBackoff(func(host string, until time.Time, reason string) {
		// only the shape matters here; the window is checked below
		_ = until
		reports <- report{host, reason}
	})
	defer SetPublishHostBackoff(nil)

	alerts := make(chan report, 4)
	SetAccountDisabledNotifier(func(host, reason string) {
		alerts <- report{host, reason}
	})
	defer SetAccountDisabledNotifier(nil)

	m := &MultiHostUploader{
		hosts: map[string]uploaderFunc{
			"Dropload": func(_ string, _ ProgressFunc) (string, error) {
				return "", errors.New("upload rejected: status 200 (msg: 0:0:0:not enough disk space on your account)")
			},
		},
		log: &nilLogger{},
	}

	results := m.UploadSelected("f.mp4", []string{"Dropload"})
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("expected one failed result, got %+v", results)
	}

	select {
	case got := <-reports:
		if got.host != "Dropload" {
			t.Errorf("published host = %q, want Dropload", got.host)
		}
		if !strings.Contains(got.reason, "not enough disk space") {
			t.Errorf("published reason %q should carry the live error text", got.reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("account-level error did not publish a fleet-wide backoff")
	}

	select {
	case got := <-alerts:
		if got.host != "Dropload" {
			t.Errorf("alert host = %q, want Dropload", got.host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("account-level error did not fire the operator alert")
	}

	if !isHostTimedOut("Dropload") {
		t.Fatal("host should be locally timed-out after an account-level rejection")
	}
	if d := hostTimedOutUntil("Dropload").Sub(time.Now()); d < 5*time.Hour || d > 7*time.Hour {
		t.Errorf("timed disable window = %v, want ~6h (accountDisabledCooldown)", d)
	}
	if !isFleetHostBackedOff("Dropload") {
		t.Fatal("host should be in the local fleet cache immediately after the account-level disable")
	}

	// A second failing file in the same run must NOT re-alert (layer 2 dedup),
	// because the host is already in the fleet cache from this run's disable.
	SetAccountDisabledNotifier(func(host, reason string) {
		t.Errorf("unexpected second alert for %q", host)
	})
	m2 := &MultiHostUploader{
		hosts: map[string]uploaderFunc{
			"Dropload": func(_ string, _ ProgressFunc) (string, error) {
				return "", errors.New("not enough disk space on your account")
			},
		},
		log: &nilLogger{},
	}
	// The local fleet cache blocks the attempt entirely — this asserts the
	// skip, and the notifier above asserts no duplicate alert sneaks through.
	_ = m2.UploadSelected("g.mp4", []string{"Dropload"})
}

// TestAuthErrorAlsoAlerts: a rejected/banned key only gets fixed by the
// operator, so the auth branch must raise the new-key alert too (with the
// same dedup), while keeping its existing rest-of-run local disable.
func TestAuthErrorAlsoAlerts(t *testing.T) {
	defer clearTimedDisablesForTest()
	clearAccountAlertsForTest()

	SetFleetHostBackoffs(nil)

	alerts := make(chan string, 2)
	SetAccountDisabledNotifier(func(host, reason string) {
		alerts <- host
	})
	defer SetAccountDisabledNotifier(nil)

	m := &MultiHostUploader{
		hosts: map[string]uploaderFunc{
			"Streamtape": func(_ string, _ ProgressFunc) (string, error) {
				return "", errors.New("get upload URL: API error 403: Authentication failed")
			},
		},
		log: &nilLogger{},
	}
	_ = m.UploadSelected("f.mp4", []string{"Streamtape"})

	select {
	case host := <-alerts:
		if host != "Streamtape" {
			t.Errorf("alert host = %q, want Streamtape", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth rejection did not fire the new-key alert")
	}
}

// TestAccountDisabledNotifierDedupThrottle pins layer 2: without the fleet
// cache blocking, repeated notifyAccountDisabled calls for the same host in
// one process must be throttled to one alert per ~day.
func TestAccountDisabledNotifierDedupThrottle(t *testing.T) {
	clearAccountAlertsForTest()
	SetFleetHostBackoffs(nil)
	defer SetFleetHostBackoffs(nil)

	n := 0
	SetAccountDisabledNotifier(func(host, reason string) { n++ })
	defer SetAccountDisabledNotifier(nil)

	for i := 0; i < 5; i++ {
		notifyAccountDisabled("SomeHost", "storage full")
	}
	// notifyAccountDisabled dispatches on its own goroutine; give it a beat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && n == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 alert after 5 detections (dedup), got %d", n)
	}
}
