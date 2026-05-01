package main

import (
	"strings"
	"testing"
)

// TestRenderEntrypointScript_ScopedHostFQDN pins the most
// important contract of the wrapper: the master FQDN baked
// into the script matches voodu0's per-pod alias convention.
// A bug here would have replicas dialing the wrong host and
// silently failing to sync, producing what looks like a master-
// only deployment with N copies of stale data.
func TestRenderEntrypointScript_ScopedHostFQDN(t *testing.T) {
	got := renderEntrypointScript("clowk-lp", "redis")

	// Master FQDN must use ${MASTER_ORDINAL} as a shell var
	// (not the literal scope/name) so a flip from
	// REDIS_MASTER_ORDINAL=0 to =1 (failover) is honored at
	// boot without a re-render.
	want := `MASTER_HOST=redis-${MASTER_ORDINAL}.clowk-lp.voodu`
	if !strings.Contains(got, want) {
		t.Errorf("rendered script missing %q\n--- script ---\n%s", want, got)
	}

	// The role decision branches on VOODU_REPLICA_ORDINAL.
	// Without this branch, every pod runs as master and
	// replication never wires up.
	if !strings.Contains(got, `ORDINAL="${VOODU_REPLICA_ORDINAL:-0}"`) {
		t.Errorf("missing ordinal read:\n%s", got)
	}

	// REDIS_MASTER_ORDINAL drives failover; default 0 means
	// pod-0 is the master out of the box.
	if !strings.Contains(got, `MASTER_ORDINAL="${REDIS_MASTER_ORDINAL:-0}"`) {
		t.Errorf("missing master-ordinal read:\n%s", got)
	}

	// Master branch: bare `redis-server $CONF`. Replica branch:
	// adds --replicaof. Both must be present for a single
	// rendered script to serve both roles.
	if !strings.Contains(got, `exec redis-server "$CONF"`) {
		t.Errorf("missing master exec line:\n%s", got)
	}

	if !strings.Contains(got, `exec redis-server "$CONF" --replicaof "$MASTER_HOST" 6379`) {
		t.Errorf("missing replica exec line:\n%s", got)
	}
}

// TestRenderEntrypointScript_UnscopedHostFQDN: scopeless
// statefulsets (rare but legal) must produce `<name>-N.voodu`,
// not `<name>-N..voodu` (double dot from a bare scope) or
// `<name>-N.voodu` with extra leading dots.
func TestRenderEntrypointScript_UnscopedHostFQDN(t *testing.T) {
	got := renderEntrypointScript("", "cache")

	want := `MASTER_HOST=cache-${MASTER_ORDINAL}.voodu`
	if !strings.Contains(got, want) {
		t.Errorf("rendered script missing %q\n--- script ---\n%s", want, got)
	}

	if strings.Contains(got, "..voodu") {
		t.Errorf("double-dot in FQDN — scope handling bug:\n%s", got)
	}
}

// TestRenderEntrypointScript_Deterministic: same inputs always
// produce identical bytes, so the asset digest stays stable
// across replays. A non-deterministic render would re-emit the
// asset on every apply, causing spurious rolling restarts.
func TestRenderEntrypointScript_Deterministic(t *testing.T) {
	a := renderEntrypointScript("clowk-lp", "redis")
	b := renderEntrypointScript("clowk-lp", "redis")

	if a != b {
		t.Errorf("renderEntrypointScript not deterministic — asset digest will churn")
	}
}
