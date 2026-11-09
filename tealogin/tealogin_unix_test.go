//go:build unix

package tealogin

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// A concurrent process refreshing the same login must win exactly once:
// while the test holds tea's config lock, Token blocks; the test then
// persists a fresh token (as tea would) and releases the lock — Token's
// double-check adopts it instead of spending the rotated refresh token on
// a second refresh call.
func TestTokenDoubleCheckUnderLock(t *testing.T) {
	home := configHome(t)
	s, srv := newTokenServer(t)
	path := writeConfig(t, home, "tea", oauthLogin(srv.URL, "old-tok", "old-refresh", time.Now().Add(-time.Hour)))

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := Token(context.Background(), srv.URL)
		done <- result{tok, err}
	}()

	// Token must be blocked on the lock; give it a moment to get there,
	// then refresh the file the way a concurrent tea would and unlock.
	select {
	case r := <-done:
		t.Fatalf("Token returned %+v without waiting for the lock", r)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(path, []byte(oauthLogin(srv.URL, "race-tok", "race-refresh", time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil || r.token != "race-tok" {
		t.Fatalf("got %q, %v; want race-tok, nil", r.token, r.err)
	}
	if len(s.requests) != 0 {
		t.Errorf("refresh requests = %d, want 0 (other process already refreshed)", len(s.requests))
	}
}
