/*
Copyright 2024 AgentTier Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhookdelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultMaxAttempts is how many delivery attempts (including the first)
// are made per event per subscription before giving up on that one event,
// per design.md's "retry w/ backoff (e.g. 5 attempts, exp backoff)".
const defaultMaxAttempts = 5

// defaultInitialBackoff is the delay before the second attempt; each
// subsequent attempt doubles it (exponential backoff).
const defaultInitialBackoff = 1 * time.Second

// Deliverer runs the leader-elected FR5 delivery loop: on each interval it
// reads Sandbox-kind Kubernetes Events created since the persisted cursor,
// matches them against webhook subscriptions, and delivers signed HTTP
// POSTs with retry/backoff and consecutive-failure auto-disable.
type Deliverer struct {
	client    client.Client
	logger    *slog.Logger
	namespace string // install namespace: where subscription Secrets + the state ConfigMap live
	interval  time.Duration
	store     *Store

	maxAttempts          int
	initialBackoff       time.Duration
	autoDisableThreshold int

	// Indirected for tests: a fake clock, a no-op sleep, an httptest-backed
	// HTTP client, and a permissive URL validator (a real httptest.Server
	// listens on loopback, which the real SSRF-guard ValidateDeliveryURL
	// correctly rejects — tests override this to exercise delivery logic
	// without also re-testing the SSRF guard, which has its own dedicated
	// tests against the real validator).
	now         func() time.Time
	sleep       func(time.Duration)
	httpClient  *http.Client
	validateURL func(string) error
}

// NewDeliverer builds a Deliverer. interval is how often a delivery pass
// runs; namespace is the install namespace (where subscription Secrets and
// the state ConfigMap live).
func NewDeliverer(c client.Client, logger *slog.Logger, namespace string, interval time.Duration) *Deliverer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Deliverer{
		client:               c,
		logger:               logger,
		namespace:            namespace,
		interval:             interval,
		store:                NewStore(c, namespace),
		maxAttempts:          defaultMaxAttempts,
		initialBackoff:       defaultInitialBackoff,
		autoDisableThreshold: defaultAutoDisableThreshold,
		now:                  time.Now,
		sleep:                time.Sleep,
		httpClient:           newDeliveryHTTPClient(),
		validateURL:          ValidateDeliveryURL,
	}
}

// RunLoop runs delivery passes on the configured interval until ctx is
// cancelled. Intended to be registered as a leader-elected manager Runnable
// (mirrors pkg/controller/backup.Scheduler.RunLoop).
func (d *Deliverer) RunLoop(ctx context.Context) {
	d.logger.Info("webhook delivery loop started", "namespace", d.namespace, "interval", d.interval.String())
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	d.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce performs a single delivery pass. Exported for tests.
//
// At-least-once semantics (DL4): the cursor is advanced and persisted only
// after the entire list-events -> list-subscriptions -> dispatch pass
// completes without a fatal error. A hard failure (e.g. the apiserver is
// unreachable) leaves the cursor untouched, so the next pass reprocesses
// the same window rather than silently skipping it. Individual delivery
// failures are NOT fatal to the pass — they're handled by retry/backoff/
// auto-disable and do not block the cursor from advancing past them.
func (d *Deliverer) RunOnce(ctx context.Context) {
	cursorState, err := d.store.LoadCursorState(ctx)
	if err != nil {
		d.logger.Error("webhook delivery: failed to load cursor", "error", err)
		return
	}

	events, newCursorState, bootstrap, err := d.listNewEvents(ctx, cursorState)
	if err != nil {
		d.logger.Error("webhook delivery: failed to list events", "error", err)
		return
	}

	if bootstrap {
		// First run ever (no persisted cursor): don't replay whatever
		// backlog of Sandbox events already exists in the cluster (which
		// predates every current subscription) — just establish a
		// starting point and begin following from here on the next pass.
		if err := d.store.SaveCursorState(ctx, newCursorState); err != nil {
			d.logger.Error("webhook delivery: failed to persist initial cursor", "error", err)
		}
		return
	}

	if len(events) == 0 {
		return
	}

	subs, secrets, err := ListSubscriptions(ctx, d.client, d.namespace)
	if err != nil {
		d.logger.Error("webhook delivery: failed to list subscriptions", "error", err)
		return
	}

	for _, evt := range events {
		d.dispatchEvent(ctx, evt, subs, secrets)
	}

	if err := d.store.SaveCursorState(ctx, newCursorState); err != nil {
		d.logger.Error("webhook delivery: failed to persist cursor", "error", err)
	}
}

// listNewEvents returns Sandbox-kind Events with LastTimestamp at-or-after
// cursorState.Cursor (an RFC3339 timestamp, or "" for "no cursor yet"),
// sorted oldest-first, plus the new cursor state to persist. bootstrap is
// true when cursor was empty — see RunOnce's bootstrap handling.
//
// The boundary second is deliberately INCLUSIVE (>=, not a strict >): the
// real Kubernetes API server truncates LastTimestamp to whole-second
// granularity on marshal, so two distinct Sandbox Events can legitimately
// share the exact timestamp the cursor was last saved at. A strict > would
// silently and permanently drop whichever of those events didn't happen to
// be seen before the cursor-saving pass ended (see decisions.md / task #45
// for the full at-least-once violation this closes). Events already
// delivered at the cursor's own second are excluded via
// cursorState.SeenUIDs — anything strictly after the cursor is
// unambiguously new and needs no such check.
func (d *Deliverer) listNewEvents(ctx context.Context, cursorState CursorState) (events []corev1.Event, newCursorState CursorState, bootstrap bool, err error) {
	bootstrap = cursorState.Cursor == ""
	var cursorTime time.Time
	if !bootstrap {
		cursorTime, err = time.Parse(time.RFC3339Nano, cursorState.Cursor)
		if err != nil {
			return nil, CursorState{}, false, fmt.Errorf("parse persisted cursor %q: %w", cursorState.Cursor, err)
		}
	}
	seenAtCursor := make(map[string]bool, len(cursorState.SeenUIDs))
	for _, uid := range cursorState.SeenUIDs {
		seenAtCursor[uid] = true
	}

	list := &corev1.EventList{}
	if err := d.client.List(ctx, list); err != nil {
		return nil, CursorState{}, false, fmt.Errorf("list events: %w", err)
	}

	// Pass 1: find maxSeen across every Sandbox-kind event regardless of
	// the cursor — this must be computed independent of dispatch/bootstrap
	// filtering so the second pass can correctly identify every event that
	// shares maxSeen's exact timestamp, including ones bootstrap will
	// never dispatch (see below).
	var sandboxEvents []corev1.Event
	maxSeen := cursorTime
	for _, evt := range list.Items {
		if evt.InvolvedObject.Kind != "Sandbox" {
			continue
		}
		ts := evt.LastTimestamp.Time
		if ts.IsZero() {
			continue
		}
		sandboxEvents = append(sandboxEvents, evt)
		if ts.After(maxSeen) {
			maxSeen = ts
		}
	}
	if maxSeen.IsZero() {
		maxSeen = d.now()
	}

	// Pass 2: select events to actually dispatch, and separately track
	// every event at maxSeen's own second for the persisted dedup set —
	// these are NOT the same set. On a bootstrap pass, nothing is
	// dispatched (no backlog replay), but any pre-existing event that
	// happens to land on maxSeen's second still MUST be recorded as seen:
	// otherwise the very next (non-bootstrap) pass's inclusive boundary
	// would treat that backlog event as new and deliver it after all,
	// silently defeating "don't replay backlog" one pass later.
	var matched []corev1.Event
	var atMaxSecondUIDs []string
	for _, evt := range sandboxEvents {
		ts := evt.LastTimestamp.Time
		if ts.Equal(maxSeen) {
			atMaxSecondUIDs = append(atMaxSecondUIDs, string(evt.UID))
		}
		if bootstrap {
			continue // bootstrap never replays backlog
		}
		if ts.Before(cursorTime) {
			continue // strictly older than the cursor — already processed
		}
		if ts.Equal(cursorTime) && seenAtCursor[string(evt.UID)] {
			continue // same second as the cursor AND already delivered
		}
		matched = append(matched, evt)
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].LastTimestamp.Time.Before(matched[j].LastTimestamp.Time)
	})

	// The dedup set persisted alongside the new cursor covers exactly the
	// events at maxSeen's own second (atMaxSecondUIDs, computed above from
	// the FULL event set, not just what got dispatched — this is what
	// keeps a bootstrap pass's skipped backlog from being misread as new
	// on the next pass). If maxSeen didn't advance past the previous
	// cursor second, the previously-seen set is carried forward too, so a
	// slow trickle of same-second events across many passes still never
	// re-delivers.
	nextSeenUIDs := atMaxSecondUIDs
	if maxSeen.Equal(cursorTime) {
		nextSeenUIDs = append(append([]string(nil), cursorState.SeenUIDs...), atMaxSecondUIDs...)
	}

	return matched, CursorState{Cursor: maxSeen.Format(time.RFC3339Nano), SeenUIDs: nextSeenUIDs}, bootstrap, nil
}

// dispatchEvent matches one Event against every subscription and delivers
// to each match, recording history and applying auto-disable per
// subscription independently — one subscription's failure never affects
// delivery to another.
func (d *Deliverer) dispatchEvent(ctx context.Context, evt corev1.Event, subs map[string]Subscription, secrets map[string]string) {
	webhookType, ok := webhookTypeForReason(evt.Reason)
	if !ok {
		return
	}
	sandboxName := evt.InvolvedObject.Name
	sandboxNamespace := evt.InvolvedObject.Namespace

	payload := webhookPayload{
		Event:     webhookType,
		SandboxID: sandboxName,
		Namespace: sandboxNamespace,
		Timestamp: evt.LastTimestamp.Time.UTC().Format(time.RFC3339),
	}
	if evt.Message != "" {
		data, err := json.Marshal(map[string]string{"message": evt.Message})
		if err == nil {
			payload.Data = data
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		d.logger.Error("webhook delivery: failed to marshal payload", "error", err, "event", webhookType, "sandbox", sandboxName)
		return
	}

	for id, sub := range subs {
		if !sub.Matches(webhookType, sandboxNamespace, sandboxName) {
			continue
		}
		// Defense-in-depth authorization check (task #50, CRITICAL): the
		// Router's handleCreateWebhook already enforces that a subscriber
		// owns (or is admin for) any sandboxId it registers, but this loop
		// must not trust that forever — a subscription created before that
		// fix shipped, or reached via any future bug in the Router-side
		// check, must still not leak another tenant's sandbox events. This
		// re-derives access independently from the live Sandbox object on
		// every single delivery, not just once at subscription-creation
		// time.
		if !canAccessSandbox(ctx, d.client, sub, sandboxNamespace, sandboxName) {
			d.logger.Warn("webhook delivery: subscription owner may not access this sandbox, skipping delivery",
				"subscription", id, "sandbox", sandboxName, "namespace", sandboxNamespace)
			continue
		}
		secret, ok := secrets[id]
		if !ok || secret == "" {
			// A subscription with no secret on record can never be
			// verified by the receiver — never attempt delivery with an
			// empty/missing HMAC key (sa-review.md Medium finding #3).
			d.logger.Error("webhook delivery: subscription has no signing secret, skipping", "subscription", id)
			continue
		}
		d.deliverWithRetry(ctx, id, sub, secret, webhookType, body)
	}
}

// deliverWithRetry attempts delivery to one subscription up to
// d.maxAttempts times with exponential backoff, re-validating the
// subscription URL before every single attempt (sa-review.md Medium
// finding #5 / DL7: DNS answers can change between attempts, so a
// once-at-creation-time check is not enough). Records exactly one
// DeliveryAttempt to history per event (the outcome of the LAST attempt
// made), and auto-disables the subscription if this pushes its
// consecutive-failure count past the threshold (FR5.4).
func (d *Deliverer) deliverWithRetry(ctx context.Context, id string, sub Subscription, secret, eventType string, body []byte) {
	backoff := d.initialBackoff
	var lastStatus int
	var lastErr error

	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if err := d.validateURL(sub.URL); err != nil {
			lastErr = fmt.Errorf("url failed pre-delivery validation: %w", err)
			lastStatus = 0
		} else {
			lastStatus, lastErr = deliverOnce(ctx, d.httpClient, sub.URL, secret, body)
		}
		if lastErr == nil {
			break
		}
		if attempt < d.maxAttempts {
			d.logger.Warn("webhook delivery attempt failed, retrying",
				"subscription", id, "event", eventType, "attempt", attempt, "error", lastErr)
			d.sleep(backoff)
			backoff *= 2
		}
	}

	success := lastErr == nil
	if !success {
		d.logger.Error("webhook delivery failed (all attempts exhausted)",
			"subscription", id, "event", eventType, "error", lastErr)
	}

	crossedThreshold, err := d.store.RecordAttempt(ctx, id, DeliveryAttempt{
		EventType:  eventType,
		Timestamp:  d.now().UTC().Format(time.RFC3339),
		StatusCode: lastStatus,
		Attempt:    d.maxAttempts,
		Success:    success,
	}, d.autoDisableThreshold)
	if err != nil {
		d.logger.Error("webhook delivery: failed to record attempt", "subscription", id, "error", err)
		return
	}

	if crossedThreshold {
		if err := DisableSubscription(ctx, d.client, d.namespace, id); err != nil {
			d.logger.Error("webhook delivery: failed to auto-disable subscription", "subscription", id, "error", err)
			return
		}
		d.logger.Warn("webhook subscription auto-disabled after consecutive failures",
			"subscription", id, "threshold", d.autoDisableThreshold)
	}
}
