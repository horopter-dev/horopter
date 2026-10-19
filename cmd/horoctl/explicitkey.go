package main

import (
	"fmt"

	"horopter.dev/horopter/internal/auth"
)

// minExplicitKeyLength refuses a placeholder secret; auth.SplitKey checks only the shape.
const minExplicitKeyLength = 32

const explicitKeyHelp = "explicit key value in the form horo_<12-char-hex-prefix>_<secret> " +
	"(the shape auth.SplitKey requires of a presented credential), development only: " +
	"never use this in production"

// parseExplicitKey uses auth.SplitKey, refusing any key that could never authenticate.
func parseExplicitKey(key string) (prefix, secret string, err error) {
	if len(key) < minExplicitKeyLength {
		return "", "", fmt.Errorf(
			"--key is %d bytes; development keys must be at least %d bytes long",
			len(key), minExplicitKeyLength)
	}
	prefix, secret, ok := auth.SplitKey(key)
	if !ok {
		return "", "", fmt.Errorf(
			"--key does not match horo_<12-char-hex-prefix>_<secret>, the shape " +
				"auth.SplitKey requires of a presented credential; anything else would " +
				"store a credential that can never authenticate")
	}
	return prefix, secret, nil
}
