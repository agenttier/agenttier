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

package router

import (
	"regexp"
	"testing"
)

// rfc1123LabelRegexp matches a Kubernetes RFC 1123 object-name label:
// lowercase alphanumeric or '-', must start and end alphanumeric.
var rfc1123LabelRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TestGenerateWebhookID_SecretNameAlwaysRFC1123Safe is a property test (not
// a single example) for a bug that was probabilistic in the old
// base64.RawURLEncoding-based implementation: any 16-byte random sample had
// a near-certain chance of containing an uppercase character, and the old
// "wh_" prefix's underscore alone was always invalid. Verified live against
// a real Kubernetes apiserver (not just the fake client unit tests use,
// which perform no server-side name validation) — POST /webhooks failed
// with a Secret-name-invalid 500 on essentially every call. Runs many
// iterations because the failure mode was probabilistic, not deterministic.
func TestGenerateWebhookID_SecretNameAlwaysRFC1123Safe(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
		id, err := generateWebhookID()
		if err != nil {
			t.Fatalf("iteration %d: generateWebhookID: %v", i, err)
		}
		name := secretNameForWebhookID(id)
		if !rfc1123LabelRegexp.MatchString(name) {
			t.Fatalf("iteration %d: secret name %q (from id %q) is not RFC 1123 safe", i, name, id)
		}
		if len(name) > 253 {
			t.Fatalf("iteration %d: secret name %q exceeds the 253-char Kubernetes object-name limit (%d chars)", i, name, len(name))
		}
	}
}

// TestGenerateWebhookID_ProducesLowercaseHex pins the exact encoding
// (encoding/hex, not base64) so a future change back to a
// mixed-case/underscore-bearing alphabet would fail this test immediately,
// rather than only failing against a real apiserver.
func TestGenerateWebhookID_ProducesLowercaseHex(t *testing.T) {
	id, err := generateWebhookID()
	if err != nil {
		t.Fatalf("generateWebhookID: %v", err)
	}
	if len(id) != 32 { // 16 bytes -> 32 lowercase hex chars
		t.Errorf("id %q has length %d, want 32 (16 bytes hex-encoded)", id, len(id))
	}
	if matched, _ := regexp.MatchString(`^[0-9a-f]+$`, id); !matched {
		t.Errorf("id %q is not lowercase hex", id)
	}
}

// TestGenerateWebhookID_UniqueAcrossCalls is a light sanity check that the
// CSPRNG is actually being read (a regression to an all-zero or fixed
// buffer would silently produce the same "random" ID every time).
func TestGenerateWebhookID_UniqueAcrossCalls(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id, err := generateWebhookID()
		if err != nil {
			t.Fatalf("iteration %d: generateWebhookID: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("iteration %d: generated a duplicate id %q", i, id)
		}
		seen[id] = true
	}
}
