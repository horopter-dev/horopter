// Package auth holds the machine-key format, ref and locus validation, and the auth middleware.
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

// MaxHeaderBytes bounds both daemons' headers; Go's 1 MiB default let a huge secret cost a SHA-256.
const MaxHeaderBytes = 16 << 10

// SHA-256, not a memory-hard hash: the secret is 32 random bytes and this runs on every request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NewKey generates a horo_<prefix>_<secret> machine key with its lookup prefix and stored hash.
func NewKey(entropy io.Reader) (key, prefix, hash string, err error) {
	raw := make([]byte, prefixBytes+secretBytes)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", "", "", fmt.Errorf("read entropy for credential: %w", err)
	}
	prefix = hex.EncodeToString(raw[:prefixBytes])
	secret := hex.EncodeToString(raw[prefixBytes:])
	return keyBrand + "_" + prefix + "_" + secret, prefix, hashSecret(secret), nil
}

// SplitKey splits a presented key into prefix and secret, refusing any prefix NewKey cannot emit.
func SplitKey(key string) (prefix, secret string, ok bool) {
	parts := strings.Split(key, "_")
	if len(parts) != 3 || parts[0] != keyBrand || !isLookupPrefix(parts[1]) || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// The prefix is a cache key, so its length must not be the caller's to choose.
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

// HashSecret hashes secret as NewKey does, for horoctl's operator-supplied --key.
func HashSecret(secret string) string { return hashSecret(secret) }
