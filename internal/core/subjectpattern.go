package core

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ErrBadSubjectPattern reports a subject pattern that is malformed or too large.
var ErrBadSubjectPattern = errors.New("bad subject pattern")

// errReversedRange marks a class range whose low end is above its high end, such as [1-0].
// path.Match accepts one and never matches it; a regexp refuses it, and so does this, since a
// range that can match nothing is a mistake rather than a filter.
var errReversedRange = errors.New("reversed range")

const (
	maxSubjectPatternBytes = 1024
	maxSubjectAlternatives = 64
)

// SubjectPatterns is a set of subject globs, any of which may match: path.Match's syntax, as a
// credential's subject_pattern uses — * and ? stop at "/", [...] is a class, \ escapes — plus
// non-nesting {a,b} alternation, which Grafana's ${var:glob} emits for a multi-value variable.
// It is held as one anchored regular expression that Postgres's ~ and Go's regexp both read,
// so the store can match it and a test can check the translation against path.Match.
type SubjectPatterns struct{ regexp string }

// ParseSubjectPatterns parses each non-empty raw pattern and ORs them. An empty value is no
// filter, as for every other filter; all empty is the zero SubjectPatterns.
func ParseSubjectPatterns(raw []string) (SubjectPatterns, error) {
	var alts []string
	for _, p := range raw {
		if p == "" {
			continue
		}
		more, err := patternAlternatives(p)
		if err != nil {
			return SubjectPatterns{}, err
		}
		alts = append(alts, more...)
		if len(alts) > maxSubjectAlternatives {
			return SubjectPatterns{}, fmt.Errorf("%w: more than %d alternatives",
				ErrBadSubjectPattern, maxSubjectAlternatives)
		}
	}
	if len(alts) == 0 {
		return SubjectPatterns{}, nil
	}
	return SubjectPatterns{regexp: "^(" + strings.Join(alts, "|") + ")$"}, nil
}

// patternAlternatives validates one raw pattern and returns the regular expression for each
// of its brace alternatives.
func patternAlternatives(p string) ([]string, error) {
	if !utf8.ValidString(p) {
		return nil, fmt.Errorf("%w: not valid UTF-8", ErrBadSubjectPattern)
	}
	if len(p) > maxSubjectPatternBytes {
		return nil, fmt.Errorf("%w: longer than %d bytes",
			ErrBadSubjectPattern, maxSubjectPatternBytes)
	}
	expanded, err := expandBraces(p)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrBadSubjectPattern, p, err)
	}
	out := make([]string, 0, len(expanded))
	for _, e := range expanded {
		if _, err := path.Match(e, ""); err != nil {
			return nil, fmt.Errorf("%w %q: %w; patterns use path.Match syntax plus {a,b}",
				ErrBadSubjectPattern, p, err)
		}
		if err := checkRanges(e); err != nil {
			return nil, fmt.Errorf("%w %q: %w", ErrBadSubjectPattern, p, err)
		}
		out = append(out, globToRegexp(e))
	}
	return out, nil
}

// IsZero reports whether no pattern was given, which is no filter.
func (p SubjectPatterns) IsZero() bool { return p.regexp == "" }

// Regexp is the anchored regular expression the patterns compile to.
func (p SubjectPatterns) Regexp() string { return p.regexp }

// expandBraces expands every non-nested {a,b,...} group, left to right, into the cross product
// of its alternatives. A backslash escapes the next byte, so \{ is a literal brace. The product
// is bounded as it is built, so {a,b} repeated forty times is refused rather than expanded.
func expandBraces(p string) ([]string, error) {
	open, closing, err := firstBraceGroup(p)
	if err != nil || open < 0 {
		return []string{p}, err
	}
	rest, err := expandBraces(p[closing+1:])
	if err != nil {
		return nil, err
	}
	alts := strings.Split(p[open+1:closing], ",")
	if len(alts)*len(rest) > maxSubjectAlternatives {
		return nil, fmt.Errorf("more than %d alternatives", maxSubjectAlternatives)
	}
	out := make([]string, 0, len(alts)*len(rest))
	for _, alt := range alts {
		for _, r := range rest {
			out = append(out, p[:open]+alt+r)
		}
	}
	return out, nil
}

