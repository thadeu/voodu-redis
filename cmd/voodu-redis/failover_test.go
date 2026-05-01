package main

import (
	"reflect"
	"testing"
)

// TestParseFailoverFlags pins the argv parser. --to may use
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
			in:         []string{"clowk-lp/redis", "--to", "1"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 1,
			wantHas:    true,
		},
		{
			name:       "space form, flag first",
			in:         []string{"--to", "2", "clowk-lp/redis"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 2,
			wantHas:    true,
		},
		{
			name:       "equals form",
			in:         []string{"clowk-lp/redis", "--to=1"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 1,
			wantHas:    true,
		},
		{
			name:       "target zero is valid (recovery flow)",
			in:         []string{"clowk-lp/redis", "--to", "0"},
			wantPos:    []string{"clowk-lp/redis"},
			wantTarget: 0,
			wantHas:    true,
		},
		{
			name: "missing --to leaves hasTarget false",
			in:   []string{"clowk-lp/redis"},

			wantPos: []string{"clowk-lp/redis"},
			wantHas: false,
		},
		{
			// `--to abc` — non-numeric: parser silently drops the
			// flag rather than parsing partial. The caller's
			// validation surfaces "missing --to" instead of a
			// confusing "ordinal -1 out of range".
			name:    "non-numeric value drops the flag",
			in:      []string{"clowk-lp/redis", "--to", "abc"},
			wantPos: []string{"clowk-lp/redis"},
			wantHas: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, target, has := parseFailoverFlags(tc.in)

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

// TestParseFailoverFlags_DanglingFlag: `--to` without a value
// silently drops to hasTarget=false. The caller surfaces the
// usage error; the parser doesn't try to be cleverer than the
// input.
func TestParseFailoverFlags_DanglingFlag(t *testing.T) {
	pos, _, has := parseFailoverFlags([]string{"clowk-lp/redis", "--to"})

	if has {
		t.Errorf("dangling --to should not set hasTarget")
	}

	if !reflect.DeepEqual(pos, []string{"clowk-lp/redis"}) {
		t.Errorf("positional: got %v, want [clowk-lp/redis]", pos)
	}
}
