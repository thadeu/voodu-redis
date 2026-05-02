package main

import (
	"strings"
	"testing"
)

// TestParseSentinelSpec_AbsentReturnsNil pins the "no block, no
// surprise" contract: an operator who never writes `sentinel { }`
// gets nil back, and the cmdExpand path stays on the data-redis
// branch verbatim. Existing redis resources keep working without
// migration.
func TestParseSentinelSpec_AbsentReturnsNil(t *testing.T) {
	operatorSpec := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
	}

	got, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("absent block produced error: %v", err)
	}

	if got != nil {
		t.Errorf("absent block should return nil, got %+v", got)
	}
}

// TestParseSentinelSpec_PresentDisabled keeps Enabled=false
// distinguishable from "block absent". This matters because the
// operator's flow for taking sentinel down is `enabled = false`
// then `vd apply --prune` — and the SAME apply that flips off
// shouldn't accidentally re-trigger sentinel-mode validation on
// stale fields.
func TestParseSentinelSpec_PresentDisabled(t *testing.T) {
	operatorSpec := map[string]any{
		"sentinel": map[string]any{
			"enabled": false,
			"monitor": "scope/redis",
		},
	}

	got, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("disabled block produced error: %v", err)
	}

	if got == nil {
		t.Fatal("disabled block should return non-nil sentinelSpec")
	}

	if got.Enabled {
		t.Errorf("Enabled should be false, got true")
	}

	if got.Monitor != "scope/redis" {
		t.Errorf("Monitor = %q, want %q", got.Monitor, "scope/redis")
	}
}

// TestParseSentinelSpec_PresentEnabled is the happy path —
// well-formed block → populated struct, no error.
func TestParseSentinelSpec_PresentEnabled(t *testing.T) {
	operatorSpec := map[string]any{
		"sentinel": map[string]any{
			"enabled": true,
			"monitor": "clowk-lp/redis",
		},
	}

	got, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("happy path produced error: %v", err)
	}

	if got == nil || !got.Enabled || got.Monitor != "clowk-lp/redis" {
		t.Errorf("got %+v, want {Enabled:true Monitor:clowk-lp/redis}", got)
	}
}

// TestParseSentinelSpec_EnabledDefaultsTrue pins the "block
// presence = intent" contract. When the operator writes
// `sentinel { monitor = "..." }` without `enabled = true`, we
// treat it as enabled — the operator wouldn't bother declaring
// the block otherwise. Saves one line of HCL boilerplate per
// sentinel resource.
//
// Inverse path (`enabled = false` toggles off) is preserved by
// the explicit-override test below.
func TestParseSentinelSpec_EnabledDefaultsTrue(t *testing.T) {
	operatorSpec := map[string]any{
		"sentinel": map[string]any{
			"monitor": "clowk-lp/redis",
			// no `enabled` field — operator wrote the minimal block
		},
	}

	got, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("minimal block produced error: %v", err)
	}

	if got == nil {
		t.Fatal("minimal block should parse to non-nil")
	}

	if !got.Enabled {
		t.Errorf("minimal block (no `enabled` field) should default to Enabled=true; got false")
	}

	if got.Monitor != "clowk-lp/redis" {
		t.Errorf("Monitor = %q, want clowk-lp/redis", got.Monitor)
	}
}

// TestParseSentinelSpec_ListShape: HCL2 sometimes emits repeated
// blocks as a list. We tolerate the [obj] shape for compat with
// HCL parsers that don't collapse single-block-as-object, but
// reject duplicate blocks since sentinel is singleton-per-resource.
func TestParseSentinelSpec_ListShape(t *testing.T) {
	operatorSpec := map[string]any{
		"sentinel": []any{
			map[string]any{
				"enabled": true,
				"monitor": "scope/redis",
			},
		},
	}

	got, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("[obj] shape rejected: %v", err)
	}

	if got == nil || !got.Enabled {
		t.Errorf("got %+v, want enabled sentinelSpec", got)
	}
}

// TestParseSentinelSpec_ListWithMultipleRejects pins the
// singleton rule. Two `sentinel { }` blocks on the same
// resource is a config error — not an attempt to declare
// "two sentinel quorums" (which doesn't make sense here).
func TestParseSentinelSpec_ListWithMultipleRejects(t *testing.T) {
	operatorSpec := map[string]any{
		"sentinel": []any{
			map[string]any{"enabled": true, "monitor": "x/a"},
			map[string]any{"enabled": true, "monitor": "x/b"},
		},
	}

	_, err := parseSentinelSpec(operatorSpec)
	if err == nil {
		t.Fatal("expected error for duplicate sentinel blocks, got nil")
	}

	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should mention singleton rule, got: %v", err)
	}
}

