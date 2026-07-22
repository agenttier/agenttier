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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// permissiveValidator lets any URL through — used by tests that want to
// exercise delivery logic against an httptest.Server (which listens on
// loopback, correctly rejected by the real ValidateDeliveryURL). SSRF-guard
// correctness itself is tested separately against the real validator.
func permissiveValidator(string) error { return nil }

func sandboxEvent(name, namespace, reason, message string, ts time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			// Unique per call (nanosecond timestamp) — a real cluster
			// would never reuse an Event object name for two Events, and
			// tests create several Running events for the same sandbox.
			Name:      fmt.Sprintf("%s.%s.%d", name, reason, ts.UnixNano()),
			Namespace: namespace,
		},
		InvolvedObject: corev1.ObjectReference{Kind: "Sandbox", Name: name, Namespace: namespace},
		Reason:         reason,
		Message:        message,
		LastTimestamp:  metav1.NewTime(ts),
	}
}

// sandboxEventWithUID is like sandboxEvent but also stamps an explicit UID.
// The fake controller-runtime client (unlike a real apiserver) leaves UID
// empty on Create unless the caller sets one — task #45's same-second dedup
// logic is keyed on Event UID, so reproducing that bug/fix requires two
// distinct events that share both a truncated timestamp AND a real UID
// collision boundary (i.e. two different UIDs, same second).
func sandboxEventWithUID(name, namespace, reason, message string, ts time.Time, uid string) *corev1.Event {
	evt := sandboxEvent(name, namespace, reason, message, ts)
	evt.Name = fmt.Sprintf("%s.%s.%s", name, reason, uid) // keep Name unique too
	evt.UID = types.UID(uid)
	return evt
}

// subscriptionSecret builds an admin-owned subscription (IsAdmin: true) so
// existing delivery-mechanics tests (retry, backoff, HMAC, cursor) that
// predate task #50's access check keep exercising exactly what they were
// designed to, without also needing to seed a matching Sandbox object and
// a real owning UserID. Task #50's own tests (access_test.go) use
// subscriptionSecretOwnedBy directly to exercise the non-admin path.
func subscriptionSecret(id, url string, eventTypes []string, secret string, disabled bool) *corev1.Secret {
	return subscriptionSecretFor(Subscription{ID: id, URL: url, EventTypes: eventTypes, Disabled: disabled, IsAdmin: true}, secret)
}

// subscriptionSecretOwnedBy builds a non-admin subscription owned by
// userID — used by access_test.go to exercise task #50's controller-side
// canAccessSandbox check against a real (non-admin) subscriber.
func subscriptionSecretOwnedBy(id, url string, eventTypes []string, sandboxID, userID, secret string) *corev1.Secret {
	return subscriptionSecretFor(Subscription{ID: id, URL: url, EventTypes: eventTypes, SandboxID: sandboxID, UserID: userID}, secret)
}

func subscriptionSecretFor(rec Subscription, secret string) *corev1.Secret {
	recJSON, _ := json.Marshal(rec)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNameForSubscriptionID(rec.ID),
			Namespace: "agenttier",
			Labels:    map[string]string{subscriptionSecretPurposeLabel: subscriptionSecretPurpose},
		},
		Data: map[string][]byte{
			"record": recJSON,
			"secret": []byte(secret),
		},
	}
}

// --- fake receiver ---

type fakeReceiver struct {
	mu          sync.Mutex
	receivedRaw [][]byte
	sigHeader   []string
	failNext    int32 // atomic counter of remaining calls to fail with 500
}

func newFakeReceiver() *fakeReceiver {
	return &fakeReceiver{}
}

