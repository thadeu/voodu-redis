package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestRedisReplicas pins the spec parser. The plugin emits
// replicas = 1 by default; operators raising it to 3 must see
// the URL builder (and topology display) read 3, not 1, not 0,
// not -1. JSON unmarshal produces float64 for numbers; the
// parser must handle both float64 and int (from synthetic
// test inputs).
func TestRedisReplicas(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want int
	}{
		{"nil spec defaults to 1", nil, 1},
		{"missing key defaults to 1", map[string]any{}, 1},
		{"float64 (json)", map[string]any{"replicas": float64(3)}, 3},
		{"int (synthetic)", map[string]any{"replicas": 3}, 3},
		{"zero clamps to 1 (matches controller's clamp)", map[string]any{"replicas": float64(0)}, 1},
		{"negative clamps to 1", map[string]any{"replicas": float64(-2)}, 1},
		{"unknown type falls through to 1", map[string]any{"replicas": "many"}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redisReplicas(tc.spec); got != tc.want {
				t.Errorf("redisReplicas(%+v) = %d, want %d", tc.spec, got, tc.want)
			}
		})
	}
}

// TestRedisMasterOrdinal: REDIS_MASTER_ORDINAL is the failover
// flip knob. Default 0 (pod-0 is master) MUST agree with the
// wrapper script's fallback — otherwise URL emission and the
// entrypoint disagree about who's the master and the linked
// consumer dials a non-master.
func TestRedisMasterOrdinal(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
		want   int
	}{
		{"nil config", nil, 0},
		{"missing key", map[string]any{}, 0},
		{"empty string", map[string]any{"REDIS_MASTER_ORDINAL": ""}, 0},
		{"valid 1", map[string]any{"REDIS_MASTER_ORDINAL": "1"}, 1},
		{"valid 2", map[string]any{"REDIS_MASTER_ORDINAL": "2"}, 2},
		{"non-numeric falls through", map[string]any{"REDIS_MASTER_ORDINAL": "primary"}, 0},
		{"negative falls through", map[string]any{"REDIS_MASTER_ORDINAL": "-1"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redisMasterOrdinal(tc.config); got != tc.want {
				t.Errorf("redisMasterOrdinal(%+v) = %d, want %d", tc.config, got, tc.want)
			}
		})
	}
}

// TestRedisMasterHost pins the per-pod FQDN convention voodu0
// resolves: `<name>-<n>.<scope>.voodu`. Same shape the wrapper
// script bakes in — a mismatch here would have linked consumers
// dialing one host while the entrypoint advertises another.
func TestRedisMasterHost(t *testing.T) {
	cases := []struct {
		scope, name string
		ord         int
		want        string
	}{
		{"clowk-lp", "redis", 0, "redis-0.clowk-lp.voodu"},
		{"clowk-lp", "redis", 1, "redis-1.clowk-lp.voodu"},
		{"clowk-lp", "redis", 7, "redis-7.clowk-lp.voodu"},
		// Unscoped: no extra dot.
		{"", "cache", 0, "cache-0.voodu"},
		// Lowercase-and-trim normalisation.
		{"  CLOWK-LP ", " Redis ", 2, "redis-2.clowk-lp.voodu"},
	}

	for _, tc := range cases {
		got := redisMasterHost(tc.scope, tc.name, tc.ord)
		if got != tc.want {
			t.Errorf("redisMasterHost(%q, %q, %d) = %q, want %q",
				tc.scope, tc.name, tc.ord, got, tc.want)
		}
	}
}

// TestBuildLinkURLs_SingleReplica covers the pre-M2 baseline:
// replicas=1 → single REDIS_URL on the round-robin shared alias,
// no REDIS_READ_URL. The contract for legacy apps must not
// regress when M2 ships.
func TestBuildLinkURLs_SingleReplica(t *testing.T) {
	spec := map[string]any{
		"replicas": float64(1),
		"ports":    []any{"6379"},
	}

	config := map[string]any{"REDIS_PASSWORD": "s3cret"}

	got := buildLinkURLs("clowk-lp", "redis", spec, config, false)

	want := linkURLs{
		WriteURL: "redis://default:s3cret@redis.clowk-lp.voodu:6379",
	}

	if got != want {
		t.Errorf("single-replica:\n  got:  %+v\n  want: %+v", got, want)
	}
}

