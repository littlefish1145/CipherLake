package pluginloader

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CapabilityToken is an opaque 64-bit value passed to WASM as the first
// argument of every host import (spec §3.3 A6). WASM cannot hold host
// pointers, so the token is a value type — the loader maintains a
// map from token ID to the granted capability set.
//
// Tokens are issued per-instance at wasm_init time and revoked when the
// instance is destroyed (instance_pool.go). Tokens issued for one
// instance are never valid in another — the host import lookup will
// fail with ErrUnauthorized.
type CapabilityToken uint64

// Capability records what a token allows. The token → Capability map
// is the single source of truth for what a plugin instance can do.
type Capability struct {
	// PluginName is the plugin this token was issued to.
	PluginName string

	// TrustTier is the tier the plugin was loaded at (0/1/2). Some
	// capabilities (iam:policy:evaluate, kms:datakey:generate,
	// crypto:audit:sign, gateway:hook:register_for_other) are
	// Tier-2-only (spec §3.7 A4).
	TrustTier int

	// Capabilities is the list of capability strings granted to this
	// token (e.g. "storage:get:memories/*", "state:kv"). Scoped
	// capabilities are matched at host import time via ScopeMatches.
	Capabilities []string

	// IssuedAt is when the token was issued (for audit + expiry).
	IssuedAt time.Time

	// ExpiresAt is when the token expires. Zero means no expiry.
	ExpiresAt time.Time

	// revoked marks the token as no longer usable. Once true, all host
	// imports using this token return ErrUnauthorized. Set by Revoke().
	revoked bool
}

// CapabilityTable is the in-memory registry of live tokens. It is
// goroutine-safe. Tokens are not persisted — on loader restart, all
// in-flight instances are gone and tokens are reissued lazily when
// instances are recreated.
type CapabilityTable struct {
	mu     sync.RWMutex
	nextID uint64
	tokens map[CapabilityToken]*Capability
}

// NewCapabilityTable constructs an empty token table.
func NewCapabilityTable() *CapabilityTable {
	return &CapabilityTable{
		tokens: make(map[CapabilityToken]*Capability),
	}
}

// Issue mints a new capability token for the given plugin + capability
// set. The token is valid until Revoke is called or (if expiresAt is
// non-zero) the expiry time passes.
//
// Token IDs are drawn from a crypto-random 64-bit space (except 0 and
// 1<<63, which we reserve as "no token" sentinels).
func (t *CapabilityTable) Issue(pluginName string, tier int, caps []string, expiresAt time.Time) (CapabilityToken, error) {
	if pluginName == "" {
		return 0, errors.New("plugin name is required")
	}
	for i := 0; i < 8; i++ {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("failed to generate token: %w", err)
		}
		id := binary.LittleEndian.Uint64(buf[:])
		// Avoid 0 (sentinel) and high bit (keep token visually distinct
		// from signed i64 negative values when inspected in tools).
		id &=^ (uint64(1) << 63)
		if id == 0 {
			continue
		}
		tok := CapabilityToken(id)
		t.mu.Lock()
		if _, exists := t.tokens[tok]; exists {
			t.mu.Unlock()
			continue
		}
		t.tokens[tok] = &Capability{
			PluginName:   pluginName,
			TrustTier:    tier,
			Capabilities: caps,
			IssuedAt:     time.Now(),
			ExpiresAt:    expiresAt,
		}
		t.mu.Unlock()
		return tok, nil
	}
	return 0, errors.New("failed to issue unique token after 8 attempts")
}

// Lookup returns the capability record for a token, or nil if the token
// is unknown, revoked, or expired. Host imports use this to authorize
// each call.
func (t *CapabilityTable) Lookup(tok CapabilityToken) *Capability {
	t.mu.RLock()
	c := t.tokens[tok]
	t.mu.RUnlock()
	if c == nil {
		return nil
	}
	if c.revoked {
		return nil
	}
	if !c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt) {
		return nil
	}
	return c
}

// Revoke marks a token as no longer usable. Called when an instance is
// returned to the pool and destroyed. Revoking an unknown token is a
// no-op (idempotent).
func (t *CapabilityTable) Revoke(tok CapabilityToken) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.tokens[tok]; ok {
		c.revoked = true
	}
}

