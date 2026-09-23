package main

import (
	"fmt"
	"strings"
)

// minExplicitKeyLength is the floor --key and bootstrap's --producer-key/--reader-key
// enforce. It is well below a generated key's 82 characters, but long enough that a
// placeholder like "test" or "password" cannot satisfy it.
const minExplicitKeyLength = 32

// explicitKeyHelp is shared verbatim by every flag that accepts a caller-chosen key, so
// the "development only" warning is never missing from one of them by omission.
const explicitKeyHelp = "explicit key value in the form horo_<prefix>_<secret>, " +
	"development only: never use this in production"

// parseExplicitKey splits an operator-supplied key into its prefix and secret. Unlike
// auth.SplitKey, it does not require the prefix to be hex: an explicit key is chosen for
// memorability (docker-compose.yml, hack/seed.sh), not generated, so it need not look like
// one NewKey could have produced.
func parseExplicitKey(key string) (prefix, secret string, err error) {
	if len(key) < minExplicitKeyLength {
		return "", "", fmt.Errorf(
			"--key is %d bytes; development keys must be at least %d bytes long",
			len(key), minExplicitKeyLength)
	}
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 || parts[0] != "horo" || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("--key %q is not of the form horo_<prefix>_<secret>", key)
	}
	return parts[1], parts[2], nil
}
