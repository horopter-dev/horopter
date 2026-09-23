package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"horopter.dev/horopter/internal/auth"
)

func TestValidateRef(t *testing.T) {
	for name, tc := range map[string]struct {
		ref     string
		wantErr bool
	}{
		"plain":            {"github/deploy-1", false},
		"interior newline": {"a\nb", false},
		"unicode":          {"déploy/日本語", false},
		"dot dot":          {"legacy/../etc", false},
		"trailing newline": {"deploy-1\n", true},
		"leading space":    {" deploy-1", true},
		"trailing space":   {"deploy-1 ", true},
		"null byte":        {"deploy\x00", true},
		"empty":            {"", true},
		"at the cap":       {strings.Repeat("x", auth.MaxRefBytes), false},
		"one over the cap": {strings.Repeat("x", auth.MaxRefBytes+1), true},
	} {
		t.Run(name, func(t *testing.T) {
			err := auth.ValidateRef(tc.ref)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateLocusAlsoRejectsSlash(t *testing.T) {
	assert.NoError(t, auth.ValidateLocus("github"))
	assert.Error(t, auth.ValidateLocus("github/actions"))
	assert.Error(t, auth.ValidateLocus(""))
}
