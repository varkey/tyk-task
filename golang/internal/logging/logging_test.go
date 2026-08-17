package logging

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":   LevelDebug,
		"DEBUG":   LevelDebug,
		" info":   LevelInfo,
		"warn":    LevelWarn,
		"WARNING": LevelWarn,
		"error":   LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	_, err := ParseLevel("verbose")
	assert.Error(t, err)
}

func TestLevelGating(t *testing.T) {
	t.Cleanup(func() { SetLevel(LevelInfo) }) // restore the package default

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	SetLevel(LevelInfo)
	assert.False(t, Enabled(LevelDebug))
	assert.True(t, Enabled(LevelInfo))

	Debugf("should not appear")
	Infof("should appear")
	assert.NotContains(t, buf.String(), "should not appear")
	assert.Contains(t, buf.String(), "should appear")

	buf.Reset()
	SetLevel(LevelDebug)
	assert.True(t, Enabled(LevelDebug))
	Debugf("now visible")
	assert.True(t, strings.Contains(buf.String(), "now visible"))
}
