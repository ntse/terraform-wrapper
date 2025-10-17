package output

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	fn()

	require.NoError(t, w.Close())
	os.Stdout = orig

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)

	return buf.String()
}

func TestManagerLifecycle(t *testing.T) {
	m := NewManager()
	m.Register("stack")

	dur, logs := runWithCapture(t, func() {
		m.Start("stack")
		time.Sleep(10 * time.Millisecond)
		m.Succeed("stack")
	})

	require.InDelta(t, 0.01, dur.Seconds(), 0.01)
	require.Contains(t, logs, "[run] stack")
	require.Contains(t, logs, "[done] stack")
}

func TestManagerWaitingAndSkip(t *testing.T) {
	m := NewManager()
	m.Register("stack")
	logs := captureStdout(t, func() {
		m.Waiting("stack", "deps")
		m.Skip("stack", "cache hit")
	})

	require.Contains(t, logs, "[wait] stack (deps)")
	require.Contains(t, logs, "[skip] stack (cache hit)")
}

func TestManagerFail(t *testing.T) {
	m := NewManager()
	m.Register("stack")
	logs := captureStdout(t, func() {
		m.Start("stack")
		m.Fail("stack", errors.New("boom"))
	})

	require.Contains(t, logs, "[fail] stack")
	require.Contains(t, logs, "boom")
}

func runWithCapture(t *testing.T, fn func()) (time.Duration, string) {
	t.Helper()

	start := time.Now()
	logs := captureStdout(t, fn)
	return time.Since(start), logs
}
