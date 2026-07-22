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

// Package auth implements authentication for the AgentTier Router.
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCValidator validates JWT tokens against an OIDC provider's JWKS endpoint.
type OIDCValidator struct {
	issuerURL  string
	clientID   string
	adminGroup string
	groupClaim string
	jwksURL    string
	jwksCache  *JWKSCache
	httpClient *http.Client
}

// OIDCConfig holds OIDC configuration.
type OIDCConfig struct {
	IssuerURL  string
	ClientID   string
	AdminGroup string
	GroupClaim string // Default: "groups"
}

// Claims represents the authenticated user's identity.
//
// SandboxID and ActionGroups are populated only for a sandbox-scoped API key
// (FR6) — see APIKeyRecord. A JWT-derived Claims (from ValidateToken) never
// sets these; only ValidateKey does, and only when the underlying record is
// scoped.
type Claims struct {
	Sub          string   `json:"sub"`
	Email        string   `json:"email"`
	Name         string   `json:"name"`
	Groups       []string `json:"groups"`
	IsAdmin      bool     `json:"isAdmin"`
	SandboxID    string   `json:"sandboxId,omitempty"`
	ActionGroups []string `json:"actionGroups,omitempty"`
}

// NewOIDCValidator creates a new OIDC token validator.
func NewOIDCValidator(config OIDCConfig) (*OIDCValidator, error) {
	groupClaim := config.GroupClaim
	if groupClaim == "" {
		groupClaim = "groups"
	}

	v := &OIDCValidator{
		issuerURL:  config.IssuerURL,
		clientID:   config.ClientID,
		adminGroup: config.AdminGroup,
		groupClaim: groupClaim,
		jwksCache:  NewJWKSCache(5 * time.Minute),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	// Discover JWKS URL from OIDC discovery endpoint
	if err := v.discover(); err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}

	// Initial JWKS fetch
	if err := v.jwksCache.Refresh(v.httpClient, v.jwksURL); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch failed: %w", err)
	}

	// Start background refresh
	go v.jwksCache.StartRefreshLoop(v.httpClient, v.jwksURL)

	return v, nil
}

// ValidateToken validates a JWT token and returns the extracted claims.
func (v *OIDCValidator) ValidateToken(ctx context.Context, tokenString string) (*Claims, error) {
	// Split JWT into parts
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Decode header to get kid
	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT header: %w", err)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("failed to parse JWT header: %w", err)
	}

	// Enforce the signing algorithm explicitly. We only verify RS256
	// (verifyRS256Signature is hardcoded to RSA + SHA-256). Rejecting any
	// other "alg" up front is defense-in-depth against algorithm-confusion
	// attacks (e.g. "none", or HS256 using the RSA public key as the HMAC
	// secret) — so a future refactor that branches on alg can never be
	// tricked into skipping or downgrading verification.
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported JWT alg %q: only RS256 is accepted", header.Alg)
	}

	// Get signing key from JWKS cache
	key, err := v.jwksCache.GetKey(header.Kid)
	if err != nil {
		return nil, fmt.Errorf("signing key not found: %w", err)
	}

	// Verify signature
	if err := verifyRS256Signature(parts[0]+"."+parts[1], parts[2], key); err != nil {
		return nil, fmt.Errorf("signature verification failed: %w", err)
	}

	// Decode and validate payload
	payloadBytes, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse JWT payload: %w", err)
	}

	// Validate standard claims
	if err := v.validateStandardClaims(payload); err != nil {
		return nil, err
	}

	// Extract user claims
	claims := &Claims{
		Sub:   getStringClaim(payload, "sub"),
		Email: getStringClaim(payload, "email"),
		Name:  getStringClaim(payload, "name"),
	}

	// Extract groups
	claims.Groups = getStringSliceClaim(payload, v.groupClaim)

	// Determine admin status
	claims.IsAdmin = v.isAdmin(claims.Groups)

	return claims, nil
}

