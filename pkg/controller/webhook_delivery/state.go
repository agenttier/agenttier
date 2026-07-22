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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StateConfigMapName is the well-known ConfigMap holding the delivery
// cursor, per-subscription failure counters, and each subscription's
// bounded delivery history — all controller-owned state per DL1/DL4
// (design.md#FR5). It lives in the install namespace, mirroring the
// governance ConfigMapStore precedent (pkg/governance/policy.go).
const StateConfigMapName = "agenttier-webhook-cursor"

// maxHistoryPerSubscription bounds the delivery-attempt ring kept per
// subscription (NFR9: no unbounded queue growth). Only the most recent N
// attempts are retained; GET /webhooks/{id}/deliveries surfaces this.
const maxHistoryPerSubscription = 20

// defaultAutoDisableThreshold is the number of consecutive delivery
// failures after which a subscription is auto-disabled (FR5.4) rather than
// retried forever.
const defaultAutoDisableThreshold = 15

// DeliveryAttempt is one recorded delivery attempt, surfaced to the Router
// via GET /webhooks/{id}/deliveries (design.md's `{eventType,timestamp,
// statusCode,attempt,success}` shape).
type DeliveryAttempt struct {
	EventType  string `json:"eventType"`
	Timestamp  string `json:"timestamp"`
	StatusCode int    `json:"statusCode,omitempty"`
	Attempt    int    `json:"attempt"`
	Success    bool   `json:"success"`
}

// subscriptionState tracks consecutive-failure count and recent delivery
// history for one subscription.
type subscriptionState struct {
	ConsecutiveFailures int               `json:"consecutiveFailures"`
	History             []DeliveryAttempt `json:"history,omitempty"`
}

// persistedState is the full JSON shape stored in the ConfigMap's "state"
// key. Cursor is the resourceVersion of the last-processed Event object
// (metav1.ObjectMeta.ResourceVersion for corev1.Event is not monotonic
// across the whole cluster in a strictly comparable way, so the cursor is
// instead the latest LastTimestamp we've successfully dispatched-past, in
// RFC3339 — see cursor.go).
//
// CursorSecondUIDs is the set of Event UIDs already processed at exactly
// Cursor's timestamp. The real Kubernetes API server truncates
// LastTimestamp to whole-second granularity on marshal, so two distinct
// Sandbox Events can legitimately share the same persisted cursor value.
// Event selection treats the cursor's own second as INCLUSIVE (>=, not a
// strict >) so an event landing in the same second as the last-saved
// cursor is never silently dropped — this set is what keeps that inclusive
// re-scan from re-delivering an event it already processed. It is reset
// (not accumulated) whenever Cursor's timestamp actually advances to a new
// second, so it never grows unbounded.
type persistedState struct {
	Cursor           string                        `json:"cursor,omitempty"`
	CursorSecondUIDs []string                      `json:"cursorSecondUids,omitempty"`
	Subscriptions    map[string]*subscriptionState `json:"subscriptions,omitempty"`
}

// CursorState is the cursor plus the dedup set for its own second, as
// returned by LoadCursorState / persisted by SaveCursorState.
type CursorState struct {
	Cursor   string
	SeenUIDs []string
}

// Store reads and writes the controller's webhook-delivery ConfigMap.
// Every load/save pair uses the ConfigMap's resourceVersion for an
// optimistic-concurrency Update, matching governance.ConfigMapStore's
// existing pattern in this codebase — the delivery loop is a single
// leader-elected goroutine, so contention is not expected, but the pattern
// is cheap insurance against a manual kubectl edit racing a save.
type Store struct {
	Client    client.Client
	Namespace string
}

// NewStore returns a Store bound to the given install namespace.
func NewStore(c client.Client, namespace string) *Store {
	return &Store{Client: c, Namespace: namespace}
}