func (f *fakeReceiver) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.receivedRaw = append(f.receivedRaw, body)
		f.sigHeader = append(f.sigHeader, r.Header.Get("X-AgentTier-Signature"))
		f.mu.Unlock()
		if atomic.LoadInt32(&f.failNext) > 0 {
			atomic.AddInt32(&f.failNext, -1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func (f *fakeReceiver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.receivedRaw)
}

// --- delivery + at-least-once across cursor restart ---

func TestDeliverer_DeliversMatchingEventToSubscription(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.running"}, "s3cr3t", false)
	evt := sandboxEvent("sb1", "default", "Running", "Sandbox is ready", time.Now())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub, evt).Build()

	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {} // no real sleeping in tests

	ctx := context.Background()
	// First pass bootstraps the cursor (no delivery yet — see RunOnce's
	// bootstrap handling, which never replays pre-existing backlog).
	d.RunOnce(ctx)
	if receiver.count() != 0 {
		t.Fatalf("expected no delivery on the bootstrap pass, got %d", receiver.count())
	}

	// A second event created after the cursor was established.
	evt2 := sandboxEvent("sb1", "default", "Running", "still ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt2); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)
	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", receiver.count())
	}
}

func TestDeliverer_AtLeastOnceAcrossCursorRestart(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.running"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d1 := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d1.validateURL = permissiveValidator
	d1.sleep = func(time.Duration) {}
	d1.RunOnce(ctx) // bootstrap cursor at t0

	evt := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d1.RunOnce(ctx)
	if receiver.count() != 1 {
		t.Fatalf("expected 1 delivery from d1, got %d", receiver.count())
	}

	// Simulate a controller restart: a brand new Deliverer against the SAME
	// backing store must not re-deliver the already-processed event, but
	// must pick up a new one created after the persisted cursor —
	// verifying the cursor (not process memory) is the source of truth
	// (DL4's "persisted ConfigMap cursor for at-least-once").
	d2 := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d2.validateURL = permissiveValidator
	d2.sleep = func(time.Duration) {}
	d2.RunOnce(ctx)
	if receiver.count() != 1 {
		t.Fatalf("expected no re-delivery of the already-processed event after restart, got %d total", receiver.count())
	}

	evt2 := sandboxEvent("sb1", "default", "Running", "still ready", time.Now().Add(2*time.Second))
	if err := c.Create(ctx, evt2); err != nil {
		t.Fatalf("create event2: %v", err)
	}
	d2.RunOnce(ctx)
	if receiver.count() != 2 {
		t.Fatalf("expected the fresh deliverer to pick up the new event past the persisted cursor, got %d total", receiver.count())
	}
}

func TestDeliverer_UnmatchedEventTypeNotDelivered(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	// Subscribed only to sandbox.error — a Running event must not deliver.
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.error"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)
	if receiver.count() != 0 {
		t.Fatalf("expected no delivery for an unsubscribed event type, got %d", receiver.count())
	}
}

func TestDeliverer_SandboxScopedSubscriptionOnlyMatchesItsSandbox(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNameForSubscriptionID("wh1"),
			Namespace: "agenttier",
			Labels:    map[string]string{subscriptionSecretPurposeLabel: subscriptionSecretPurpose},
		},
	}
	// IsAdmin: true — this test exercises Matches()'s sandboxID filtering,
	// not task #50's access check (covered separately in access_test.go).
	rec := Subscription{ID: "wh1", URL: srv.URL, EventTypes: []string{"sandbox.running"}, SandboxID: "sb-mine", IsAdmin: true}
	recJSON, _ := json.Marshal(rec)
	sub.Data = map[string][]byte{"record": recJSON, "secret": []byte("s3cr3t")}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx)

	other := sandboxEvent("sb-other", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, other); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)
	if receiver.count() != 0 {
		t.Fatalf("expected no delivery for a different sandbox, got %d", receiver.count())
	}

	mine := sandboxEvent("sb-mine", "default", "Running", "ready", time.Now().Add(2*time.Second))
	if err := c.Create(ctx, mine); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)
	if receiver.count() != 1 {
		t.Fatalf("expected delivery for the subscription's own sandbox, got %d", receiver.count())
	}
}

// --- retry then auto-disable ---

