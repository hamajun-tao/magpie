package testenv

import (
	"os/exec"
	"sync"
	"testing"
)

// bunsRun holds a *sync.Once for each bun Bun has run.
var bunsRun sync.Map

// Bun is the bun on PATH for a test that runs plugins with it (as
// MAGPIE_BUN), run once before the test opens its deadline; a machine
// without one skips the test. A bun just installed or upgraded hasn't run
// on this machine yet, and macOS checks a program before its first exec,
// which can take tens of seconds on a busy machine: that check must not eat
// the minute a test gives its plugin calls (ggbdpq, #1306).
func Bun(t testing.TB) string {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("no bun on PATH")
	}
	once, _ := bunsRun.LoadOrStore(bun, new(sync.Once))
	once.(*sync.Once).Do(func() { exec.Command(bun, "--version").Run() })
	return bun
}