func (s *Store) load(ctx context.Context) (*persistedState, string, error) {
	cm := &corev1.ConfigMap{}
	err := s.Client.Get(ctx, types.NamespacedName{Name: StateConfigMapName, Namespace: s.Namespace}, cm)
	if err != nil && !errors.IsNotFound(err) {
		return nil, "", fmt.Errorf("load webhook delivery state configmap: %w", err)
	}
	state := &persistedState{Subscriptions: map[string]*subscriptionState{}}
	if cm.Data != nil {
		if raw, ok := cm.Data["state"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), state); err != nil {
				return nil, "", fmt.Errorf("parse webhook delivery state configmap: %w", err)
			}
		}
	}
	if state.Subscriptions == nil {
		state.Subscriptions = map[string]*subscriptionState{}
	}
	return state, cm.ResourceVersion, nil
}

func (s *Store) save(ctx context.Context, state *persistedState, resourceVersion string) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("serialize webhook delivery state: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: StateConfigMapName, Namespace: s.Namespace},
	}
	if resourceVersion == "" {
		cm.Data = map[string]string{"state": string(raw)}
		if err := s.Client.Create(ctx, cm); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create webhook delivery state configmap: %w", err)
			}
			if err := s.Client.Get(ctx, types.NamespacedName{Name: StateConfigMapName, Namespace: s.Namespace}, cm); err != nil {
				return fmt.Errorf("reload webhook delivery state configmap: %w", err)
			}
		} else {
			return nil
		}
	} else {
		cm.ResourceVersion = resourceVersion
	}
	cm.Data = map[string]string{"state": string(raw)}
	if err := s.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("update webhook delivery state configmap: %w", err)
	}
	return nil
}

// LoadCursorState returns the persisted cursor plus the dedup set for its
// own second (both zero-valued if no cursor has ever been saved).
func (s *Store) LoadCursorState(ctx context.Context) (CursorState, error) {
	state, _, err := s.load(ctx)
	if err != nil {
		return CursorState{}, err
	}
	return CursorState{Cursor: state.Cursor, SeenUIDs: state.CursorSecondUIDs}, nil
}

// SaveCursorState persists the cursor and the dedup set for its own
// second. Called only after a full match-dispatch pass over a batch of
// events has completed (at-least-once semantics per DL4/design.md: if the
// process crashes mid-batch, the next run reprocesses the whole batch
// rather than skipping events that weren't fully handled).
func (s *Store) SaveCursorState(ctx context.Context, next CursorState) error {
	state, rv, err := s.load(ctx)
	if err != nil {
		return err
	}
	state.Cursor = next.Cursor
	state.CursorSecondUIDs = next.SeenUIDs
	return s.save(ctx, state, rv)
}

// RecordAttempt appends a delivery attempt to a subscription's bounded
// history (trimming to maxHistoryPerSubscription), updates its
// consecutive-failure counter (reset to 0 on success), and returns whether
// the subscription crossed the auto-disable threshold on this call — the
// caller is responsible for actually marking the subscription disabled
// (a separate write against the Router's Secret-backed store, not this
// ConfigMap) when true is returned.
func (s *Store) RecordAttempt(ctx context.Context, subscriptionID string, attempt DeliveryAttempt, autoDisableThreshold int) (crossedThreshold bool, err error) {
	if autoDisableThreshold <= 0 {
		autoDisableThreshold = defaultAutoDisableThreshold
	}
	state, rv, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	sub, ok := state.Subscriptions[subscriptionID]
	if !ok {
		sub = &subscriptionState{}
		state.Subscriptions[subscriptionID] = sub
	}
	sub.History = append(sub.History, attempt)
	if len(sub.History) > maxHistoryPerSubscription {
		sub.History = sub.History[len(sub.History)-maxHistoryPerSubscription:]
	}
	if attempt.Success {
		sub.ConsecutiveFailures = 0
	} else {
		sub.ConsecutiveFailures++
	}
	crossedThreshold = sub.ConsecutiveFailures >= autoDisableThreshold
	if err := s.save(ctx, state, rv); err != nil {
		return false, err
	}
	return crossedThreshold, nil
}

// History returns the bounded delivery history for one subscription (most
// recent last), or an empty slice if none recorded yet.
func (s *Store) History(ctx context.Context, subscriptionID string) ([]DeliveryAttempt, error) {
	state, _, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	sub, ok := state.Subscriptions[subscriptionID]
	if !ok {
		return []DeliveryAttempt{}, nil
	}
	return sub.History, nil
}
