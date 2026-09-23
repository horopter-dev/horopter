package main

import (
	"fmt"

	"horopter.dev/horopter/internal/auth"
)

// minExplicitKeyLength is the floor --key and bootstrap's --producer-key/--reader-key
// enforce, on top of auth.SplitKey's own shape check below. SplitKey requires only a
// non-empty secret, so this is what stops a trivially short, guessable one from passing:
// well below a generated key's 82 characters, but long enough that a placeholder like
// "test" or "password" cannot satisfy it.
const minExplicitKeyLength = 32

// explicitKeyHelp is shared verbatim by every flag that accepts a caller-chosen key, so
// neither the format requirement nor the "development only" warning is ever missing from
// one of them by omission.
const explicitKeyHelp = "explicit key value in the form horo_<12-char-hex-prefix>_<secret> " +
	"(the shape auth.SplitKey requires of a presented credential), development only: " +
	"never use this in production"

// parseExplicitKey validates an operator-supplied key against auth.SplitKey — the same
// parser a presented credential goes through at request time — rather than a looser rule
// of its own. A key SplitKey would refuse must be refused here too: stored anyway, it
// would insert cleanly and print successfully, and only fail, silently, the first time
// someone tried to use it — a dead credential that costs an operator a debugging session
// to explain, not a security hole, but a real cost for a check this cheap to share.
func parseExplicitKey(key string) (prefix, secret string, err error) {
	if len(key) < minExplicitKeyLength {
		return "", "", fmt.Errorf(
			"--key is %d bytes; development keys must be at least %d bytes long",
			len(key), minExplicitKeyLength)
	}
	prefix, secret, ok := auth.SplitKey(key)
	if !ok {
		return "", "", fmt.Errorf(
			"--key %q does not match horo_<12-char-hex-prefix>_<secret>, the shape "+
				"auth.SplitKey requires of a presented credential; anything else would "+
				"store a credential that can never authenticate", key)
	}
	return prefix, secret, nil
}
