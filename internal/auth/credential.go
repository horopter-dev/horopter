// Package auth holds the machine-key format, the validation rules for the ref and locus
// identifiers, and the middleware that resolves a presented key into a Principal and
// gates a route on what that principal's role can do.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const (
	keyBrand    = "horo"
	prefixBytes = 6
	secretBytes = 32
)

// MaxHeaderBytes is what both daemons set on http.Server.MaxHeaderBytes: it bounds the
// request line plus headers, Authorization included.
//
// It lives here rather than in either main because what it bounds is this package's hot path.
// Go's 1 MiB default is what made an oversized Authorization header worth sending: SplitKey
// bounds the lookup prefix, but the secret half is only length-checked by a constant-time
// compare, so a large header still costs one SHA-256 before rejection. 16 KiB comfortably fits
// a real request's headers — the Authorization header itself is under 100 bytes — with
// headroom for whatever a proxy in front of a daemon adds. Two copies of this number could
// drift so that one daemon's bound quietly stopped matching the other's.
const MaxHeaderBytes = 16 << 10

// hashSecret uses SHA-256 rather than a memory-hard hash: the secret is 32 bytes of
// server-generated randomness, and this runs on every request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NewKey generates a machine key of the form horo_<prefix>_<secret>, returning the full
// key, its lookup prefix, and the stored hash of its secret.
func NewKey(entropy io.Reader) (key, prefix, hash string, err error) {
	raw := make([]byte, prefixBytes+secretBytes)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", "", "", fmt.Errorf("read entropy for credential: %w", err)
	}
	prefix = hex.EncodeToString(raw[:prefixBytes])
	secret := hex.EncodeToString(raw[prefixBytes:])
	return keyBrand + "_" + prefix + "_" + secret, prefix, hashSecret(secret), nil
}

// SplitKey separates a presented key into its lookup prefix and secret, reporting
// ok=false for anything that does not match the horo_<prefix>_<secret> shape, including
// a prefix NewKey could not have produced.
func SplitKey(key string) (prefix, secret string, ok bool) {
	parts := strings.Split(key, "_")
	if len(parts) != 3 || parts[0] != keyBrand || !isLookupPrefix(parts[1]) || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// isLookupPrefix reports whether s is exactly the prefix NewKey emits. The length is
// checked rather than assumed because the prefix becomes a cache key: accepting a longer
// one would let an unauthenticated caller choose how many bytes each cached entry costs.
func isLookupPrefix(s string) bool {
	if len(s) != prefixBytes*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// VerifySecret reports whether secret hashes to hash, comparing in constant time.
func VerifySecret(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}

// HashSecret hashes secret the way NewKey's returned hash was produced. It exists for a
// caller storing a credential whose key it did not generate — horoctl's --key accepts an
// operator-supplied value for development — so that secret is hashed identically to one
// NewKey issued, rather than the algorithm being reimplemented at the call site.
func HashSecret(secret string) string { return hashSecret(secret) }
