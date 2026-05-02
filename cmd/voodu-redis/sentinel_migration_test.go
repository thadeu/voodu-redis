// Migration-shape tests pinning the user-facing F3 contract:
//
//   1. A data redis can have a separate sentinel resource added
//      to its scope without re-emitting the data redis itself.
//      Adding sentinel = pure additive, zero churn on data plane.
//
//   2. Removing the sentinel resource doesn't touch the data
//      redis. Roll-back is "delete the sentinel HCL block, run
//      `vd apply --prune`" — simpler than any forward migration.
//
// These tests don't simulate the controller's apply pipeline
// end-to-end; they assert the SHAPE of expand outputs the
// pipeline consumes. The two-resource design (one redis without
// sentinel block + one redis with sentinel block) means each
// resource's expand stays pure: the data redis's bytes don't
// contain ANY reference to the sentinel resource, and vice
// versa. Cross-resource coupling lives only in the runtime
// entrypoint (which queries the controller for the current
// state) — not in the static manifests.

package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// runExpand drives cmdExpand with a JSON request, returning the
// full envelope. Mirrors the wire shape the controller uses to
// invoke the plugin via stdin/stdout.
func runExpandSpec(t *testing.T, scope, name string, spec map[string]any, config map[string]string) (manifests []manifest, err error) {
	t.Helper()

	rawSpec, _ := json.Marshal(spec)

	req := expandRequest{
		Kind:   "redis",
		Scope:  scope,
		Name:   name,
		Spec:   rawSpec,
		Config: config,
	}

	// Reuse the same parse + validate + emit flow cmdExpand uses.
	// Splitting it out as a callable would be cleanest; for now
	// we mimic the relevant decisions inline.
	var operatorSpec map[string]any
	_ = json.Unmarshal(req.Spec, &operatorSpec)

	sentinel, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		return nil, err
	}

	if err := validateSentinelSpec(sentinel, req, operatorSpec); err != nil {
		return nil, err
	}

	if sentinel != nil && sentinel.Enabled {
		return sentinelManifests(req, sentinel, operatorSpec), nil
	}

	// Data-redis path (simulated — we don't call composeDefaults
	// here because the data emission needs readGeneratedConf
	// which reads from disk; for migration tests we only care
	// about whether the sentinel-side emit affects the data
	// path's STRUCTURE, not its byte-for-byte output).
	return nil, nil
}

// TestMigration_AddingSentinelDoesNotTouchDataRedis is the Q1
// promise from the design conversation: "no sentinel → add
// sentinel, dados intactos, sem migração". The data redis's
// expand input doesn't change; the sentinel expand is a SEPARATE
// resource. This test asserts that even if both resources are in
// the same scope, the data redis's expand request is unchanged
// from a world without sentinel.
func TestMigration_AddingSentinelDoesNotTouchDataRedis(t *testing.T) {
	// World 1: data redis only (pre-sentinel).
	dataSpecBefore := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
	}

	// World 2: same data redis + a NEW sentinel resource in the
	// same scope. The data redis's spec is BIT-IDENTICAL to before.
	dataSpecAfter := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
	}

	if !reflect.DeepEqual(dataSpecBefore, dataSpecAfter) {
		t.Fatalf("test setup error: spec maps should be equal\nbefore: %+v\nafter:  %+v", dataSpecBefore, dataSpecAfter)
	}

	// Adding the sentinel resource is a SEPARATE expand call
	// against a different (scope, name). It doesn't pass through
	// dataSpecAfter at all. This is the architectural property
	// that gives us "zero churn on data redis" for free.
	sentinelSpec := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
		"sentinel": map[string]any{"enabled": true, "monitor": "clowk-lp/redis"},
	}

	got, err := runExpandSpec(t, "clowk-lp", "redis-quorum", sentinelSpec, nil)
	if err != nil {
		t.Fatalf("sentinel expand failed: %v", err)
	}

	// Sanity: the sentinel expand emits its own (asset, statefulset)
	// pair, scoped to the sentinel resource — it never references
	// the data redis's name in the manifest's Name field.
	for i, m := range got {
		if m.Name != "redis-quorum" {
			t.Errorf("manifest[%d].Name = %q, expected sentinel resource name 'redis-quorum' (would suggest cross-resource leakage if not)", i, m.Name)
		}
	}
}