// TestParseSentinelSpec_TypeErrors: malformed field types should
// fail with a typed error mentioning the field. Operators see
// "sentinel.enabled has unexpected type ..." and know exactly
// where to look.
func TestParseSentinelSpec_TypeErrors(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want string
	}{
		{
			name: "enabled-not-bool",
			spec: map[string]any{"sentinel": map[string]any{"enabled": "yes"}},
			want: "sentinel.enabled",
		},
		{
			name: "monitor-not-string",
			spec: map[string]any{"sentinel": map[string]any{"enabled": true, "monitor": 42}},
			want: "sentinel.monitor",
		},
		{
			name: "block-not-object",
			spec: map[string]any{"sentinel": "enabled"},
			want: "sentinel block",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSentinelSpec(tc.spec)
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateSentinelSpec_DisabledIsNoop confirms validation
// runs ONLY when Enabled is true. A disabled block (or no block)
// passes validation regardless of other fields — the operator
// might leave a stale `monitor` value during a flip-off cycle.
func TestValidateSentinelSpec_DisabledIsNoop(t *testing.T) {
	cases := []struct {
		name string
		spec *sentinelSpec
	}{
		{"nil-block", nil},
		{"disabled-with-empty-monitor", &sentinelSpec{Enabled: false}},
		{"disabled-with-bad-monitor", &sentinelSpec{Enabled: false, Monitor: "garbage"}},
	}

	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSentinelSpec(tc.spec, req, nil); err != nil {
				t.Errorf("disabled/nil should pass: %v", err)
			}
		})
	}
}

// TestValidateSentinelSpec_EnabledRequiresMonitor: operator wrote
// `sentinel { enabled = true }` but forgot the monitor. Fail loud.
func TestValidateSentinelSpec_EnabledRequiresMonitor(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}

	cases := []struct {
		name string
		spec *sentinelSpec
	}{
		{"empty-monitor", &sentinelSpec{Enabled: true, Monitor: ""}},
		{"whitespace-monitor", &sentinelSpec{Enabled: true, Monitor: "   "}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSentinelSpec(tc.spec, req, nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !strings.Contains(err.Error(), "requires sentinel.monitor") {
				t.Errorf("error should explain monitor is required, got: %v", err)
			}
		})
	}
}

// TestValidateSentinelSpec_MonitorShape: monitor must be exactly
// "scope/name". Test the typos operators actually make.
func TestValidateSentinelSpec_MonitorShape(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}

	cases := []struct {
		name    string
		monitor string
	}{
		{"missing-slash", "redis"},
		{"too-many-slashes", "team/scope/redis"},
		{"trailing-slash", "clowk-lp/"},
		{"leading-slash", "/redis"},
		{"only-slash", "/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &sentinelSpec{Enabled: true, Monitor: tc.monitor}

			err := validateSentinelSpec(s, req, nil)
			if err == nil {
				t.Fatalf("monitor=%q should be rejected", tc.monitor)
			}

			if !strings.Contains(err.Error(), "sentinel.monitor") {
				t.Errorf("error should mention sentinel.monitor field, got: %v", err)
			}
		})
	}
}

// TestValidateSentinelSpec_CrossScopeRejected: monitor has scope
// "infra" but resource is in "team-a". Cross-scope is parked for
// a future milestone — for now the operator must declare both
// resources in the same scope.
func TestValidateSentinelSpec_CrossScopeRejected(t *testing.T) {
	req := expandRequest{Scope: "team-a", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "infra/shared-redis"}

	err := validateSentinelSpec(s, req, nil)
	if err == nil {
		t.Fatal("cross-scope monitor should be rejected")
	}

	if !strings.Contains(err.Error(), "cross-scope") {
		t.Errorf("error should mention cross-scope is unsupported, got: %v", err)
	}
}

// TestValidateSentinelSpec_SelfMonitorRejected: a sentinel that
// monitors itself is a topology mistake — sentinel needs a peer
// data redis to watch. Must be a different name in same scope.
func TestValidateSentinelSpec_SelfMonitorRejected(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis-quorum"}

	err := validateSentinelSpec(s, req, nil)
	if err == nil {
		t.Fatal("self-monitor should be rejected")
	}

	if !strings.Contains(err.Error(), "itself") {
		t.Errorf("error should mention self-reference, got: %v", err)
	}
}

