package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseLinkFlags_Sentinel pins the new flag surface — the
// parser must recognise --sentinel alongside --reads, with the
// same order-agnostic treatment.
func TestParseLinkFlags_Sentinel(t *testing.T) {
	cases := []struct {
		name         string
		in           []string
		wantPos      []string
		wantReads    bool
		wantSentinel bool
	}{
		{
			name:         "no flags",
			in:           []string{"a", "b"},
			wantPos:      []string{"a", "b"},
			wantReads:    false,
			wantSentinel: false,
		},
		{
			name:         "sentinel only",
			in:           []string{"--sentinel", "a", "b"},
			wantPos:      []string{"a", "b"},
			wantReads:    false,
			wantSentinel: true,
		},
		{
			name:         "reads + sentinel together",
			in:           []string{"a", "--reads", "b", "--sentinel"},
			wantPos:      []string{"a", "b"},
			wantReads:    true,
			wantSentinel: true,
		},
		{
			name:         "sentinel between positional",
			in:           []string{"a", "--sentinel", "b"},
			wantPos:      []string{"a", "b"},
			wantReads:    false,
			wantSentinel: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, reads, sentinel := parseLinkFlags(tc.in)

			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}

			if reads != tc.wantReads {
				t.Errorf("readsOnly: got %v, want %v", reads, tc.wantReads)
			}

			if sentinel != tc.wantSentinel {
				t.Errorf("sentinel: got %v, want %v", sentinel, tc.wantSentinel)
			}
		})
	}
}

// TestMonitorTargetFromSpec_DataRedis confirms a regular data
// redis (no sentinel block at expand time) returns false. The
// sentinel-detection contract is "VOODU_MONITOR_NAME present in
// the env" — without that key, the resource is a data redis.
func TestMonitorTargetFromSpec_DataRedis(t *testing.T) {
	spec := map[string]any{
		"image": "redis:8",
		"env": map[string]any{
			"REDIS_PASSWORD": "secret",
		},
	}

	target, isSentinel := monitorTargetFromSpec(spec)

	if isSentinel {
		t.Errorf("data redis spec should NOT be detected as sentinel; got target=%+v", target)
	}
}

// TestMonitorTargetFromSpec_NoEnv handles the spec with no env
// map at all. Should be treated as not-a-sentinel (defensive).
func TestMonitorTargetFromSpec_NoEnv(t *testing.T) {
	spec := map[string]any{"image": "redis:8"}

	_, isSentinel := monitorTargetFromSpec(spec)

	if isSentinel {
		t.Errorf("spec without env should NOT be detected as sentinel")
	}
}

// TestMonitorTargetFromSpec_Sentinel is the happy path — a
// sentinel-mode spec carries VOODU_MONITOR_SCOPE +
// VOODU_MONITOR_NAME (baked by sentinelPodEnv at expand time).
// Detection works AND the target ref is correctly extracted.
func TestMonitorTargetFromSpec_Sentinel(t *testing.T) {
	spec := map[string]any{
		"image": "redis:8",
		"env": map[string]any{
			"VOODU_MONITOR_SCOPE":  "clowk-lp",
			"VOODU_MONITOR_NAME":   "redis",
			"VOODU_REDIS_REPLICAS": "3",
		},
	}

	target, isSentinel := monitorTargetFromSpec(spec)

	if !isSentinel {
		t.Fatal("spec with VOODU_MONITOR_NAME should be detected as sentinel")
	}

	if target.scope != "clowk-lp" {
		t.Errorf("target.scope = %q, want clowk-lp", target.scope)
	}

	if target.name != "redis" {
		t.Errorf("target.name = %q, want redis", target.name)
	}
}

// TestBuildSentinelHosts_Replicas pins the host list shape:
// comma-separated `<name>-<ord>.<scope>.voodu:26379` entries,
// one per replica, ordinals 0..N-1, port hard to sentinelPort.
func TestBuildSentinelHosts_Replicas(t *testing.T) {
	got := buildSentinelHosts("clowk-lp", "redis-quorum", 3)

	want := "redis-quorum-0.clowk-lp.voodu:26379," +
		"redis-quorum-1.clowk-lp.voodu:26379," +
		"redis-quorum-2.clowk-lp.voodu:26379"

	if got != want {
		t.Errorf("buildSentinelHosts:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestBuildSentinelHosts_Unscoped: unscoped resources resolve as
// `<name>-<ord>.voodu:26379` (no scope segment). Same convention
// as the data-redis FQDN scheme.
func TestBuildSentinelHosts_Unscoped(t *testing.T) {
	got := buildSentinelHosts("", "redis-quorum", 1)

	want := "redis-quorum-0.voodu:26379"

	if got != want {
		t.Errorf("unscoped: got %q, want %q", got, want)
	}
}

// TestBuildSentinelHosts_ReplicasClampToOne: replicas <= 0 falls
// back to a single host. Defensive (validation upstream rejects
// the bad values, but the function shouldn't return an empty
// list — that would emit REDIS_SENTINEL_HOSTS="" which fails on
// every sentinel-aware client.)
func TestBuildSentinelHosts_ReplicasClampToOne(t *testing.T) {
	cases := []int{0, -1, -100}

	for _, n := range cases {
		got := buildSentinelHosts("scope", "redis-quorum", n)

		if !strings.Contains(got, "redis-quorum-0") {
			t.Errorf("replicas=%d should yield at least one host, got %q", n, got)
		}

		if strings.Contains(got, "redis-quorum-1") {
			t.Errorf("replicas=%d should NOT yield ordinal 1, got %q", n, got)
		}
	}
}

// TestBuildSentinelHosts_LowercasesNames: voodu0's DNS is
// case-insensitive but we normalize for consistency with
// redisMasterHost (which also lowercases).
func TestBuildSentinelHosts_LowercasesNames(t *testing.T) {
	got := buildSentinelHosts("Clowk-LP", "Redis-Quorum", 1)

	if !strings.Contains(got, "redis-quorum-0.clowk-lp.voodu:26379") {
		t.Errorf("hosts should be lowercased, got %q", got)
	}
}
