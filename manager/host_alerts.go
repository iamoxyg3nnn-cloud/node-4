package manager

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/notifier"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// notifyUploadHostNeedsKey is the operator alert for an upload host whose
// ACCOUNT can no longer accept uploads — storage quota full, account banned /
// suspended, or the key revoked.  The uploader already disabled the host
// locally and published the fleet-wide backoff row by the time this runs; this
// sink only decides whether THIS node's detection should raise the Discord
// alert.
//
// Cross-node dedup: the credentials are shared by every node, so when one
// account fills up, all nodes hit the same rejection within the 5-minute
// fleet-poll window.  Each candidate alert re-reads the shared backoff table
// and stays silent when an ACTIVE row for the host already exists — the first
// node whose publish landed owns the incident and alerts.  (A same-instant
// race can still produce a duplicate or two; the notifier's per-key cooldown
// keeps it bounded, and one incident posting a couple of identical messages
// is an acceptable price for not adding a new table.)
//
// The notifier's own per-key cooldown dedups repeats per process.
func notifyUploadHostNeedsKey(host, reason string) {
	// Cross-node dedup layer: skip when the shared table already has an active
	// row for this host.  Our own upsert is in flight on another goroutine —
	// if it hasn't landed yet we read "no row" and alert, which is correct
	// for the first detector and duplicates only in the same-instant race.
	client := server.GetDBClient()
	if client != nil {
		rows, err := client.ListHostBackoffs()
		if err == nil {
			now := time.Now()
			thisNode := server.NodeID()
			for _, r := range rows {
				if !strings.EqualFold(r.Host, host) {
					continue
				}
				// If a PEER node already reported an active backoff, stay silent
				// so peer nodes don't spam the webhook simultaneously.
				// But if this row was created by THIS node, we are the detector
				// and must fire our alert.
				if r.UpdatedBy != "" && r.UpdatedBy != thisNode {
					if until, ok := r.UntilTime(); ok && until.After(now) {
						return // a peer already reported it
					}
				}
			}
		}
		// On a read error fall through and let the notifier cooldown decide —
		// one possible extra message beats silently dropping a real outage.
	}

	title := fmt.Sprintf("🔑 Upload host needs a new key — %s", host)
	// Direct link to the host's sign-up/dashboard page so the operator can
	// grab a replacement key without hunting for the site.
	site := uploader.HostWebsite(host)
	siteLine := ""
	if site != "" {
		siteLine = fmt.Sprintf("\nGet a new key: %s", site)
	}
	// Direct link to THIS node's admin page (the tunnel URL rotates each
	// session, so resolve it at alert time) — one click to the Upload Keys /
	// Backoffs panels instead of hunting for the current runner URL.
	adminLine := ""
	if tunnel, err := server.LoadCurrentTunnel(); err == nil && tunnel != "" {
		adminLine = fmt.Sprintf("\nAdmin page (paste a new key here): %s/admin", strings.TrimSuffix(tunnel, "/"))
	}
	msg := fmt.Sprintf(
		"The account behind %s can no longer accept uploads.\n"+
			"Reason: %s\n%s%s\n\n"+
			"The host has been disabled fleet-wide for %v (auto re-enables after that, or on the next session restart).\n"+
			"To restore: paste a new %s API key in the admin page's Upload Keys panel — or tell the operator to clear its upload_host_backoffs row.",
		host, reason, siteLine, adminLine, uploader.AccountDisabledCooldown(), host,
	)
	notifier.Notify(notifier.KeyHostNeedsKey(host), title, msg)
	log.Printf("[host-alert] reported %s needs a new key (%s)", host, reason)
}
