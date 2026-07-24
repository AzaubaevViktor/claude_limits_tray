package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

// security(1) exits with the OSStatus it hit. 44 is errSecItemNotFound: the
// item simply isn't stored, so no dialog appeared and nothing was refused.
const keychainItemNotFound = 44

// An unanswered password dialog must not wedge the poll goroutine forever.
var keychainReadTimeout = 90 * time.Second

// keychainGate keeps macOS's "enter your keychain password" dialog out of the
// polling loop. Reading a credential item can pop that dialog, and if the user
// dismisses it the next poll pops it again, every interval, forever. So a read
// that fails for any reason other than "no such item" latches the gate shut:
// automatic reads are refused from then on, and only an explicit menu action
// ("Use current … login") may prompt again.
type keychainGate struct {
	mu      sync.Mutex
	blocked bool
	reason  string
	run     func(ctx context.Context, args []string) ([]byte, error) // nil = real security(1)
}

// find reads a generic password item. Automatic reads (the poller) are refused
// while the gate is latched; manual reads (a menu click) always run, and a
// successful one re-opens the gate.
func (g *keychainGate) find(label string, manual bool, args ...string) ([]byte, error) {
	if !manual {
		if blocked, reason := g.state(); blocked {
			return nil, &APIError{Msg: reason}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainReadTimeout)
	defer cancel()

	out, err := g.exec(ctx, append([]string{"find-generic-password"}, args...))
	if err == nil {
		g.open()
		return out, nil
	}
	if exitStatus(err) == keychainItemNotFound {
		// Nothing is stored under that name — no dialog, nothing to latch.
		return nil, &APIError{Msg: fmt.Sprintf("No %s credentials in the keychain", label)}
	}
	reason := fmt.Sprintf("%s keychain access refused — use \"Use current %s login\"", label, label)
	if ctx.Err() != nil {
		reason = fmt.Sprintf("%s keychain prompt timed out — use \"Use current %s login\"", label, label)
	}
	g.latch(reason)
	log.Printf("%s keychain read failed (%v) — no further prompts until asked from the menu", label, err)
	return nil, &APIError{Msg: reason}
}

func (g *keychainGate) exec(ctx context.Context, args []string) ([]byte, error) {
	if g.run != nil {
		return g.run(ctx, args)
	}
	return exec.CommandContext(ctx, "security", args...).Output()
}

func (g *keychainGate) state() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blocked, g.reason
}

func (g *keychainGate) latch(reason string) {
	g.mu.Lock()
	g.blocked, g.reason = true, reason
	g.mu.Unlock()
}

func (g *keychainGate) open() {
	g.mu.Lock()
	g.blocked, g.reason = false, ""
	g.mu.Unlock()
}

// exitStatus returns the process exit code behind err, or -1 if err isn't one.
func exitStatus(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