// discover fetches the OIDC discovery document to find the JWKS URL.
func (v *OIDCValidator) discover() error {
	discoveryURL := strings.TrimSuffix(v.issuerURL, "/") + "/.well-known/openid-configuration"

	resp, err := v.httpClient.Get(discoveryURL)
	if err != nil {
		return fmt.Errorf("failed to fetch discovery document: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery endpoint returned %d", resp.StatusCode)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
		Issuer  string `json:"issuer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("failed to parse discovery document: %w", err)
	}

	v.jwksURL = doc.JWKSURI
	return nil
}

// validateStandardClaims checks exp, iss, and aud claims.
func (v *OIDCValidator) validateStandardClaims(payload map[string]interface{}) error {
	// Check expiration
	exp, ok := payload["exp"].(float64)
	if !ok {
		return fmt.Errorf("missing or invalid exp claim")
	}
	if time.Now().Unix() > int64(exp) {
		return fmt.Errorf("token expired")
	}

	// Check issuer
	iss, ok := payload["iss"].(string)
	if !ok || iss != v.issuerURL {
		return fmt.Errorf("invalid issuer: expected %s, got %s", v.issuerURL, iss)
	}

	// Check audience
	aud := payload["aud"]
	if !v.validateAudience(aud) {
		return fmt.Errorf("invalid audience: expected %s", v.clientID)
	}

	return nil
}

// validateAudience checks if the token audience includes our client ID.
func (v *OIDCValidator) validateAudience(aud interface{}) bool {
	switch a := aud.(type) {
	case string:
		return a == v.clientID
	case []interface{}:
		for _, item := range a {
			if s, ok := item.(string); ok && s == v.clientID {
				return true
			}
		}
	}
	return false
}

// isAdmin checks if the user belongs to the admin group.
func (v *OIDCValidator) isAdmin(groups []string) bool {
	if v.adminGroup == "" {
		return false
	}
	for _, g := range groups {
		if g == v.adminGroup {
			return true
		}
	}
	return false
}

// --- JWKS Cache ---

// JWKSCache caches JWKS keys with periodic refresh.
type JWKSCache struct {
	keys         map[string]*rsa.PublicKey
	mu           sync.RWMutex
	refreshEvery time.Duration
}

// NewJWKSCache creates a new JWKS cache.
func NewJWKSCache(refreshEvery time.Duration) *JWKSCache {
	return &JWKSCache{
		keys:         make(map[string]*rsa.PublicKey),
		refreshEvery: refreshEvery,
	}
}

// GetKey retrieves a public key by kid.
func (c *JWKSCache) GetKey(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key %s not found in JWKS cache", kid)
	}
	return key, nil
}

// Refresh fetches the JWKS from the endpoint and updates the cache.
func (c *JWKSCache) Refresh(client *http.Client, jwksURL string) error {
	resp, err := client.Get(jwksURL)
	if err != nil {
		return fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("failed to parse JWKS: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pubKey, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue // Skip invalid keys
		}
		newKeys[k.Kid] = pubKey
	}

	c.mu.Lock()
	c.keys = newKeys
	c.mu.Unlock()

	return nil
}

// StartRefreshLoop periodically refreshes the JWKS cache.
func (c *JWKSCache) StartRefreshLoop(client *http.Client, jwksURL string) {
	ticker := time.NewTicker(c.refreshEvery)
	defer ticker.Stop()

	for range ticker.C {
		_ = c.Refresh(client, jwksURL)
	}
}

// --- Helpers ---

func getStringClaim(payload map[string]interface{}, key string) string {
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}

func getStringSliceClaim(payload map[string]interface{}, key string) []string {
	raw, ok := payload[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []interface{}:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case string:
		return []string{v}
	}
	return nil
}

func base64URLDecode(s string) ([]byte, error) {
	// Add padding if needed
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64URLDecode(nStr)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64URLDecode(eStr)
	if err != nil {
		return nil, err
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}

// verifyRS256Signature verifies a JWT's RS256 signature: the signing input
// (header.payload) is SHA-256 hashed and checked against the base64url
// signature using RSASSA-PKCS1-v1_5, the algorithm OIDC providers use for
// `alg: RS256`.
//
// This previously returned nil unconditionally — accepting ANY signature,
// including forged tokens. That was the P0 auth hole. Do not "simplify" this
// back to a no-op.
func verifyRS256Signature(signingInput, signatureB64 string, key *rsa.PublicKey) error {
	if key == nil {
		return fmt.Errorf("no public key provided")
	}

	sig, err := base64URLDecode(signatureB64)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, hashed[:], sig); err != nil {
		return fmt.Errorf("RS256 signature invalid: %w", err)
	}
	return nil
}
