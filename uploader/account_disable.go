package uploader

import (
	"strings"
	"sync"
	"time"
)

// accountDisabledCooldown is how long a host stays skipped after an
// ACCOUNT-level rejection: its storage is full, the account got banned, or the
// key was revoked.  Unlike a per-day quota, no amount of retrying within this
// window can succeed — only the operator supplying a new key fixes it.  Six
// hours matches keysExhaustedCooldown's philosophy: stop the churn, but let a
// key rotation restore the host within the same day.  A new key takes effect
// on the next session restart (fresh process, empty timed-disable map), and
// the fleet-wide row lifts itself after the window.
const accountDisabledCooldown = 6 * time.Hour

// isAccountDisabledError reports whether an upload error means the ACCOUNT
// behind the shared key rejected us in a way only a new key (or a manual
// cleanup on the host's site) can fix:
//
//   - storage full / over quota:  Dropload's live wording is "not enough disk
//     space on your account"; VOE says "storage space"/"maximum storage";
//     general hosts say "storage quota exceeded".  (The isVoeStorageFull
//     lesson: per-host matchers kept missing new wording, so this classifier
//     is deliberately broad.)
//   - banned / suspended / terminated account.
//   - locked account ("account may be locked" is VOE's auth+lock wording).
//
// It must NOT match file-level rejections ("video is too short", 413 payload
// limits) or transient network failures — those are per-upload problems.
func isAccountDisabledError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		// storage exhaustion (any phrasing we have observed live)
		"not enough disk space",
		"storage full",
		"storage space",
		"maximum storage",
		"storage quota",
		"quota exceeded",
		"insufficient storage",
		"over capacity",
		// account-level bans / locks
		"account banned",
		"account suspended",
		"account terminated",
		"account has been suspended",
		"account is locked",
		"account may be locked",
		"account is disabled",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// accountDisabledNotifyFn, when set, receives every account-level disable so
// the operator can be alerted (Discord webhook in production).  Installed by
// the manager at startup; nil in tests that don't need alerts.
var accountDisabledNotifyFn func(host, reason string)

// SetAccountDisabledNotifier installs the alert sink for account-level host
// disables (storage full / banned / suspended).  Passing nil disables alerts.
func SetAccountDisabledNotifier(fn func(host, reason string)) {
	accountDisabledNotifyFn = fn
}

// lastAccountAlert remembers the last time each host raised an account-level
// alert in THIS process, so a run that keeps hitting a full account (before
// the 5-minute fleet poll propagates the backoff row) alerts once, not once
// per file.
var (
	lastAccountAlertMu sync.Mutex
	lastAccountAlert   = map[string]time.Time{}
)

// accountAlertMinInterval is the in-process dedup window.  Cross-process and
// cross-node dedup come from the fleet backoff cache + the manager's DB read
// (see manager/host_alerts.go); this map only stops a hot retry loop.
const accountAlertMinInterval = 23 * time.Hour

// notifyAccountDisabled fires the operator alert for a host that just hit an
// account-level rejection.  Best-effort: never blocks or fails the upload.
//
// Dedup layers:
//  1. the CALLER checks the fleet-backoff cache BEFORE disabling (see
//     disableHostAccountLevel / the auth branch) — once our own disable has
//     populated the cache the check would always be true, so it must be
//     snapshotted first;
//  2. in-process map: at most one alert per host per ~day per node;
//  3. the manager sink re-checks the shared table and applies the notifier's
//     own cooldown before actually sending.
func notifyAccountDisabled(host, reason string) {
	if host == "" {
		return
	}
	// Layer 2: in-process throttle.
	lastAccountAlertMu.Lock()
	if last, ok := lastAccountAlert[host]; ok && time.Since(last) < accountAlertMinInterval {
		lastAccountAlertMu.Unlock()
		return
	}
	lastAccountAlert[host] = time.Now()
	lastAccountAlertMu.Unlock()

	if accountDisabledNotifyFn != nil {
		go accountDisabledNotifyFn(host, reason)
	}
}

// disableHostAccountLevel is the full account-level reaction: package-wide
// timed disable (which also publishes the fleet-wide backoff row so peer
// nodes skip the host), local instance disable, and the operator alert.
// Called from the uploader's error paths on their own goroutines.
func disableHostAccountLevel(name, errText string) {
	// Snapshot the fleet cache BEFORE disableHostFor writes it — a peer that
	// detected the same outage earlier owns the incident and this node stays
	// silent (its publish is then just a harmless row refresh).
	wasBackedOff := isFleetHostBackedOff(name)
	reason := "account-level rejection: " + firstLine(errText)
	disableHostFor(name, accountDisabledCooldown, reason)
	if !wasBackedOff {
		notifyAccountDisabled(name, reason)
	}
}

// AccountDisabledCooldown exposes the account-level disable window for the
// manager's alert text, so the message never drifts from the actual behaviour.
func AccountDisabledCooldown() time.Duration {
	return accountDisabledCooldown
}

// clearAccountAlertsForTest resets the in-process alert dedup map (test hook,
// mirrors clearTimedDisablesForTest / clearFleetBackoffsForTest).
func clearAccountAlertsForTest() {
	lastAccountAlertMu.Lock()
	defer lastAccountAlertMu.Unlock()
	lastAccountAlert = map[string]time.Time{}
}

// firstLine trims an error to its first line so backoff reasons and alert
// bodies stay readable when hosts return whole HTML error pages.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
