package main

import (
	"reflect"
	"testing"
)

// TestVolumeTarget pins the parser for `src:dst[:mode]` strings.
// Single-token strings (no colon) return "" so the caller can
// route them to a "raw" bucket — better than guessing a target
// path that doesn't exist.
func TestVolumeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/host:/etc/redis/redis.conf:ro", "/etc/redis/redis.conf"},
		{"/host:/etc/redis/redis.conf", "/etc/redis/redis.conf"},
		{"${asset.X.Y}:/data:rw", "/data"},
		{"src:dst", "dst"},
		{"single-token-no-colon", ""},
		{"", ""},
		{"/host:/dst:ro:extra", "/dst"},
	}

	for _, tc := range cases {
		if got := volumeTarget(tc.in); got != tc.want {
			t.Errorf("volumeTarget(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMergeVolumes_NoOperator: the contract says plugin defaults
// always show up. With nil/empty operator input, output equals
// defaults verbatim.
func TestMergeVolumes_NoOperator(t *testing.T) {
	defaults := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
	}

	got := mergeVolumes(defaults, nil)

	if !reflect.DeepEqual(got, defaults) {
		t.Errorf("nil operator should pass defaults through:\n  got:  %v\n  want: %v", got, defaults)
	}

	got = mergeVolumes(defaults, []any{})

	if !reflect.DeepEqual(got, defaults) {
		t.Errorf("empty []any operator should also pass defaults through:\n  got:  %v\n  want: %v", got, defaults)
	}
}

// TestMergeVolumes_OperatorAddsNewTarget: operator entry whose
// destination path is NOT in defaults appends to the final list.
// Plugin defaults stay intact, operator additions follow.
func TestMergeVolumes_OperatorAddsNewTarget(t *testing.T) {
	defaults := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
	}

	operator := []any{
		"${asset.x.acls}:/etc/redis/users.acl:ro",
	}

	got := mergeVolumes(defaults, operator)

	want := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
		"${asset.x.acls}:/etc/redis/users.acl:ro",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("add new target:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestMergeVolumes_OperatorOverridesSameTarget: operator entry
// whose destination path matches a default REPLACES that default
// in place. Final list has one entry per target, operator wins
// when targets collide. Crucial for avoiding docker's
// "duplicate mount point" error.
func TestMergeVolumes_OperatorOverridesSameTarget(t *testing.T) {
	defaults := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
	}

	operator := []any{
		"${asset.y.custom}:/etc/redis/redis.conf:ro",
	}

	got := mergeVolumes(defaults, operator)

	want := []any{
		"${asset.y.custom}:/etc/redis/redis.conf:ro",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("override same target:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestMergeVolumes_AddAndOverrideTogether: combo case from the
// design doc. Operator simultaneously overrides one default and
// adds a new entry. Final order: overrides keep position from
// defaults; new adds go to the end.
func TestMergeVolumes_AddAndOverrideTogether(t *testing.T) {
	defaults := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
	}

	operator := []any{
		"${asset.y.custom}:/etc/redis/redis.conf:ro",
		"${asset.x.acls}:/etc/redis/users.acl:ro",
	}

	got := mergeVolumes(defaults, operator)

	want := []any{
		"${asset.y.custom}:/etc/redis/redis.conf:ro",
		"${asset.x.acls}:/etc/redis/users.acl:ro",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("add+override:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestMergeVolumes_OperatorOnly: with no defaults (e.g., a future
// plugin that has no volumes baseline), operator entries flow
// through verbatim. Same dedup-by-target rules apply if the
// operator has duplicates.
func TestMergeVolumes_OperatorOnly(t *testing.T) {
	operator := []any{
		"${asset.x.acls}:/etc/redis/users.acl:ro",
		"${asset.y.cfg}:/etc/redis/redis.conf:ro",
	}

	got := mergeVolumes(nil, operator)

	if !reflect.DeepEqual(got, operator) {
		t.Errorf("operator-only:\n  got:  %v\n  want: %v", got, operator)
	}
}

// TestMergeVolumes_BothNilReturnsNil: when neither side declares
// volumes, return nil rather than an empty slice. Avoids emitting
// `"volumes": []` in the manifest JSON when there's nothing to
// say — keeps the spec minimal.
func TestMergeVolumes_BothNilReturnsNil(t *testing.T) {
	got := mergeVolumes(nil, nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestMergeVolumes_OperatorDuplicatesItself: if the operator
// declares two entries with the same target, the LATER one
// wins (matches HCL's last-wins semantics for repeated keys).
// Without this, docker would reject the run with
// "Duplicate mount point".
func TestMergeVolumes_OperatorDuplicatesItself(t *testing.T) {
	operator := []any{
		"${asset.first}:/etc/redis/redis.conf:ro",
		"${asset.second}:/etc/redis/redis.conf:ro",
	}

	got := mergeVolumes(nil, operator)

	want := []any{
		"${asset.second}:/etc/redis/redis.conf:ro",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("operator-dedup:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestMergeVolumes_MalformedEntriesPreserved: entries without a
// colon (e.g. `volumes = ["broken"]` typo) are kept verbatim
// under a synthetic "_raw:" target so they propagate to docker
// — which surfaces the malformed mount as the real error
// instead of the plugin silently dropping them.
func TestMergeVolumes_MalformedEntriesPreserved(t *testing.T) {
	operator := []any{
		"broken-no-colon",
		"${asset.x}:/data:ro",
	}

	got := mergeVolumes(nil, operator)

	want := []any{
		"broken-no-colon",
		"${asset.x}:/data:ro",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("malformed entries preserved:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestMergeEnv_DeepMerge confirms the existing env-merge contract
// stays intact: defaults and operator entries coexist, operator
// wins on key collision.
func TestMergeEnv_DeepMerge(t *testing.T) {
	defaults := map[string]any{
		"REDIS_LOG_LEVEL": "notice",
	}

	operator := map[string]any{
		"REDIS_LOG_LEVEL": "debug", // override
		"SKIP_FIX_PERMS":  "1",     // add
	}

	got, _ := mergeEnv(defaults, operator).(map[string]any)

	if got["REDIS_LOG_LEVEL"] != "debug" {
		t.Errorf("operator should win on collision, got %v", got["REDIS_LOG_LEVEL"])
	}

	if got["SKIP_FIX_PERMS"] != "1" {
		t.Errorf("operator-only key should be present, got %v", got["SKIP_FIX_PERMS"])
	}
}

// TestMergeEnv_BothEmptyReturnsNil: same posture as
// mergeVolumes — no env declared anywhere → nil so the manifest
// stays minimal.
func TestMergeEnv_BothEmptyReturnsNil(t *testing.T) {
	got := mergeEnv(nil, nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	got = mergeEnv(map[string]any{}, map[string]any{})
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestMergeSpec_OperatorWinsForUntypedKeys covers the alias
// contract for keys outside the special-case (env, volumes):
// operator declares `image = "redis:8"`, plugin had
// `image = "redis:7-alpine"` → operator wins outright.
func TestMergeSpec_OperatorWinsForUntypedKeys(t *testing.T) {
	defaults := composeDefaults("data", "cache")

	operator := map[string]any{
		"image":    "redis:8",
		"replicas": float64(3),
	}

	got := mergeSpec(defaults, operator)

	if got["image"] != "redis:8" {
		t.Errorf("image should be operator's, got %v", got["image"])
	}

	if got["replicas"] != float64(3) {
		t.Errorf("replicas should be operator's, got %v", got["replicas"])
	}

	// Untouched defaults still present.
	if got["ports"] == nil {
		t.Error("plugin's ports default should still be there")
	}
}

// TestMergeSpec_VolumesAdditiveOnRedisBlock simulates the user's
// real scenario: the operator declares an asset volume and
// expects both the plugin's defaults (redis.conf bind AND the
// entrypoint wrapper bind, post-M2) AND their addition to land
// in the final list.
func TestMergeSpec_VolumesAdditiveOnRedisBlock(t *testing.T) {
	defaults := composeDefaults("clowk-lp", "redis")

	operator := map[string]any{
		"volumes": []any{
			"${asset.clowk-lp.cdn.acls}:/etc/redis/users.acl:ro",
		},
	}

	got := mergeSpec(defaults, operator)

	vols, ok := got["volumes"].([]any)
	if !ok {
		t.Fatalf("volumes not a list: %T", got["volumes"])
	}

	if len(vols) != 3 {
		t.Errorf("expected 3 volumes (redis.conf + entrypoint + operator add), got %d: %v", len(vols), vols)
	}

	want := []any{
		"${asset.clowk-lp.redis.redis_conf}:/etc/redis/redis.conf:ro",
		"${asset.clowk-lp.redis.entrypoint}:/usr/local/bin/voodu-redis-entrypoint:ro",
		"${asset.clowk-lp.cdn.acls}:/etc/redis/users.acl:ro",
	}

	if !reflect.DeepEqual(vols, want) {
		t.Errorf("volumes:\n  got:  %v\n  want: %v", vols, want)
	}
}

// TestComposeDefaults_HasRequiredFields locks in the plugin's
// baseline contract: image, command, ports, volumes, volume_claims
// are always present so the operator can rely on them being
// there even with an empty `redis "..." "..." {}` block.
func TestComposeDefaults_HasRequiredFields(t *testing.T) {
	d := composeDefaults("data", "cache")

	required := []string{"image", "command", "ports", "volumes", "volume_claims", "replicas"}

	for _, k := range required {
		if d[k] == nil {
			t.Errorf("composeDefaults missing %q", k)
		}
	}

	// Volumes default must reference both assets emitted alongside:
	// the redis.conf bind (without it redis won't start) and the
	// entrypoint wrapper bind (without it the command path is a
	// directory and exec fails).
	vols, _ := d["volumes"].([]any)
	if len(vols) != 2 {
		t.Fatalf("expected 2 default volumes (redis.conf + entrypoint), got %d", len(vols))
	}

	wantVols := []any{
		"${asset.data.cache.redis_conf}:/etc/redis/redis.conf:ro",
		"${asset.data.cache.entrypoint}:/usr/local/bin/voodu-redis-entrypoint:ro",
	}
	if !reflect.DeepEqual(vols, wantVols) {
		t.Errorf("default volumes:\n  got:  %v\n  want: %v", vols, wantVols)
	}

	// Command default invokes the wrapper via `sh` (asset files
	// land non-executable; sh doesn't care about the +x bit).
	cmd, _ := d["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "sh" || cmd[1] != "/usr/local/bin/voodu-redis-entrypoint" {
		t.Errorf("default command unexpected: %v", cmd)
	}
}

// TestMergeSpec_EnvCoexistsWithDefaults pins env behaviour
// alongside the new volumes path — they're the two special-case
// keys, and they must not interfere.
func TestMergeSpec_EnvCoexistsWithDefaults(t *testing.T) {
	defaults := composeDefaults("data", "cache")

	operator := map[string]any{
		"env": map[string]any{
			"SKIP_FIX_PERMS": "1",
		},
	}

	got := mergeSpec(defaults, operator)

	env, ok := got["env"].(map[string]any)
	if !ok {
		t.Fatalf("env not a map: %T", got["env"])
	}

	if env["SKIP_FIX_PERMS"] != "1" {
		t.Errorf("operator env should be present, got %v", env)
	}

	// Volumes defaults still present (env merge doesn't touch
	// volumes). Both the redis.conf bind AND the entrypoint
	// wrapper bind must survive.
	vols, _ := got["volumes"].([]any)
	if len(vols) != 2 {
		t.Errorf("volumes defaults lost when operator declared env: %v", vols)
	}
}