func TestDeliverer_RetriesOnFailureThenSucceeds(t *testing.T) {
	receiver := newFakeReceiver()
	atomic.StoreInt32(&receiver.failNext, 2) // fail first 2 attempts, succeed on 3rd
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.running"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	var slept []time.Duration
	d.sleep = func(dur time.Duration) { slept = append(slept, dur) }
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", receiver.count())
	}
	if len(slept) != 2 {
		t.Fatalf("expected 2 backoff sleeps (before attempts 2 and 3), got %d: %v", len(slept), slept)
	}
	if slept[1] <= slept[0] {
		t.Errorf("expected exponential backoff (second sleep > first), got %v then %v", slept[0], slept[1])
	}

	history, err := d.store.History(ctx, "wh1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || !history[0].Success {
		t.Fatalf("expected exactly one successful history entry (the pass's final outcome), got %+v", history)
	}
}

func TestDeliverer_AutoDisablesAfterConsecutiveFailureThreshold(t *testing.T) {
	receiver := newFakeReceiver()
	atomic.StoreInt32(&receiver.failNext, 1<<20) // always fail
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.running"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.autoDisableThreshold = 2 // low threshold to keep the test fast
	d.RunOnce(ctx)             // bootstrap

	for i := 0; i < 2; i++ {
		evt := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(time.Duration(i+1)*time.Second))
		if err := c.Create(ctx, evt); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
		d.RunOnce(ctx)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "agenttier", Name: secretNameForSubscriptionID("wh1")}, secret); err != nil {
		t.Fatalf("get subscription secret: %v", err)
	}
	var rec Subscription
	if err := json.Unmarshal(secret.Data["record"], &rec); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if !rec.Disabled {
		t.Fatalf("expected subscription to be auto-disabled after %d consecutive failures, record: %+v", d.autoDisableThreshold, rec)
	}

	// A disabled subscription must not receive further deliveries.
	beforeCount := receiver.count()
	evt3 := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(10*time.Second))
	if err := c.Create(ctx, evt3); err != nil {
		t.Fatalf("create event3: %v", err)
	}
	d.RunOnce(ctx)
	// The freshly-read subscription list will now show Disabled: true, so
	// Matches() short-circuits — no new delivery attempts (of any kind,
	// success or failure) should reach the receiver.
	afterCount := receiver.count()
	if afterCount != beforeCount {
		t.Errorf("expected no further delivery attempts once disabled, before=%d after=%d", beforeCount, afterCount)
	}
}

// --- NFR9: bounded delivery history ---

func TestStore_HistoryIsBoundedPerSubscription(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := NewStore(c, "agenttier")
	ctx := context.Background()

	const attempts = maxHistoryPerSubscription + 15
	for i := 0; i < attempts; i++ {
		if _, err := store.RecordAttempt(ctx, "wh1", DeliveryAttempt{
			EventType: "sandbox.running",
			Timestamp: time.Now().Format(time.RFC3339),
			Success:   true,
		}, defaultAutoDisableThreshold); err != nil {
			t.Fatalf("record attempt %d: %v", i, err)
		}
	}

	history, err := store.History(ctx, "wh1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != maxHistoryPerSubscription {
		t.Fatalf("expected history capped at %d entries, got %d", maxHistoryPerSubscription, len(history))
	}
}

func TestStore_ConsecutiveFailuresResetOnSuccess(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := NewStore(c, "agenttier")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := store.RecordAttempt(ctx, "wh1", DeliveryAttempt{Success: false}, 100); err != nil {
			t.Fatalf("record failure %d: %v", i, err)
		}
	}
	crossed, err := store.RecordAttempt(ctx, "wh1", DeliveryAttempt{Success: true}, 100)
	if err != nil {
		t.Fatalf("record success: %v", err)
	}
	if crossed {
		t.Fatal("a success must never itself cross the auto-disable threshold")
	}

	// One more failure after the reset should count as failure #1, not #4.
	crossed, err = store.RecordAttempt(ctx, "wh1", DeliveryAttempt{Success: false}, 2)
	if err != nil {
		t.Fatalf("record failure after reset: %v", err)
	}
	if crossed {
		t.Fatal("expected the counter to have reset after the success, so 1 failure should not cross a threshold of 2")
	}
}

// --- HMAC signing correctness (sa-review.md checklist #10) ---

func verifySignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// Constant-time comparison per sa-review.md High finding #4 — this test
	// helper itself must not regress to a plain == the way it warns
	// production code against doing.
	return hmac.Equal([]byte(header[len(prefix):]), []byte(expected))
}

func TestSignBody_ValidSignatureVerifies(t *testing.T) {
	secret := "webhook-signing-secret"
	body := []byte(`{"event":"sandbox.running","sandboxId":"sb1"}`)
	header := signBody(secret, body)
	if !verifySignature(secret, body, header) {
		t.Fatal("expected a freshly signed body to verify")
	}
}

func TestSignBody_BitFlippedSignatureFailsVerification(t *testing.T) {
	secret := "webhook-signing-secret"
	body := []byte(`{"event":"sandbox.running"}`)
	header := signBody(secret, body)
	// Flip one hex character in the digest — the checklist's specific
	// "single-bit-flipped signature verifies false" requirement.
	digest := header[len("sha256="):]
	flipped := "0"
	if digest[0] == '0' {
		flipped = "1"
	}
	tamperedHeader := "sha256=" + flipped + digest[1:]
	if verifySignature(secret, body, tamperedHeader) {
		t.Fatal("expected a bit-flipped signature to fail verification")
	}
}

func TestSignBody_TamperedBodyFailsVerification(t *testing.T) {
	secret := "webhook-signing-secret"
	body := []byte(`{"event":"sandbox.running","sandboxId":"sb1"}`)
	header := signBody(secret, body)
	tamperedBody := []byte(`{"event":"sandbox.running","sandboxId":"sb2"}`)
	if verifySignature(secret, tamperedBody, header) {
		t.Fatal("expected a tampered body to fail verification against the original signature")
	}
}

func TestDeliverOnce_SignsExactDeliveredBytes(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	secret := "s3cr3t"
	body := []byte(`{"event":"sandbox.running","sandboxId":"sb1","timestamp":"2026-07-21T00:00:00Z"}`)
	client := newDeliveryHTTPClient()
	if _, err := deliverOnce(context.Background(), client, srv.URL, secret, body); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}

	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.receivedRaw) != 1 {
		t.Fatalf("expected 1 received request, got %d", len(receiver.receivedRaw))
	}
	if string(receiver.receivedRaw[0]) != string(body) {
		t.Fatalf("receiver saw different bytes than were signed: got %q want %q", receiver.receivedRaw[0], body)
	}
	if !verifySignature(secret, receiver.receivedRaw[0], receiver.sigHeader[0]) {
		t.Fatal("signature over the exact delivered bytes should verify")
	}
}

// --- SSRF guard (sa-review.md Medium finding #5 / checklist #11) ---

func TestValidateDeliveryURL_RejectsNonHTTPS(t *testing.T) {
	if err := ValidateDeliveryURL("http://example.com/hook"); err == nil {
		t.Fatal("expected non-https url to be rejected")
	}
}

func TestValidateDeliveryURL_RejectsLoopback(t *testing.T) {
	if err := ValidateDeliveryURL("https://127.0.0.1/hook"); err == nil {
		t.Fatal("expected loopback url to be rejected")
	}
	if err := ValidateDeliveryURL("https://localhost/hook"); err == nil {
		t.Fatal("expected localhost (resolves to loopback) to be rejected")
	}
}

func TestValidateDeliveryURL_RejectsCloudMetadataAddress(t *testing.T) {
	if err := ValidateDeliveryURL("https://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("expected cloud metadata address to be rejected")
	}
}

func TestValidateDeliveryURL_RejectsPrivateRFC1918Ranges(t *testing.T) {
	cases := []string{
		"https://10.0.0.1/hook",
		"https://172.16.0.1/hook",
		"https://192.168.1.1/hook",
	}
	for _, u := range cases {
		if err := ValidateDeliveryURL(u); err == nil {
			t.Errorf("expected %s (private range) to be rejected", u)
		}
	}
}