// Delete removes a token from the table entirely. Called when the
// instance is fully destroyed (not just returned to pool). Use Revoke
// first to make the token unusable while any in-flight host call
// finishes, then Delete after a grace period.
func (t *CapabilityTable) Delete(tok CapabilityToken) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tokens, tok)
}

// RevokeAllForPlugin revokes every token belonging to the named plugin.
// Used during uninstall/reload to immediately invalidate all in-flight
// instances of a plugin (spec §3.5).
func (t *CapabilityTable) RevokeAllForPlugin(pluginName string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, c := range t.tokens {
		if c.PluginName == pluginName {
			c.revoked = true
			n++
		}
	}
	return n
}

// Count returns the number of live (non-revoked) tokens. Used by the
// metrics host import to report active instance count.
func (t *CapabilityTable) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for _, c := range t.tokens {
		if !c.revoked {
			n++
		}
	}
	return n
}

// HasCapability reports whether the token grants the given capability.
// The match is on the full capability string (namespace + name + scope).
// For scoped capabilities (e.g. "storage:get:memories/sessions/*") the
// caller passes the requested concrete capability (e.g.
// "storage:get:memories/sessions/abc") and ScopeMatches does globbing.
func (c *Capability) HasCapability(requested string) bool {
	for _, granted := range c.Capabilities {
		if capabilityMatches(granted, requested) {
			return true
		}
	}
	return false
}

// IsTier2Only reports whether the capability string names a Tier-2-only
// capability (spec §3.7 A4). Host imports for these capabilities must
// additionally check that the token's TrustTier == 2.
func IsTier2Only(capability string) bool {
	tier2Prefixes := []string{
		"iam:policy:evaluate",
		"kms:datakey:generate",
		"crypto:audit:sign",
		"gateway:hook:register_for_other",
	}
	for _, p := range tier2Prefixes {
		if capability == p || strings.HasPrefix(capability, p+":") {
			return true
		}
	}
	return false
}

// capabilityMatches checks whether the granted capability authorizes
// the requested capability. A scoped grant also authorizes the base
// capability (e.g. "vector:search:*" authorizes "vector:search"), and
// glob grants ending in "/*" match any suffix under that prefix.
//
// Examples:
//
//	"storage:get:memories/*"  matches "storage:get:memories/sessions/abc"
//	"storage:get:memories/*"  matches "storage:get"
//	"storage:get:memories"    matches only "storage:get:memories"
//	"state:kv"               matches "state:kv"
//	"vector:search:*"        matches "vector:search:anything"
//	"vector:search:*"        matches "vector:search"
func capabilityMatches(granted, requested string) bool {
	if granted == requested {
		return true
	}
	// Scoped grant authorizes the base capability and any narrower request.
	if strings.HasPrefix(granted, requested+":") {
		return true
	}
	if !strings.HasSuffix(granted, "*") {
		return false
	}
	prefix := strings.TrimSuffix(granted, "*")
	return strings.HasPrefix(requested, prefix)
}

// Authorize is the helper host imports call to validate a token has a
// capability. Returns nil if authorized; ErrUnauthorized (or a wrapped
// variant) otherwise. This is the single chokepoint for capability
// enforcement — every host import must call it.
func (t *CapabilityTable) Authorize(tok CapabilityToken, requested string) error {
	c := t.Lookup(tok)
	if c == nil {
		return ErrUnauthorized{Capability: requested, Reason: "token unknown, revoked, or expired"}
	}
	if IsTier2Only(requested) && c.TrustTier < 2 {
		return ErrUnauthorized{Capability: requested, Reason: "capability requires Tier 2 trust"}
	}
	if !c.HasCapability(requested) {
		return ErrUnauthorized{Capability: requested, Reason: "capability not granted to this token"}
	}
	return nil
}

// ErrUnauthorized is returned by host imports when capability
// authorization fails. The Reason field carries a human-readable
// explanation (mirrored to audit log).
type ErrUnauthorized struct {
	Capability string
	Reason     string
}

func (e ErrUnauthorized) Error() string {
	return fmt.Sprintf("unauthorized: capability %q: %s", e.Capability, e.Reason)
}

// IsErrUnauthorized reports whether err is an authorization failure.
func IsErrUnauthorized(err error) bool {
	var u ErrUnauthorized
	return errors.As(err, &u)
}