// TestValidateSentinelSpec_ReplicasMatrix pins the quorum-math
// guard. replicas=2 is THE landmine — quorum=2 means losing one
// pod kills failover, which is strictly worse than running a
// single sentinel. We reject 2 explicitly so operators don't
// reach for "I'll just use 2 to save resources" without seeing
// the real cost.
func TestValidateSentinelSpec_ReplicasMatrix(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}

	cases := []struct {
		replicas any
		wantErr  bool
		errHint  string
	}{
		{nil, false, ""},      // omitted → falls to sentinel default 3 in M-S1
		{1, false, ""},        // degenerate observer-only, allowed
		{3, false, ""},        // quorum=2, survives 1 outage
		{5, false, ""},        // quorum=3, survives 2 outages
		{2, true, "replicas = 2"},
		{0, true, "not allowed"},
		{-1, true, "not allowed"},
	}

	for _, tc := range cases {
		name := "omitted"
		if tc.replicas != nil {
			name = "replicas-" + nonceName(tc.replicas)
		}

		t.Run(name, func(t *testing.T) {
			s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

			operatorSpec := map[string]any{}
			if tc.replicas != nil {
				operatorSpec["replicas"] = tc.replicas
			}

			err := validateSentinelSpec(s, req, operatorSpec)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("replicas=%v should be rejected", tc.replicas)
				}

				if tc.errHint != "" && !strings.Contains(err.Error(), tc.errHint) {
					t.Errorf("error %q should contain %q", err.Error(), tc.errHint)
				}

				return
			}

			if err != nil {
				t.Errorf("replicas=%v should pass: %v", tc.replicas, err)
			}
		})
	}
}

// TestValidateSentinelSpec_ReplicasFromFloat64 covers the
// JSON-decoder reality: integers come back as float64 unless we
// post-process. asInt should accept whole floats and reject
// non-integral ones (operator wrote `replicas = 3.5` → typo
// surfaced, not rounded silently).
func TestValidateSentinelSpec_ReplicasFromFloat64(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

	// Whole number as float64 → accepted as 3
	if err := validateSentinelSpec(s, req, map[string]any{"replicas": float64(3)}); err != nil {
		t.Errorf("float64(3) should pass: %v", err)
	}

	// Fractional float → rejected as non-integer
	err := validateSentinelSpec(s, req, map[string]any{"replicas": 3.5})
	if err == nil {
		t.Fatal("fractional replicas should be rejected")
	}

	if !strings.Contains(err.Error(), "unexpected type") {
		t.Errorf("error should hint at type mismatch, got: %v", err)
	}
}

// TestSplitMonitorRef_Edges hits the parser directly to nail the
// "what's a valid scope/name" surface. Used by validate, also
// useful for M-S1 entrypoint code that re-parses the same field.
func TestSplitMonitorRef_Edges(t *testing.T) {
	cases := []struct {
		in      string
		scope   string
		name    string
		wantErr bool
	}{
		{"scope/name", "scope", "name", false},
		{"  scope/name  ", "scope", "name", false},
		{"scope / name", "scope", "name", false},
		{"", "", "", true},
		{"   ", "", "", true},
		{"noslash", "", "", true},
		{"a/b/c", "", "", true},
		{"/name", "", "", true},
		{"scope/", "", "", true},
		{"/", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			scope, name, err := splitMonitorRef(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got (%q, %q)", tc.in, scope, name)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}

			if scope != tc.scope || name != tc.name {
				t.Errorf("splitMonitorRef(%q) = (%q, %q), want (%q, %q)",
					tc.in, scope, name, tc.scope, tc.name)
			}
		})
	}
}

// TestStripSentinelBlock confirms the block doesn't leak into
// the merged spec downstream. If M-S0 stub-error path is bypassed
// somehow (test-only) or M-S1 handles sentinel mode upstream,
// the data-redis emit path still strips defensively.
func TestStripSentinelBlock(t *testing.T) {
	merged := map[string]any{
		"image":    "redis:7-alpine",
		"replicas": 1,
		"sentinel": map[string]any{"enabled": false},
	}

	stripSentinelBlock(merged)

	if _, present := merged["sentinel"]; present {
		t.Errorf("sentinel key should be removed, still present in: %+v", merged)
	}

	if merged["image"] != "redis:7-alpine" {
		t.Errorf("non-sentinel keys should survive: image = %v", merged["image"])
	}
}

// nonceName turns a replicas value into a stable test-name slug
// without pulling fmt — keeps the test table readable in -v output.
func nonceName(v any) string {
	switch n := v.(type) {
	case int:
		if n < 0 {
			return "neg" + itoa(-n)
		}

		return itoa(n)
	}

	return "x"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}
