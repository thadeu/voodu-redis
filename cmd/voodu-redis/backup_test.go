package main

import (
	"reflect"
	"testing"
)

// TestParseBackupFlags pins the argv parser for the backup command.
// Operators rely on both space (--destination /path) and equals
// (--destination=/path) forms; --source is integer-typed.
func TestParseBackupFlags(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantPos     []string
		wantDest    string
		wantSource  int
		wantHasSrc  bool
	}{
		{
			name:    "minimal — only destination",
			in:      []string{"clowk-lp/redis", "--destination", "/tmp/snap.rdb"},
			wantPos: []string{"clowk-lp/redis"},
			wantDest: "/tmp/snap.rdb",
		},
		{
			name:    "equals form for destination",
			in:      []string{"clowk-lp/redis", "--destination=/tmp/snap.rdb"},
			wantPos: []string{"clowk-lp/redis"},
			wantDest: "/tmp/snap.rdb",
		},
		{
			name:       "with explicit source",
			in:         []string{"clowk-lp/redis", "--destination", "/tmp/snap.rdb", "--source", "0"},
			wantPos:    []string{"clowk-lp/redis"},
			wantDest:   "/tmp/snap.rdb",
			wantSource: 0,
			wantHasSrc: true,
		},
		{
			name:       "source equals form",
			in:         []string{"clowk-lp/redis", "--source=2", "--destination=/tmp/snap.rdb"},
			wantPos:    []string{"clowk-lp/redis"},
			wantDest:   "/tmp/snap.rdb",
			wantSource: 2,
			wantHasSrc: true,
		},
		{
			name:    "missing destination — caller's job to surface usage error",
			in:      []string{"clowk-lp/redis"},
			wantPos: []string{"clowk-lp/redis"},
		},
		{
			name:       "flags before positional",
			in:         []string{"--destination", "/tmp/x.rdb", "--source", "1", "clowk-lp/redis"},
			wantPos:    []string{"clowk-lp/redis"},
			wantDest:   "/tmp/x.rdb",
			wantSource: 1,
			wantHasSrc: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, dest, src, hasSrc := parseBackupFlags(tc.in)

			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}

			if dest != tc.wantDest {
				t.Errorf("destination: got %q, want %q", dest, tc.wantDest)
			}

			if hasSrc != tc.wantHasSrc {
				t.Errorf("hasSource: got %v, want %v", hasSrc, tc.wantHasSrc)
			}

			if hasSrc && src != tc.wantSource {
				t.Errorf("source: got %d, want %d", src, tc.wantSource)
			}
		})
	}
}

// TestParseRestoreFlags pins the simpler --from parser.
func TestParseRestoreFlags(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantPos []string
		wantSrc string
	}{
		{
			name:    "space form",
			in:      []string{"clowk-lp/redis", "--from", "/tmp/snap.rdb"},
			wantPos: []string{"clowk-lp/redis"},
			wantSrc: "/tmp/snap.rdb",
		},
		{
			name:    "equals form",
			in:      []string{"clowk-lp/redis", "--from=/tmp/snap.rdb"},
			wantPos: []string{"clowk-lp/redis"},
			wantSrc: "/tmp/snap.rdb",
		},
		{
			name:    "flag first",
			in:      []string{"--from", "/tmp/x.rdb", "clowk-lp/redis"},
			wantPos: []string{"clowk-lp/redis"},
			wantSrc: "/tmp/x.rdb",
		},
		{
			name:    "missing --from",
			in:      []string{"clowk-lp/redis"},
			wantPos: []string{"clowk-lp/redis"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, src := parseRestoreFlags(tc.in)

			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}

			if src != tc.wantSrc {
				t.Errorf("source: got %q, want %q", src, tc.wantSrc)
			}
		})
	}
}

// TestPickBackupSource pins the auto-source selection.
//
//   - Single pod (replicas=1): always ordinal 0 (the master).
//   - Multi-pod (replicas>1): highest ordinal (offload master).
//   - Multi-pod where master moved to highest ordinal via failover:
//     step back one to avoid backing up FROM the master.
//   - --source override always wins.
func TestPickBackupSource(t *testing.T) {
	cases := []struct {
		name       string
		replicas   int
		master     int   // current master ordinal (from REDIS_MASTER_ORDINAL)
		hasOverride bool
		override   int
		want       int
	}{
		{name: "single pod", replicas: 1, master: 0, want: 0},
		{name: "3 pods, master at 0 → backup from highest replica (2)", replicas: 3, master: 0, want: 2},
		{name: "3 pods, master flipped to 2 → step back to 1", replicas: 3, master: 2, want: 1},
		{name: "5 pods, master at 0 → backup from 4", replicas: 5, master: 0, want: 4},
		{name: "override forces ordinal 0 even with replicas>1", replicas: 3, master: 0, hasOverride: true, override: 0, want: 0},
		{name: "override forces master ordinal", replicas: 3, master: 1, hasOverride: true, override: 1, want: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{
				"REDIS_MASTER_ORDINAL": "" + string(rune('0'+tc.master)),
			}

			got := pickBackupSource(tc.replicas, config, tc.override, tc.hasOverride)
			if got != tc.want {
				t.Errorf("pickBackupSource(replicas=%d, master=%d, override=%v(%d)) = %d, want %d",
					tc.replicas, tc.master, tc.hasOverride, tc.override, got, tc.want)
			}
		})
	}
}

// TestContainerNameFor pins the docker container naming convention
// — must mirror containers.ContainerName from voodu's internal
// package: <scope>-<name>.<ordinal> for scoped, <name>.<ordinal>
// for unscoped. If voodu's convention drifts, our docker exec
// will hit a non-existent container.
func TestContainerNameFor(t *testing.T) {
	cases := []struct {
		scope, name string
		ordinal     int
		want        string
	}{
		{"clowk-lp", "redis", 0, "clowk-lp-redis.0"},
		{"clowk-lp", "redis", 2, "clowk-lp-redis.2"},
		{"", "postgres", 1, "postgres.1"},
		{"team-a", "cache", 5, "team-a-cache.5"},
	}

	for _, tc := range cases {
		got := containerNameFor(tc.scope, tc.name, tc.ordinal)
		if got != tc.want {
			t.Errorf("containerNameFor(%q, %q, %d) = %q, want %q",
				tc.scope, tc.name, tc.ordinal, got, tc.want)
		}
	}
}

// TestAsString covers the env-value extraction that
// detectSentinelWatching relies on. The /describe response can
// shape values as plain strings or json.RawMessage depending on
// codec path.
func TestAsString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"plain string", "redis", "redis"},
		{"empty string", "", ""},
		{"nil", nil, ""},
		{"int", 42, ""},
		{"bool", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := asString(tc.in); got != tc.want {
				t.Errorf("asString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
