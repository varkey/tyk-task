package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNamespaces(t *testing.T) {
	cases := map[string][]string{
		"":              nil,
		"ns-a":          {"ns-a"},
		"ns-a,ns-b":     {"ns-a", "ns-b"},
		" ns-a , ns-b ": {"ns-a", "ns-b"},
		"ns-a,,ns-b":    {"ns-a", "ns-b"},
		",":             nil,
		"ns-a,ns-b,":    {"ns-a", "ns-b"},
	}

	for in, want := range cases {
		assert.Equal(t, want, parseNamespaces(in), "input %q", in)
	}
}
