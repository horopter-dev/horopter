// Package auth holds the machine-key format and the validation rules for the ref and
// locus identifiers.
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
// ok=false for anything that does not match the horo_<prefix>_<secret> shape.
func SplitKey(key string) (prefix, secret string, ok bool) {
	parts := strings.Split(key, "_")
	if len(parts) != 3 || parts[0] != keyBrand || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// VerifySecret reports whether secret hashes to hash, comparing in constant time.
func VerifySecret(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}