// TestBuildLinkURLs_MultiReplicaWriter is the dual-URL case:
// write goes to the master FQDN (`redis-0.<scope>.voodu`),
// reads go to the round-robin pool. This is the default for a
// non-`--reads` link when the provider has replicas > 1.
func TestBuildLinkURLs_MultiReplicaWriter(t *testing.T) {
	spec := map[string]any{
		"replicas": float64(3),
		"ports":    []any{"6379"},
	}

	config := map[string]any{"REDIS_PASSWORD": "p"}

	got := buildLinkURLs("clowk-lp", "redis", spec, config, false)

	want := linkURLs{
		WriteURL: "redis://default:p@redis-0.clowk-lp.voodu:6379",
		ReadURL:  "redis://default:p@redis.clowk-lp.voodu:6379",
	}

	if got != want {
		t.Errorf("multi-replica writer:\n  got:  %+v\n  want: %+v", got, want)
	}
}

// TestBuildLinkURLs_MultiReplicaReadsOnly: with --reads, the
// consumer gets ONLY REDIS_URL pointing at the round-robin
// pool. No REDIS_READ_URL — the consumer is read-only by
// declaration, so the dual-URL pattern doesn't apply.
func TestBuildLinkURLs_MultiReplicaReadsOnly(t *testing.T) {
	spec := map[string]any{
		"replicas": float64(3),
		"ports":    []any{"6379"},
	}

	config := map[string]any{"REDIS_PASSWORD": "p"}

	got := buildLinkURLs("clowk-lp", "redis", spec, config, true)

	want := linkURLs{
		WriteURL: "redis://default:p@redis.clowk-lp.voodu:6379",
		ReadURL:  "",
	}

	if got != want {
		t.Errorf("multi-replica reads-only:\n  got:  %+v\n  want: %+v", got, want)
	}
}

// TestBuildLinkURLs_FailoverFlipsMaster: setting
// REDIS_MASTER_ORDINAL=2 in the provider config makes the URL
// builder pin the writer to `redis-2.<scope>.voodu` — the new
// master. F2.2 (manual failover) flow depends on this: operator
// `vd config set redis REDIS_MASTER_ORDINAL=2`, then
// `vd redis:link` (or rotate password) re-emits URLs against
// the new master.
func TestBuildLinkURLs_FailoverFlipsMaster(t *testing.T) {
	spec := map[string]any{
		"replicas": float64(3),
		"ports":    []any{"6379"},
	}

	config := map[string]any{
		"REDIS_PASSWORD":       "p",
		"REDIS_MASTER_ORDINAL": "2",
	}

	got := buildLinkURLs("clowk-lp", "redis", spec, config, false)

	if !strings.Contains(got.WriteURL, "redis-2.clowk-lp.voodu") {
		t.Errorf("WriteURL didn't follow REDIS_MASTER_ORDINAL flip: %q", got.WriteURL)
	}
}

