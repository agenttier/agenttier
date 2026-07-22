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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// dialTimeout and requestTimeout mirror pkg/router/sandboxhttp/client.go's
// timeout discipline (DefaultDialTimeout/DefaultRequestTimeout) — sa-review.md
// Medium finding #5 calls out that the delivery client needs a short
// timeout and no unchecked redirect-follow, since the controller has
// broader network reach than a sandboxed workload and a webhook URL is
// fully user-controlled (SSRF-shaped surface).
const (
	dialTimeout    = 5 * time.Second
	requestTimeout = 10 * time.Second
)

// newDeliveryHTTPClient returns an http.Client hardened against SSRF
// pivoting via redirect: it never follows a redirect automatically. A
// receiver that wants to redirect must be re-validated as a fresh
// subscription URL — silently following a 3xx to an internal address would
// defeat ValidateDeliveryURL's pre-flight check entirely.
func newDeliveryHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// signBody computes the X-AgentTier-Signature header value for a delivered
// body: "sha256=<hex hmac-sha256(secret, body)>". Signs the EXACT bytes
// that will be sent — the caller must pass the same []byte to both this
// function and the HTTP request body, never re-marshal in between (a
// re-serialized JSON object can reorder keys or normalize whitespace,
// producing a signature the receiver can never reproduce from the bytes it
// actually receives — sa-review.md High finding #4).
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookPayload is the delivered event body shape.
type webhookPayload struct {
	Event     string          `json:"event"`
	SandboxID string          `json:"sandboxId,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// deliverOnce makes exactly one HTTP delivery attempt. Returns the response
// status code (0 if the request never got a response, e.g. a network/DNS
// failure) and an error describing what went wrong, if anything. A non-2xx
// status is reported via the returned error but is not itself a Go error
// value distinct from a transport failure — callers only need to know
// success (nil error, 2xx) vs. failure (any error) to drive retry/backoff.
func deliverOnce(ctx context.Context, httpClient *http.Client, subURL, secret string, body []byte) (statusCode int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, subURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build delivery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AgentTier-Signature", signBody(secret, body))

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("delivery request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("delivery received non-2xx status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
