package main

import (
	"strings"
	"testing"
)

// TestGeneratePassword pins the strength contract: 256 bits of
// entropy, hex-encoded → 64 chars. A regression that drops the
// length or switches to math/rand would silently weaken every
// new redis password.
func TestGeneratePassword(t *testing.T) {
	pwd, err := generatePassword()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(pwd) != passwordEntropyBytes*2 {
		t.Errorf("len=%d want %d (hex of %d bytes)", len(pwd), passwordEntropyBytes*2, passwordEntropyBytes)
	}

	for _, r := range pwd {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("non-hex char %q in password", r)
			break
		}
	}

	// Two consecutive calls must produce different passwords —
	// the source is crypto/rand. A fluke collision is
	// astronomically unlikely; if this ever fails, the
	// generator is broken (not random).
	other, _ := generatePassword()
	if pwd == other {
		t.Errorf("two generations returned the same password — randomness broken")
	}
}

// TestResolveOrGeneratePassword_ReusesExisting: if the bucket
// has REDIS_PASSWORD, expand reuses it (idempotent). Critical
// — without this the password would change on every apply,
// invalidating every linked consumer's URL.
func TestResolveOrGeneratePassword_ReusesExisting(t *testing.T) {
	cfg := map[string]string{
		"REDIS_PASSWORD": "existing-password-from-prior-apply",
	}

	got, isNew, err := resolveOrGeneratePassword(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got != "existing-password-from-prior-apply" {
		t.Errorf("got %q, want existing", got)
	}

	if isNew {
		t.Error("isNew should be false when password already exists")
	}
}

// TestResolveOrGeneratePassword_GeneratesWhenAbsent: first
// apply has empty config; expand generates + flags isNew=true
// so the caller emits a config_set action to persist.
func TestResolveOrGeneratePassword_GeneratesWhenAbsent(t *testing.T) {
	cfg := map[string]string{}

	got, isNew, err := resolveOrGeneratePassword(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got == "" {
		t.Error("password should not be empty")
	}

	if !isNew {
		t.Error("isNew should be true on fresh generation")
	}
}

// TestResolveOrGeneratePassword_TreatsEmptyAsAbsent: an
// explicitly-empty REDIS_PASSWORD is treated as "not set" —
// generates a fresh one. Operators who genuinely want no
// auth must `vd config unset` the key, not blank it.
func TestResolveOrGeneratePassword_TreatsEmptyAsAbsent(t *testing.T) {
	cfg := map[string]string{"REDIS_PASSWORD": ""}

	got, isNew, err := resolveOrGeneratePassword(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got == "" {
		t.Error("should have generated a password")
	}

	if !isNew {
		t.Error("isNew should be true for empty-string config value")
	}
}

// TestAppendRequirepass_AppendedToEnd: the directive lands as
// the LAST line so redis's "last wins" override semantics make
// it authoritative even if the upstream conf already had one.
func TestAppendRequirepass_AppendedToEnd(t *testing.T) {
	conf := []byte("port 6379\nbind 0.0.0.0\n")
	out := appendRequirepass(conf, "secret123")

	s := string(out)

	if !strings.HasSuffix(s, "requirepass secret123\n") {
		t.Errorf("requirepass should be the last line:\n%s", s)
	}

	// The original directives must remain.
	if !strings.Contains(s, "port 6379") || !strings.Contains(s, "bind 0.0.0.0") {
		t.Errorf("original directives lost:\n%s", s)
	}

	// Marker comment helps operators understand provenance.
	if !strings.Contains(s, "voodu-redis plugin") {
		t.Errorf("missing managed-by marker:\n%s", s)
	}
}

// TestAppendRequirepass_HandlesMissingTrailingNewline: some
// get-conf scripts forget the final \n. The helper inserts one
// before the directive so we don't accidentally produce
// `bind 0.0.0.0requirepass …` on the same line.
func TestAppendRequirepass_HandlesMissingTrailingNewline(t *testing.T) {
	conf := []byte("port 6379\nbind 0.0.0.0") // no trailing newline
	out := appendRequirepass(conf, "x")

	s := string(out)

	if strings.Contains(s, "bind 0.0.0.0requirepass") {
		t.Errorf("requirepass should be on its own line:\n%s", s)
	}

	if !strings.HasSuffix(s, "requirepass x\n") {
		t.Errorf("should still end with the directive:\n%s", s)
	}
}

// TestAppendRequirepass_NoInlineCommentsOnDirective: redis 7+
// rejects `requirepass foo # comment` (parses # as part of the
// password). Pin that the helper never emits inline comments
// on the directive line itself — comment goes ABOVE.
func TestAppendRequirepass_NoInlineCommentsOnDirective(t *testing.T) {
	conf := []byte("port 6379\n")
	out := appendRequirepass(conf, "secret")

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "requirepass ") {
			if strings.Contains(line, "#") {
				t.Errorf("requirepass line has inline comment (redis 7+ would reject): %q", line)
			}
		}
	}
}