// firstBraceGroup returns the indexes of p's first unescaped '{' and its '}', or -1 for both
// when p has no group. A second '{' before the '}' is nesting, which is refused.
func firstBraceGroup(p string) (open, closing int, err error) {
	open = nextUnescapedBrace(p, 0)
	switch {
	case open < 0:
		return -1, -1, nil
	case p[open] == '}':
		return 0, 0, errors.New("'}' without '{'")
	}
	closing = nextUnescapedBrace(p, open+1)
	switch {
	case closing < 0:
		return 0, 0, errors.New("'{' without '}'")
	case p[closing] == '{':
		return 0, 0, errors.New("nested '{'")
	}
	return open, closing, nil
}

// nextUnescapedBrace returns the index of the first '{' or '}' in p at or after from that no
// backslash escapes, or -1.
func nextUnescapedBrace(p string, from int) int {
	for i := from; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '{', '}':
			return i
		}
	}
	return -1
}

// globToRegexp translates a path.Match pattern, already validated, to a regular expression.
// It works bytewise: only ASCII bytes are special, and every other byte is copied as it is,
// so a multi-byte rune passes through intact.
func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '\\':
			i++
			writeLiteral(&b, g[i])
		case '[':
			i = writeClass(&b, g, i)
		default:
			writeLiteral(&b, c)
		}
	}
	return b.String()
}

// writeClass copies the class starting at g[i] and returns the index of its closing ']'.
// Inside it every ASCII punctuation byte is escaped, escaped or not in the glob, except an
// unescaped '-', which is path.Match's range operator. That is what keeps [[:digit:]] the
// literal set path.Match reads it as — copied bare, a regexp reads a POSIX class, and Postgres
// also reads [. and [= as collating openers. An escaped letter becomes the bare letter, since
// \a or \d mean something else to a regexp. Like path.Match, a class may match "/".
func writeClass(b *strings.Builder, g string, i int) int {
	b.WriteByte('[')
	i++
	if g[i] == '^' {
		b.WriteByte('^')
		i++
	}
	for ; g[i] != ']'; i++ {
		switch c := g[i]; c {
		case '\\':
			i++
			writeLiteral(b, g[i])
		case '-':
			b.WriteByte('-')
		default:
			writeLiteral(b, c)
		}
	}
	b.WriteByte(']')
	return i
}

func writeLiteral(b *strings.Builder, c byte) {
	if isASCIIPunct(c) {
		b.WriteByte('\\')
	}
	b.WriteByte(c)
}

func isASCIIPunct(c byte) bool {
	return c < 0x80 && strings.IndexByte("\\.+*?()|[]{}^$-/#&~", c) >= 0
}

// checkRanges refuses a reversed range in any class of g, a pattern path.Match has already
// validated, so every class is closed and every escape has its byte. Range ends are compared as
// runes, as path.Match compares them.
func checkRanges(g string) error {
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '\\':
			i++
		case '[':
			end, err := checkClass(g, i+1)
			if err != nil {
				return err
			}
			i = end
		}
	}
	return nil
}

// checkClass scans the class body starting at g[i] and returns the index of its closing ']'.
func checkClass(g string, i int) (int, error) {
	if g[i] == '^' {
		i++
	}
	for g[i] != ']' {
		lo, n := classRune(g, i)
		i += n
		if g[i] != '-' {
			continue
		}
		hi, m := classRune(g, i+1)
		if hi < lo {
			return 0, fmt.Errorf("%w %q", errReversedRange, g[i-n:i+1+m])
		}
		i += 1 + m
	}
	return i, nil
}

// classRune decodes the possibly escaped rune at g[i] and the bytes it occupies.
func classRune(g string, i int) (rune, int) {
	if g[i] == '\\' {
		r, n := utf8.DecodeRuneInString(g[i+1:])
		return r, 1 + n
	}
	return utf8.DecodeRuneInString(g[i:])
}