// TestParseLinkedConsumers covers the parser edge cases. The
// list is operator-touchable via `vd config set/unset` so the
// parser must tolerate stray commas, whitespace, empty
// segments — anything that round-trips through a human terminal
// without needing precise format discipline.
func TestParseLinkedConsumers(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"nil config", nil, nil},
		{"missing key", map[string]any{}, nil},
		{"empty string", map[string]any{"REDIS_LINKED_CONSUMERS": ""}, nil},
		{
			"single consumer",
			map[string]any{"REDIS_LINKED_CONSUMERS": "clowk-lp/web"},
			[]string{"clowk-lp/web"},
		},
		{
			"multiple consumers",
			map[string]any{"REDIS_LINKED_CONSUMERS": "clowk-lp/web,clowk-lp/api"},
			[]string{"clowk-lp/web", "clowk-lp/api"},
		},
		{
			"whitespace tolerated",
			map[string]any{"REDIS_LINKED_CONSUMERS": " clowk-lp/web , clowk-lp/api "},
			[]string{"clowk-lp/web", "clowk-lp/api"},
		},
		{
			"empty segments dropped",
			map[string]any{"REDIS_LINKED_CONSUMERS": ",a,,b,"},
			[]string{"a", "b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLinkedConsumers(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseLinkedConsumers(%+v):\n  got:  %v\n  want: %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAddLinkedConsumer_Idempotent: re-linking the same
// consumer must not duplicate. Operators may run
// `vd redis:link prov cons` repeatedly (e.g. as part of CI)
// and expect the list to converge.
func TestAddLinkedConsumer_Idempotent(t *testing.T) {
	config := map[string]any{"REDIS_LINKED_CONSUMERS": "clowk-lp/web"}

	got := addLinkedConsumer(config, "clowk-lp", "web")

	if got != "clowk-lp/web" {
		t.Errorf("re-add of existing consumer should be a no-op, got %q", got)
	}
}

// TestAddLinkedConsumer_AppendsNew: a fresh consumer joins
// the existing list. Order is "existing first, new last" so
// the list reads chronologically (helpful for operators
// scanning what was added when).
func TestAddLinkedConsumer_AppendsNew(t *testing.T) {
	config := map[string]any{"REDIS_LINKED_CONSUMERS": "clowk-lp/web"}

	got := addLinkedConsumer(config, "clowk-lp", "api")

	want := "clowk-lp/web,clowk-lp/api"

	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAddLinkedConsumer_FromEmpty: first link on a fresh
// provider produces a single-entry list (no leading comma).
func TestAddLinkedConsumer_FromEmpty(t *testing.T) {
	got := addLinkedConsumer(nil, "clowk-lp", "web")

	if got != "clowk-lp/web" {
		t.Errorf("got %q, want %q", got, "clowk-lp/web")
	}
}

// TestRemoveLinkedConsumer_RemovesPresent: dropping a consumer
// from the middle of the list preserves order of the rest.
func TestRemoveLinkedConsumer_RemovesPresent(t *testing.T) {
	config := map[string]any{
		"REDIS_LINKED_CONSUMERS": "a/x,b/y,c/z",
	}

	got := removeLinkedConsumer(config, "b", "y")

	if got != "a/x,c/z" {
		t.Errorf("got %q, want %q", got, "a/x,c/z")
	}
}

// TestRemoveLinkedConsumer_AbsentIsNoop: removing a consumer
// that isn't in the list returns the list unchanged.
func TestRemoveLinkedConsumer_AbsentIsNoop(t *testing.T) {
	config := map[string]any{
		"REDIS_LINKED_CONSUMERS": "a/x,b/y",
	}

	got := removeLinkedConsumer(config, "z", "z")

	if got != "a/x,b/y" {
		t.Errorf("got %q, want %q", got, "a/x,b/y")
	}
}

// TestRemoveLinkedConsumer_EmptyAfterAll: removing the only
// entry leaves an empty list. cmdUnlink uses this to know
// whether to emit config_unset (clear the key entirely) vs
// config_set (write the new shorter list).
func TestRemoveLinkedConsumer_EmptyAfterAll(t *testing.T) {
	config := map[string]any{
		"REDIS_LINKED_CONSUMERS": "only/one",
	}

	got := removeLinkedConsumer(config, "only", "one")

	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestParseLinkFlags_OrderAgnostic: --reads can appear anywhere
// in argv. Operators have wildly different muscle memory for
// flag placement (POSIX vs Go, before vs after positional);
// the parser tolerates both.
func TestParseLinkFlags_OrderAgnostic(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		pos  []string
		want bool
	}{
		{"no flag", []string{"a", "b"}, []string{"a", "b"}, false},
		{"flag first", []string{"--reads", "a", "b"}, []string{"a", "b"}, true},
		{"flag middle", []string{"a", "--reads", "b"}, []string{"a", "b"}, true},
		{"flag last", []string{"a", "b", "--reads"}, []string{"a", "b"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, reads, _ := parseLinkFlags(tc.in)

			if !reflect.DeepEqual(pos, tc.pos) {
				t.Errorf("positional: got %v, want %v", pos, tc.pos)
			}

			if reads != tc.want {
				t.Errorf("readsOnly: got %v, want %v", reads, tc.want)
			}
		})
	}
}
