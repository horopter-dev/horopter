package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONIsShorthandForFormatJSON(t *testing.T) {
	url := egress(t, changeA, "")
	_, want, _ := horo(t, url, true, "changes", "--format", "json")
	code, out, _ := horo(t, url, true, "changes", "--json")
	require.Equal(t, 0, code)
	assert.Equal(t, want, out)
}

func TestJSONWithFormatIsAUsageError(t *testing.T) {
	code, out, errOut := horo(t, egress(t, changeA, ""), true,
		"changes", "--json", "--format", "table")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errOut, "--json or --format, not both")
}

func TestAnUnknownFormatNamesTheChoices(t *testing.T) {
	code, _, errOut := horo(t, egress(t, changeA, ""), true, "show", "--format", "yaml", "ci/dep-1")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, `--format "yaml": use table, json or jsonl`)
}

func TestOutputIsGone(t *testing.T) {
	code, _, errOut := horo(t, egress(t, changeA, ""), true, "changes", "--output", "json")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "flag provided but not defined: -output")
}
