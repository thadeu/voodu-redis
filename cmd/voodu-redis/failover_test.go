package main

import (
	"reflect"
	"testing"
)

// TestParseFailoverFlags pins the argv parser. --replica may use
// space or = separator, may appear before or after the
// positional ref. Operators have wildly different muscle memory
// for flag placement; the parser tolerates both.
func TestParseFailoverFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string

		wantPos    []string
		wantTarget int
		wantHas    bool
	}{
		{
			name:       "space form, flag last",
			in:         []string{"clowk-lp/redis", "--replica", "1"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 1,
			wantHas:    true,
		},
		{
			name:       "space form, flag first",
			in:         []string{"--replica", "2", "clowk-lp/redis"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 2,
			wantHas:    true,
		},
		{
			name:       "equals form",
			in:         []string{"clowk-lp/redis", "--replica=1"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 1,
			wantHas:    true,
		},
		{
			name:       "target zero is valid (recovery flow)",
			in:         []string{"clowk-lp/redis", "--replica", "0"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 0,
			wantHas:    true,
		},
		{
			name: "missing --replica leaves hasTarget false",
			in:   []string{"clowk-lp/redis"},

			wantPos: []string{"clowk-lp/redis"},
			wantHas: false,
		},
		{
			// `--replica abc` — non-numeric: parser silently drops the
			// flag rather than parsing partial. The caller's
			// validation surfaces "missing --replica" instead of a
			// confusing "ordinal -1 out of range".
			name:    "non-numeric value drops the flag",
			in:      []string{"clowk-lp/redis", "--replica", "abc"},
			wantPos: []string{"clowk-lp/redis"},
			wantHas: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, target, has, _ := parseFailoverFlags(tc.in)

			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}

			if has != tc.wantHas {
				t.Errorf("hasTarget: got %v, want %v", has, tc.wantHas)
			}

			if has && target != tc.wantTarget {
				t.Errorf("target: got %d, want %d", target, tc.wantTarget)
			}
		})
	}
}

// TestParseFailoverFlags_DanglingFlag: `--replica` without a value
// silently drops to hasTarget=false. The caller surfaces the
// usage error; the parser doesn't try to be cleverer than the
// input.
func TestParseFailoverFlags_DanglingFlag(t *testing.T) {
	pos, _, has, _ := parseFailoverFlags([]string{"clowk-lp/redis", "--replica"})

	if has {
		t.Errorf("dangling --replica should not set hasTarget")
	}

	if !reflect.DeepEqual(pos, []string{"clowk-lp/redis"}) {
		t.Errorf("positional: got %v, want [clowk-lp/redis]", pos)
	}
}

// TestParseFailoverFlags_NoRestart pins the --no-restart flag
// surface — it's the protocol the sentinel auto-failover hook
// uses to tell voodu "I already moved the roles inside Redis,
// just sync the store, don't roll the pods".
//
// Order-agnostic with --replica and the positional ref. Default off.
func TestParseFailoverFlags_NoRestart(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantPos     []string
		wantTarget  int
		wantNoRestart bool
	}{
		{
			name:          "no-restart absent → default false",
			in:            []string{"clowk-lp/redis", "--replica", "1"},
			wantPos:       []string{"clowk-lp/redis"},
			wantTarget:    1,
			wantNoRestart: false,
		},
		{
			name:          "no-restart at end",
			in:            []string{"clowk-lp/redis", "--replica", "1", "--no-restart"},
			wantPos:       []string{"clowk-lp/redis"},
			wantTarget:    1,
			wantNoRestart: true,
		},
		{
			name:          "no-restart at start",
			in:            []string{"--no-restart", "clowk-lp/redis", "--replica", "1"},
			wantPos:       []string{"clowk-lp/redis"},
			wantTarget:    1,
			wantNoRestart: true,
		},
		{
			name:          "no-restart between args",
			in:            []string{"clowk-lp/redis", "--no-restart", "--replica=2"},
			wantPos:       []string{"clowk-lp/redis"},
			wantTarget:    2,
			wantNoRestart: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, target, has, noRestart := parseFailoverFlags(tc.in)

			if !has {
				t.Fatalf("expected hasTarget=true, got false")
			}

			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}

			if target != tc.wantTarget {
				t.Errorf("target: got %d, want %d", target, tc.wantTarget)
			}

			if noRestart != tc.wantNoRestart {
				t.Errorf("noRestart: got %v, want %v", noRestart, tc.wantNoRestart)
			}
		})
	}
}

// TestParseFailoverFlags_LegacyToFlagIgnored confirms `--to` is
// no longer recognized as a synonym. It just becomes a positional
// arg (which the caller rejects with the usage error). This
// pins the rename — operators relying on muscle memory get a
// clear failure rather than silently-wrong behaviour.
func TestParseFailoverFlags_LegacyToFlagIgnored(t *testing.T) {
	// `--to 1` with no `--replica` → hasTarget should be false,
	// `--to` and `1` end up in positional.
	pos, _, has, _ := parseFailoverFlags([]string{"clowk-lp/redis", "--to", "1"})

	if has {
		t.Errorf("legacy --to should NOT set hasTarget; got hasTarget=true")
	}

	// `--to` and `1` leak into positional — caller's positional
	// length check will surface this as a usage error.
	if len(pos) != 3 {
		t.Errorf("legacy --to should leak into positional; got pos=%v", pos)
	}
}