func TestValidateDeliveryURL_AllowsPublicHTTPS(t *testing.T) {
	// A well-known public IP-literal HTTPS URL — avoids a real DNS lookup
	// flaking the test while still exercising the "not in any disallowed
	// range" success path. 93.184.216.34 was example.com's IP; any
	// non-private literal works since we're not asserting reachability.
	if err := ValidateDeliveryURL("https://93.184.216.34/hook"); err != nil {
		t.Fatalf("expected a public IP-literal https url to pass the range check, got: %v", err)
	}
}

func TestDeliverWithRetry_RevalidatesURLBeforeEachAttempt(t *testing.T) {
	// A subscription whose URL fails the (real, non-permissive) SSRF guard
	// must never reach the HTTP client at all — this proves
	// deliverWithRetry calls d.validateURL itself rather than only at
	// subscription-creation time (which is the Router's job, not this
	// loop's, but the loop must re-check per DL7).
	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", "https://127.0.0.1:1/hook", []string{"sandbox.running"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	// Deliberately leave d.validateURL as the REAL ValidateDeliveryURL
	// (the default from NewDeliverer) to prove the loopback URL is caught.
	d.sleep = func(time.Duration) {}
	d.maxAttempts = 1
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("sb1", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	history, err := d.store.History(ctx, "wh1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || history[0].Success {
		t.Fatalf("expected a recorded failure (SSRF guard rejection), got %+v", history)
	}
	if history[0].StatusCode != 0 {
		t.Errorf("expected status code 0 (request never sent), got %d", history[0].StatusCode)
	}
}

// --- same-second cursor boundary (task #45 — at-least-once fix) ---

// truncateToSecond mimics the real Kubernetes API server's behavior:
// corev1.Event.LastTimestamp (metav1.Time) is always serialized at
// whole-second RFC3339 granularity on JSON marshal, dropping any
// fractional-second component. The fake controller-runtime client used
// elsewhere in this file preserves full nanosecond precision, which is
// exactly why the original bug (task #45) had zero unit-test coverage —
// every existing test's synthetic timestamps never collided at the second
// boundary. Tests in this section explicitly truncate to reproduce that
// real-cluster behavior.
func truncateToSecond(t time.Time) time.Time {
	return t.Truncate(time.Second)
}

// TestListNewEvents_SameSecondEventsBothSelected_InclusiveBoundary is the
// direct reproduction test: two distinct Sandbox Events share the EXACT
// same (second-truncated) LastTimestamp, straddling a cursor-save boundary
// — event A is processed in pass 1 (which saves the cursor at that exact
// second), event B shares that second but wasn't seen in pass 1 (simulating
// list-ordering / informer-lag — modeled here by simply not existing yet
// when pass 1's cursor was computed). Against the OLD strict `ts.After
// (cursorTime)` selection, pass 2 would compute `T.After(T) == false` and
// permanently skip event B. The fix (`>=` plus UID dedup) must select B in
// pass 2 without re-selecting A.
func TestListNewEvents_SameSecondEventsBothSelected_InclusiveBoundary(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)

	sameSecond := truncateToSecond(time.Now())
	evtA := sandboxEventWithUID("sb1", "default", "Running", "a", sameSecond, "uid-a")
	if err := c.Create(context.Background(), evtA); err != nil {
		t.Fatalf("create evtA: %v", err)
	}

	// Pass 1: bootstrap. maxSeen becomes sameSecond (evtA is the only
	// event); nothing is dispatched (bootstrap never replays backlog), but
	// evtA's UID must be recorded as "seen at this second" so pass 2 knows
	// not to re-select it once the inclusive boundary admits that second.
	events1, cursorState1, bootstrap1, err := d.listNewEvents(context.Background(), CursorState{})
	if err != nil {
		t.Fatalf("listNewEvents (bootstrap): %v", err)
	}
	if !bootstrap1 {
		t.Fatal("expected bootstrap on first call with no cursor")
	}
	if len(events1) != 0 {
		t.Fatalf("bootstrap must not dispatch anything, got %d events", len(events1))
	}
	if cursorState1.Cursor != sameSecond.Format(time.RFC3339Nano) {
		t.Fatalf("expected cursor = %v, got %v", sameSecond, cursorState1.Cursor)
	}
	if len(cursorState1.SeenUIDs) != 1 || cursorState1.SeenUIDs[0] != "uid-a" {
		t.Fatalf("expected SeenUIDs=[uid-a] after bootstrap pass, got %v", cursorState1.SeenUIDs)
	}

	// Event B lands with the SAME truncated second as A (the real-world
	// trigger: two phase transitions in the same wall-clock second).
	evtB := sandboxEventWithUID("sb1", "default", "Stopped", "b", sameSecond, "uid-b")
	if err := c.Create(context.Background(), evtB); err != nil {
		t.Fatalf("create evtB: %v", err)
	}

	// Pass 2: cursor is now sameSecond with SeenUIDs=[uid-a]. The OLD code
	// (strict After) would find zero matches here since both A and B share
	// exactly the cursor's timestamp. The FIX must select B (new UID at
	// the cursor's second) while excluding A (already-seen UID).
	events2, cursorState2, bootstrap2, err := d.listNewEvents(context.Background(), cursorState1)
	if err != nil {
		t.Fatalf("listNewEvents (pass 2): %v", err)
	}
	if bootstrap2 {
		t.Fatal("pass 2 must not be a bootstrap pass — a cursor is already persisted")
	}
	if len(events2) != 1 {
		t.Fatalf("expected exactly 1 new event (B) selected in pass 2, got %d: %+v", len(events2), events2)
	}
	if events2[0].UID != "uid-b" {
		t.Fatalf("expected event B (uid-b) to be selected, got UID=%q", events2[0].UID)
	}
	// The dedup set must now cover BOTH uid-a and uid-b at this second, so
	// a hypothetical pass 3 (nothing new arrives) doesn't re-select either.
	seen := map[string]bool{}
	for _, u := range cursorState2.SeenUIDs {
		seen[u] = true
	}
	if !seen["uid-a"] || !seen["uid-b"] {
		t.Fatalf("expected SeenUIDs to cover both uid-a and uid-b after pass 2, got %v", cursorState2.SeenUIDs)
	}

	// Pass 3: nothing new. Must select zero events — both UIDs at this
	// second are now marked seen, so the inclusive boundary correctly
	// treats them as already-delivered rather than re-selecting them.
	events3, _, _, err := d.listNewEvents(context.Background(), cursorState2)
	if err != nil {
		t.Fatalf("listNewEvents (pass 3): %v", err)
	}
	if len(events3) != 0 {
		t.Fatalf("expected no re-delivery in pass 3 (both same-second events already seen), got %d: %+v", len(events3), events3)
	}
}

