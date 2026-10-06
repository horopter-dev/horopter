package core

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func matches(t *testing.T, raw []string, subject string) bool {
	t.Helper()
	p, err := ParseSubjectPatterns(raw)
	require.NoError(t, err)
	return regexp.MustCompile(p.Regexp()).MatchString(subject)
}

func TestSubjectPatternsMatch(t *testing.T) {
	for _, tc := range []struct {
		raw     []string
		subject string
		want    bool
	}{
		{[]string{"*/lastlight/*"}, "Deployment/lastlight/cloudflared", true},
		{[]string{"*/lastlight/*"}, "Deployment/logging/vector", false},
		{[]string{"*/lastlight/*"}, "Namespace/lastlight", false},
		{[]string{"*"}, "a/b", false},
		{[]string{"HelmRelease/*/*"}, "HelmRelease/monitoring/kube-prometheus-stack", true},
		{[]string{"*/{lastlight,logging}/*"}, "Deployment/logging/vector", true},
		{[]string{"*/{lastlight,logging}/*"}, "Deployment/monitoring/x", false},
		{[]string{"*/lastlight/*", "*/logging/*"}, "Deployment/logging/vector", true},
		{[]string{"a.b"}, "a.b", true},
		{[]string{"a.b"}, "axb", false},
		{[]string{"a+b"}, "a+b", true},
		{[]string{"(x)"}, "(x)", true},
		{[]string{"a|b"}, "a", false},
		{[]string{`\*`}, "*", true},
		{[]string{"x?z"}, "xyz", true},
		{[]string{"[a-c]"}, "b", true},
		{[]string{"[[:digit:]]"}, "1", false},
		{[]string{"[[:digit:]]"}, "d]", true},
		{[]string{"[.a]"}, ".", true},
		{[]string{"acme/prod"}, "acme/prod", true},
		{[]string{"acme/prod"}, "acme/prod2", false},
	} {
		assert.Equal(t, tc.want, matches(t, tc.raw, tc.subject), "%v ~ %q", tc.raw, tc.subject)
	}
}

func TestSubjectPatternsEmptyIsNoFilter(t *testing.T) {
	for _, raw := range [][]string{nil, {""}, {"", ""}} {
		p, err := ParseSubjectPatterns(raw)
		require.NoError(t, err)
		assert.True(t, p.IsZero(), "%q", raw)
	}
	assert.True(t, matches(t, []string{"", "*/a/*"}, "K/a/n"),
		"an empty value beside a real one is ignored")
}

func TestSubjectPatternsRefuseMalformedAndPathological(t *testing.T) {
	for name, raw := range map[string][]string{
		"unclosed class":        {"["},
		"trailing escape":       {`a\`},
		"unclosed brace":        {"{a,b"},
		"stray close":           {"a}"},
		"nested braces":         {"{a,{b,c}}"},
		"too long":              {strings.Repeat("a", maxSubjectPatternBytes+1)},
		"too many alternatives": {strings.Repeat("{a,b}", 8)},
		"exponential braces":    {strings.Repeat("{a,b}", 40)},
		"invalid UTF-8":         {"\x80"},
		"reversed range":        {"[1-0]"},
	} {
		_, err := ParseSubjectPatterns(raw)
		assert.True(t, errors.Is(err, ErrBadSubjectPattern), "%s: %v", name, err)
	}
}

// FuzzSubjectPatternAgreesWithPathMatch makes path.Match the oracle for every brace-free
// pattern: the translation must accept exactly what the credential matcher accepts.
func FuzzSubjectPatternAgreesWithPathMatch(f *testing.F) {
	for _, s := range [][2]string{{"*/a/*", "K/a/n"}, {"[^a]", "/"}, {`\*`, "*"}, {"a.b", "axb"},
		{"[[:digit:]]", "d]"}, {"[=a=]", "="}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(agreesWithPathMatch)
}

// outsideTheOracle reports inputs path.Match cannot judge: braces are this package's
// extension, invalid UTF-8 is refused before translation, and path.Match's * backtracks
// through a subject byte by byte, so against a multi-byte rune it can match ? to a fragment of
// one ("*??" matches the single rune U+16E16). The regexp matches by rune, which is right.
func outsideTheOracle(pattern, subject string) bool {
	return strings.ContainsAny(pattern, "{}") || pattern == "" || subject == "" ||
		!utf8.ValidString(pattern) || (strings.Contains(pattern, "*") && !isASCII(subject))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func agreesWithPathMatch(t *testing.T, pattern, subject string) {
	if outsideTheOracle(pattern, subject) {
		t.Skip()
	}
	want, err := path.Match(pattern, subject)
	p, perr := ParseSubjectPatterns([]string{pattern})
	if err != nil {
		require.Error(t, perr, "path.Match refuses %q", pattern)
		return
	}
	// Stricter than path.Match on purpose: too long, or a range that can never match.
	if perr != nil && (len(pattern) > maxSubjectPatternBytes || errors.Is(perr, errReversedRange)) {
		return
	}
	require.NoError(t, perr, "path.Match accepts %q", pattern)
	re, cerr := regexp.Compile(p.Regexp())
	require.NoError(t, cerr, "translation of %q is not a valid regexp: %s", pattern, p.Regexp())
	assert.Equal(t, want, re.MatchString(subject), "%q ~ %q via %s", pattern, subject, p.Regexp())
}

// TestSubjectPatternsCollapseStarRuns: ** means what * means within a segment, and a run of
// stars is what made Postgres spend seconds compiling one pattern.
func TestSubjectPatternsCollapseStarRuns(t *testing.T) {
	many, err := ParseSubjectPatterns([]string{"a" + strings.Repeat("*", 500) + "b"})
	require.NoError(t, err)
	one, err := ParseSubjectPatterns([]string{"a*b"})
	require.NoError(t, err)
	assert.Equal(t, one.Regexp(), many.Regexp())
}

// TestSubjectPatternsRefuseARegexpPostgresWouldNotCompile: every alternative is within the
// per-pattern limits, but together they would exceed Postgres's regex complexity limit, which
// it reports as an error the caller would see as a 500.
func TestSubjectPatternsRefuseARegexpPostgresWouldNotCompile(t *testing.T) {
	_, err := ParseSubjectPatterns([]string{strings.Repeat("{a,b}", 6) + strings.Repeat("x", 900)})
	assert.ErrorIs(t, err, ErrBadSubjectPattern)
}