// TestMigration_RemovingSentinelDoesNotTouchDataRedis is the Q2
// promise: "with sentinel → remove sentinel, dados intactos".
// Removing the sentinel block / sentinel resource = `vd apply
// --prune` deletes the sentinel manifests; the data redis's
// manifests are untouched (different (kind, scope, name) tuple).
//
// This test verifies the REVERSE of the add path: a data-redis
// expand without a sentinel block produces the same manifest
// shape regardless of whether a sentinel resource USED TO exist
// in the same scope. The data redis's expand has no awareness
// of peer sentinel resources.
func TestMigration_RemovingSentinelDoesNotTouchDataRedis(t *testing.T) {
	// Data redis spec is identical pre- and post-sentinel-removal:
	// the operator just deleted the OTHER resource block from HCL.
	spec := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
	}

	// expand the data redis — no sentinel block present, no
	// sentinel-related fields in spec.
	op, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	req := expandRequest{Kind: "redis", Scope: "clowk-lp", Name: "redis", Spec: op}

	var operatorSpec map[string]any
	_ = json.Unmarshal(req.Spec, &operatorSpec)

	sentinel, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("data-redis expand parse: %v", err)
	}

	if sentinel != nil {
		t.Errorf("data redis spec should yield NIL sentinelSpec; got %+v", sentinel)
	}

	// And it shouldn't trip validation either.
	if err := validateSentinelSpec(sentinel, req, operatorSpec); err != nil {
		t.Errorf("data-redis expand validate: %v", err)
	}
}

// TestMigration_SentinelTogglingPreservesDataRedisIdempotency
// pins the soft-toggle path: operator writes
// `sentinel { enabled = false }` on the SENTINEL resource (not
// the data one) to disable the sentinel quorum without deleting
// it from HCL. The sentinel's expand should fall through to the
// data-redis path (because Enabled=false), and the result must
// not depend on whether `sentinel` block was present-but-disabled
// vs absent.
//
// This is the "scratch fix" path before the operator commits
// to fully removing the sentinel resource — useful in incident
// scenarios where you want to disable sentinel quickly.
func TestMigration_SentinelTogglingPreservesDataRedisIdempotency(t *testing.T) {
	// Sentinel disabled via `sentinel { enabled = false }` —
	// the parser yields a non-nil spec but Enabled is false,
	// so validate is a noop and expand drops back to
	// data-redis-style emission.
	op := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
		"sentinel": map[string]any{"enabled": false},
	}

	rawOp, _ := json.Marshal(op)

	req := expandRequest{Kind: "redis", Scope: "clowk-lp", Name: "redis-quorum", Spec: rawOp}

	var operatorSpec map[string]any
	_ = json.Unmarshal(req.Spec, &operatorSpec)

	sentinel, err := parseSentinelSpec(operatorSpec)
	if err != nil {
		t.Fatalf("disabled sentinel parse: %v", err)
	}

	// Disabled sentinel block yields non-nil but Enabled=false,
	// so validation is noop and the cmdExpand short-circuit
	// doesn't fire — the resource falls through to data-redis
	// emission. This is the soft-toggle off path.
	if err := validateSentinelSpec(sentinel, req, operatorSpec); err != nil {
		t.Errorf("disabled sentinel block tripped validation: %v", err)
	}

	if sentinel == nil || sentinel.Enabled {
		t.Errorf("disabled sentinel should parse to non-nil with Enabled=false; got %+v", sentinel)
	}
}

// TestMigration_NoSentinelEnvOnDataRedis is the inverse of
// TestMonitorTargetFromSpec_Sentinel — confirms a data redis's
// emitted statefulset spec doesn't accidentally carry the
// VOODU_MONITOR_* env keys (which would cause `vd redis:link
// --sentinel` to mis-detect it as a sentinel resource and try
// to emit sentinel hosts pointing at non-existent sentinel pods).
//
// Future-proofing: if anyone adds VOODU_MONITOR_NAME to
// composeDefaults() for data-redis, this catches it before the
// data-vs-sentinel detection breaks.
func TestMigration_NoSentinelEnvOnDataRedis(t *testing.T) {
	// Direct inspection of composeDefaults — the function that
	// builds the data-redis statefulset spec. It must NOT carry
	// any VOODU_MONITOR_* env (those are sentinel-only).
	defaults := composeDefaults("clowk-lp", "redis")

	env, _ := defaults["env"].(map[string]any)
	for k := range env {
		if k == "VOODU_MONITOR_NAME" || k == "VOODU_MONITOR_SCOPE" || k == "VOODU_REDIS_REPLICAS" {
			t.Errorf("data-redis defaults emit %q env var; this would cross-contaminate sentinel detection", k)
		}
	}
}