// TestDeliverer_SameSecondEvents_BothDeliveredEndToEnd exercises the same
// scenario through the full delivery pipeline (RunOnce -> dispatch ->
// persisted ConfigMap state), rather than calling listNewEvents directly,
// so the fix is verified end-to-end and not just at the selection-logic
// unit level.
func TestDeliverer_SameSecondEvents_BothDeliveredEndToEnd(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"sandbox.running", "sandbox.stopped"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	ctx := context.Background()

	sameSecond := truncateToSecond(time.Now())
	evtA := sandboxEventWithUID("sb1", "default", "Running", "a", sameSecond, "uid-a-e2e")
	if err := c.Create(ctx, evtA); err != nil {
		t.Fatalf("create evtA: %v", err)
	}
	d.RunOnce(ctx) // bootstrap — no delivery yet, but uid-a-e2e recorded as seen

	if receiver.count() != 0 {
		t.Fatalf("expected no delivery on bootstrap pass, got %d", receiver.count())
	}

	// Event B shares evtA's exact (truncated) second.
	evtB := sandboxEventWithUID("sb1", "default", "Stopped", "b", sameSecond, "uid-b-e2e")
	if err := c.Create(ctx, evtB); err != nil {
		t.Fatalf("create evtB: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery (event B — A was pre-existing backlog at bootstrap), got %d", receiver.count())
	}

	// A further pass with nothing new must not re-deliver B (or A).
	d.RunOnce(ctx)
	if receiver.count() != 1 {
		t.Fatalf("expected no re-delivery on a subsequent empty pass, got %d total", receiver.count())
	}
}
