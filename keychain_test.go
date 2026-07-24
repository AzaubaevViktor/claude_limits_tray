package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// exitErr returns a real *exec.ExitError carrying the given status, which is the
// only portable way to synthesise one.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatalf("expected a non-zero exit for code %d", code)
	}
	return err
}

// A refused read (the user dismissing the password dialog) must latch the gate
// so the poll loop stops re-prompting.
func TestKeychainGateLatchesAfterRefusal(t *testing.T) {
	calls := 0
	g := &keychainGate{run: func(ctx context.Context, args []string) ([]byte, error) {
		calls++
		return nil, exitErr(t, 128) // errSecUserCanceled
	}}

	if _, err := g.find("Codex", false); err == nil {
		t.Fatal("want an error from the refused read")
	}
	for i := 0; i < 3; i++ {
		_, err := g.find("Codex", false)
		if err == nil {
			t.Fatal("want an error while the gate is latched")
		}
		if !strings.Contains(err.Error(), "Use current Codex login") {
			t.Fatalf("error should point at the menu action, got %q", err)
		}
	}
	if calls != 1 {
		t.Fatalf("security ran %d times, want 1 — the gate must not re-prompt", calls)
	}
}

// "Item not found" never shows a dialog, so it must not latch the gate: the
// credential may appear later without the user having to touch the menu.
func TestKeychainGateIgnoresMissingItem(t *testing.T) {
	calls := 0
	g := &keychainGate{run: func(ctx context.Context, args []string) ([]byte, error) {
		calls++
		return nil, exitErr(t, keychainItemNotFound)
	}}
	for i := 0; i < 3; i++ {
		if _, err := g.find("Claude", false); err == nil {
			t.Fatal("want an error for a missing item")
		}
	}
	if blocked, _ := g.state(); blocked {
		t.Fatal("a missing item must not latch the gate")
	}
	if calls != 3 {
		t.Fatalf("security ran %d times, want 3", calls)
	}
}

// A menu action may prompt even after a refusal, and a success re-opens the gate.
func TestKeychainGateManualReadClearsLatch(t *testing.T) {
	fail := true
	g := &keychainGate{run: func(ctx context.Context, args []string) ([]byte, error) {
		if fail {
			return nil, exitErr(t, 128)
		}
		return []byte("{}"), nil
	}}
	if _, err := g.find("Codex", false); err == nil {
		t.Fatal("want an error from the refused read")
	}
	fail = false
	if _, err := g.find("Codex", true); err != nil {
		t.Fatalf("a manual read must run while latched: %v", err)
	}
	if blocked, _ := g.state(); blocked {
		t.Fatal("a successful read must re-open the gate")
	}
	if _, err := g.find("Codex", false); err != nil {
		t.Fatalf("automatic reads should work again: %v", err)
	}
}

// A dialog nobody answers must time out instead of wedging the poll goroutine,
// and it latches like any other failure.
func TestKeychainGateTimesOut(t *testing.T) {
	old := keychainReadTimeout
	keychainReadTimeout = 20 * time.Millisecond
	defer func() { keychainReadTimeout = old }()

	g := &keychainGate{run: func(ctx context.Context, args []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	_, err := g.find("Codex", false)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if blocked, _ := g.state(); !blocked {
		t.Fatal("a timed-out prompt must latch the gate")
	}
}

// The Codex bootstrap path must surface the latched reason rather than the
// generic "run codex login", and must not re-read the keychain each poll.
func TestCodexBootstrapRespectsGate(t *testing.T) {
	home := t.TempDir()
	calls := 0
	c := &CodexClient{CodexHome: home}
	c.kc.run = func(ctx context.Context, args []string) ([]byte, error) {
		calls++
		return nil, exitErr(t, 128)
	}
	t.Setenv(codexAccessEnvVar, "")

	for i := 0; i < 3; i++ {
		_, err := c.bootstrapCreds(false)
		if err == nil || !strings.Contains(err.Error(), "keychain access refused") {
			t.Fatalf("want the keychain reason, got %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("security ran %d times, want 1", calls)
	}
}
