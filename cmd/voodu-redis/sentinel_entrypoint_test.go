package main

import (
	"strings"
	"testing"
)

// TestRenderSentinelEntrypointScript_ContractFields pins the env
// keys the entrypoint reads. Two flavours:
//
//   - VOODU_* keys: plugin-managed, baked into the statefulset
//     spec via sentinelPodEnv (plugin contract surface)
//   - REDIS_* keys: flow in via env_from from the monitor target's
//     config bucket (controller-managed plumbing)
//
// Renaming either side of either contract without the other =
// silent breakage at boot.
func TestRenderSentinelEntrypointScript_ContractFields(t *testing.T) {
	script := renderSentinelEntrypointScript()

	mustContain := []string{
		"VOODU_MONITOR_SCOPE",
		"VOODU_MONITOR_NAME",
		"VOODU_REDIS_REPLICAS",
		"REDIS_MASTER_ORDINAL", // flows via env_from from monitor target
		"REDIS_PASSWORD",       // ditto
	}

	for _, key := range mustContain {
		if !strings.Contains(script, key) {
			t.Errorf("entrypoint script missing required env reference %q", key)
		}
	}

	// And the OLD baked-at-expand env should be GONE — its purpose
	// (telling the entrypoint which ordinal is master) is now
	// served by REDIS_MASTER_ORDINAL via env_from.
	if strings.Contains(script, "VOODU_MASTER_ORDINAL_INITIAL") {
		t.Errorf("VOODU_MASTER_ORDINAL_INITIAL should be removed (replaced by REDIS_MASTER_ORDINAL via env_from)")
	}
}

// TestRenderSentinelEntrypointScript_QuorumFormula confirms the
// shell math matches the validation formula in checkSentinelReplicas
// (replicas/2 + 1). If these drift, validation might accept a
// replicas count that produces a runtime quorum mismatch — bad.
func TestRenderSentinelEntrypointScript_QuorumFormula(t *testing.T) {
	script := renderSentinelEntrypointScript()

	if !strings.Contains(script, "VOODU_REDIS_REPLICAS / 2 + 1") {
		t.Errorf("entrypoint quorum formula drifted; want `VOODU_REDIS_REPLICAS / 2 + 1` in script")
	}
}

// TestRenderSentinelEntrypointScript_FQDNScheme: the master FQDN
// must follow voodu0's <name>-<ord>.<scope>.voodu convention.
// Tests pin both the scoped and unscoped branches.
func TestRenderSentinelEntrypointScript_FQDNScheme(t *testing.T) {
	script := renderSentinelEntrypointScript()

	// Scoped branch
	if !strings.Contains(script, "${VOODU_MONITOR_NAME}-${MASTER_ORDINAL}.${VOODU_MONITOR_SCOPE}.voodu") {
		t.Errorf("scoped FQDN expression missing or drifted from voodu0 scheme")
	}

	// Unscoped branch
	if !strings.Contains(script, "${VOODU_MONITOR_NAME}-${MASTER_ORDINAL}.voodu") {
		t.Errorf("unscoped FQDN expression missing or drifted from voodu0 scheme")
	}
}

// TestRenderSentinelEntrypointScript_SentinelDirectives confirms
// the conf written by the entrypoint includes the directives
// sentinel actually needs. Missing `sentinel monitor` would mean
// sentinel boots with nothing to watch; missing the
// client-reconfig-script line would orphan auto-failover events
// from voodu (the M-S2 callback wouldn't fire).
func TestRenderSentinelEntrypointScript_SentinelDirectives(t *testing.T) {
	script := renderSentinelEntrypointScript()

	mustContain := []string{
		"sentinel monitor mymaster",
		"sentinel down-after-milliseconds mymaster",
		"sentinel failover-timeout mymaster",
		"sentinel parallel-syncs mymaster",
		"sentinel client-reconfig-script mymaster",
		"sentinel resolve-hostnames yes",
		"sentinel announce-hostnames yes",
	}

	for _, directive := range mustContain {
		if !strings.Contains(script, directive) {
			t.Errorf("entrypoint conf-write missing directive %q", directive)
		}
	}
}

// TestRenderSentinelEntrypointScript_PreflightCheck pins M-S4:
// the entrypoint runs a `wget` GET against /describe of the
// monitor target before booting sentinel. Catches operator
// typos (wrong scope/name in HCL) at boot rather than after
// sentinel logs "master unreachable" 10 times.
//
// Best-effort and non-fatal — if the controller is unreachable
// or VOODU_CONTROLLER_URL isn't set, we skip and proceed.
func TestRenderSentinelEntrypointScript_PreflightCheck(t *testing.T) {
	script := renderSentinelEntrypointScript()

	if !strings.Contains(script, "/describe?kind=statefulset&scope=") {
		t.Errorf("entrypoint should call /describe to verify monitor target exists")
	}

	if !strings.Contains(script, "preflight OK") {
		t.Errorf("entrypoint should log a recognizable success line")
	}

	if !strings.Contains(script, "WARNING — monitor target") {
		t.Errorf("entrypoint should log a recognizable warning when target missing")
	}

	// Must NOT exit on preflight failure — sentinel can still
	// useful start; the boot log is the operator signal.
	preflightSection := script
	if i := strings.Index(preflightSection, "preflight"); i >= 0 {
		preflightSection = preflightSection[i:]
	}

	if strings.Contains(preflightSection, "exit 1") {
		t.Errorf("preflight failure must NOT exit (continue to sentinel boot, log warning instead)")
	}
}

// TestRenderSentinelEntrypointScript_AuthPassConditional pins
// the "REDIS_PASSWORD set → emit auth-pass; not set → skip" flow.
// If we hardcoded auth-pass with empty value, sentinel would log
// auth-failures every monitor cycle.
func TestRenderSentinelEntrypointScript_AuthPassConditional(t *testing.T) {
	script := renderSentinelEntrypointScript()

	if !strings.Contains(script, `if [ -n "${REDIS_PASSWORD:-}" ]; then`) {
		t.Errorf("auth-pass should be conditional on REDIS_PASSWORD being non-empty")
	}

	if !strings.Contains(script, "sentinel auth-pass mymaster $REDIS_PASSWORD") {
		t.Errorf("auth-pass directive shape drifted")
	}
}

// TestRenderSentinelEntrypointScript_ExecsRedisServerWithSentinel
// pins the exec form. We use `redis-server $CONF --sentinel`
// (canonical Redis 2.8+ invocation) instead of bare
// `redis-sentinel`. Functionally equivalent (sentinel is a
// symlink to redis-server with --sentinel baked in) but works
// against minimal images that don't ship the symlink.
func TestRenderSentinelEntrypointScript_ExecsRedisServerWithSentinel(t *testing.T) {
	script := renderSentinelEntrypointScript()

	if !strings.Contains(script, "exec redis-server") {
		t.Fatal("sentinel entrypoint should exec redis-server with --sentinel flag")
	}

	if !strings.Contains(script, "--sentinel") {
		t.Fatal("redis-server invocation MUST carry --sentinel (otherwise runs as a regular redis-server)")
	}

	// Also confirm we don't accidentally exec the bare binary
	// (would work on most images but fails on minimal ones —
	// this is the WHOLE reason we picked the redis-server form).
	if strings.Contains(script, "exec redis-sentinel") {
		t.Errorf("entrypoint should NOT exec the bare `redis-sentinel` binary (use `redis-server --sentinel` for image portability)")
	}
}

// TestRenderSentinelEntrypointScript_OperatorOverrideInclude
// pins the conf.d include — same operator-override pattern the
// data redis uses for ACLs. Without this, overriding sentinel
// directives (down-after-milliseconds, failover-timeout, etc.)
// requires re-emitting the whole bootstrap conf, which fights
// the plugin.
func TestRenderSentinelEntrypointScript_OperatorOverrideInclude(t *testing.T) {
	script := renderSentinelEntrypointScript()

	if !strings.Contains(script, "include /etc/sentinel/conf.d/*.conf") {
		t.Fatal("sentinel.conf must `include /etc/sentinel/conf.d/*.conf` for operator overrides")
	}

	// And the directory must be mkdir'd so an empty conf.d
	// (no overrides) doesn't error out the include glob.
	if !strings.Contains(script, "/etc/sentinel/conf.d") {
		t.Errorf("entrypoint should ensure /etc/sentinel/conf.d exists at boot")
	}
}

// TestRenderSentinelHookScript_AlwaysExits0 pins the
// "exit 0 even on failure" decision. Sentinel treats non-zero
// from client-reconfig-script as a failed callback and may log
// noisily, but doesn't undo the failover. So returning 0 even
// when the controller callback fails just means "I tried; the
// store may be stale" rather than spamming sentinel logs with
// retry attempts the operator can't act on.
func TestRenderSentinelHookScript_AlwaysExits0(t *testing.T) {
	hook := renderSentinelHookScript()

	// Multiple exit 0 paths: state != end, missing controller URL,
	// parse failure, callback success, callback exhaustion. None
	// should be exit 1.
	if strings.Contains(hook, "exit 1") {
		t.Errorf("hook must NEVER exit 1 — sentinel can't act on failure, would just log spam")
	}

	if !strings.Contains(hook, "voodu-sentinel-hook:") {
		t.Errorf("hook should log a recognizable prefix for grep-ability in sentinel logs")
	}
}

// TestRenderSentinelHookScript_AcceptsSentinelArgs pins that the
// hook reads the 7 positional args sentinel actually passes. If
// we shifted the indices, the callback would write the wrong
// master ordinal back to voodu store.
func TestRenderSentinelHookScript_AcceptsSentinelArgs(t *testing.T) {
	hook := renderSentinelHookScript()

	wantRefs := []string{
		`MASTER_NAME="${1:-?}"`,
		`ROLE="${2:-?}"`,
		`STATE="${3:-?}"`,
		`FROM_IP="${4:-?}"`,
		`FROM_PORT="${5:-?}"`,
		`TO_IP="${6:-?}"`,
		`TO_PORT="${7:-?}"`,
	}

	for _, ref := range wantRefs {
		if !strings.Contains(hook, ref) {
			t.Errorf("hook missing positional arg ref %q (sentinel arg order is fixed)", ref)
		}
	}
}

// TestRenderSentinelHookScript_OnlyActsOnEnd: sentinel fires the
// hook on multiple events (start, end). Only the end state has
// a finalised new master to record — acting on start would write
// a stale ordinal because $TO_IP doesn't yet hold the new master.
func TestRenderSentinelHookScript_OnlyActsOnEnd(t *testing.T) {
	hook := renderSentinelHookScript()

	if !strings.Contains(hook, `if [ "$STATE" != "end" ]; then`) {
		t.Errorf("hook should short-circuit on STATE != end")
	}
}

// TestRenderSentinelHookScript_CallsBackWithNoRestart pins the
// callback semantics: the hook MUST pass --no-restart so voodu
// updates the store WITHOUT rolling the redis statefulset
// (which would drop active connections on the freshly promoted
// master and risk ping-ponging with sentinel).
func TestRenderSentinelHookScript_CallsBackWithNoRestart(t *testing.T) {
	hook := renderSentinelHookScript()

	if !strings.Contains(hook, "--no-restart") {
		t.Fatal("hook callback MUST pass --no-restart (the entire point of this path)")
	}

	if !strings.Contains(hook, "/plugin/redis/failover") {
		t.Errorf("hook should POST to /plugin/redis/failover endpoint")
	}
}

// TestRenderSentinelHookScript_OrdinalParse: the regex extracting
// the ordinal from $TO_IP must match the FQDN scheme voodu0 uses
// (<name>-<ord>.<scope>.voodu). If the scheme drifts, sentinel
// auto-failover sync silently breaks.
func TestRenderSentinelHookScript_OrdinalParse(t *testing.T) {
	hook := renderSentinelHookScript()

	if !strings.Contains(hook, `sed -nE 's/^[a-z0-9-]+-([0-9]+)\..*/\1/p'`) {
		t.Errorf("ordinal-extraction regex changed; review compatibility with voodu0 FQDN scheme")
	}
}

// TestRenderSentinelHookScript_RetriesWithBackoff pins the retry
// surface — 5 attempts with exponential backoff, total ~31s
// worst case. Bounded so sentinel doesn't get blocked on a slow
// callback indefinitely (sentinel fires this synchronously).
func TestRenderSentinelHookScript_RetriesWithBackoff(t *testing.T) {
	hook := renderSentinelHookScript()

	if !strings.Contains(hook, `while [ "$ATTEMPT" -lt 5 ]`) {
		t.Errorf("retry loop should cap at 5 attempts")
	}

	if !strings.Contains(hook, "SLEEP=$((SLEEP * 2))") {
		t.Errorf("backoff should double each iteration")
	}
}

// TestRenderSentinelHookScript_GracefulDegradationWhenURLMissing:
// when VOODU_CONTROLLER_URL isn't set (operator forgot to inject,
// or single-VM dev without a routable URL), the hook logs the
// degradation and exits 0 — the failover still happened inside
// Redis; only the voodu store stays stale until the operator
// runs a manual `vd redis:failover` to fix.
func TestRenderSentinelHookScript_GracefulDegradationWhenURLMissing(t *testing.T) {
	hook := renderSentinelHookScript()

	if !strings.Contains(hook, `if [ -z "${VOODU_CONTROLLER_URL:-}" ]; then`) {
		t.Errorf("hook should check VOODU_CONTROLLER_URL before the callback path")
	}

	if !strings.Contains(hook, "store will be stale") {
		t.Errorf("missing-URL log should mention the stale-store consequence so operator knows")
	}
}

// TestComposeSentinelDefaults_Shape pins the keys downstream
// statefulset machinery expects. Missing `command` → controller
// uses the image ENTRYPOINT (probably redis-server); missing
// `ports` → consumers can't reach sentinel; missing `volumes`
// → the entrypoint script isn't on disk to exec.
func TestComposeSentinelDefaults_Shape(t *testing.T) {
	d := composeSentinelDefaults("clowk-lp", "redis-quorum")

	requiredKeys := []string{"image", "replicas", "command", "ports", "volumes"}
	for _, k := range requiredKeys {
		if _, ok := d[k]; !ok {
			t.Errorf("sentinel defaults missing required key %q", k)
		}
	}

	if d["replicas"] != 3 {
		t.Errorf("sentinel default replicas = %v, want 3 (HA minimum)", d["replicas"])
	}

	cmd, ok := d["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[0] != "sh" || cmd[1] != sentinelEntrypointMountPath {
		t.Errorf("command should invoke `sh %s`, got %v", sentinelEntrypointMountPath, d["command"])
	}

	ports, ok := d["ports"].([]any)
	if !ok || len(ports) != 1 || ports[0] != "26379" {
		t.Errorf("ports should be [26379], got %v", d["ports"])
	}
}

// TestComposeSentinelDefaults_VolumeBinds checks both asset
// references resolve to the right scope/name and mount paths.
// Asset path mismatch = entrypoint script not on disk = boot
// failure with "no such file" — easy to debug but worth pinning.
func TestComposeSentinelDefaults_VolumeBinds(t *testing.T) {
	d := composeSentinelDefaults("clowk-lp", "redis-quorum")

	vols, _ := d["volumes"].([]any)

	wantEntrypoint := "${asset.clowk-lp.redis-quorum." + sentinelEntrypointAssetKey + "}:" + sentinelEntrypointMountPath + ":ro"
	wantHook := "${asset.clowk-lp.redis-quorum." + sentinelHookAssetKey + "}:" + sentinelHookMountPath + ":ro"

	found := map[string]bool{}
	for _, v := range vols {
		s, _ := v.(string)
		found[s] = true
	}

	if !found[wantEntrypoint] {
		t.Errorf("missing entrypoint volume bind: %q", wantEntrypoint)
	}

	if !found[wantHook] {
		t.Errorf("missing hook volume bind: %q", wantHook)
	}
}

// TestSentinelPodEnv_EmitsContractKeys: the env map the plugin
// puts into the statefulset spec must contain exactly the keys
// the entrypoint script reads. This pairs with
// TestRenderSentinelEntrypointScript_ContractFields — both sides
// of the contract pinned.
//
// REDIS_PASSWORD and REDIS_MASTER_ORDINAL are NOT in this map —
// they flow via env_from from the monitor target's bucket, set
// up automatically by sentinelManifests.
func TestSentinelPodEnv_EmitsContractKeys(t *testing.T) {
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}
	env := sentinelPodEnv(s, 3)

	want := map[string]string{
		"VOODU_MONITOR_SCOPE":  "clowk-lp",
		"VOODU_MONITOR_NAME":   "redis",
		"VOODU_REDIS_REPLICAS": "3",
	}

	for k, v := range want {
		if got := env[k]; got != v {
			t.Errorf("env[%q] = %q, want %q", k, got, v)
		}
	}

	if len(env) != len(want) {
		t.Errorf("unexpected extra env keys: got %v, want %v", env, want)
	}

	// REDIS_PASSWORD must NOT be baked here — it flows via
	// env_from. Baking it would (1) leak the password into the
	// statefulset spec wire shape, (2) require re-emission on
	// password rotation, (3) make link rotation logic harder.
	if _, present := env["REDIS_PASSWORD"]; present {
		t.Errorf("REDIS_PASSWORD must NOT be in plugin pod env (flows via env_from)")
	}

	if _, present := env["REDIS_MASTER_ORDINAL"]; present {
		t.Errorf("REDIS_MASTER_ORDINAL must NOT be in plugin pod env (flows via env_from)")
	}
}

// TestSentinelManifests_EmitsEnvFromMonitor pins THE auto-plumbing
// that makes the operator's HCL minimal. The sentinel statefulset
// MUST emit env_from = [<monitor>] so REDIS_PASSWORD and
// REDIS_MASTER_ORDINAL flow in from the data redis's bucket
// without operator gymnastics.
func TestSentinelManifests_EmitsEnvFromMonitor(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-ha"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

	got := sentinelManifests(req, s, map[string]any{"replicas": 3})

	envFrom, _ := got[1].Spec["env_from"].([]string)
	if len(envFrom) != 1 || envFrom[0] != "clowk-lp/redis" {
		t.Errorf("env_from should be [\"clowk-lp/redis\"], got %v", envFrom)
	}
}

// TestSentinelManifests_OperatorEnvFromCoexists: when the operator
// declares their own env_from refs (e.g., shared secrets bucket),
// the plugin keeps those AND appends the monitor ref. The monitor
// ref lands LAST so its values win — same last-wins semantics
// jobs already use.
func TestSentinelManifests_OperatorEnvFromCoexists(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-ha"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

	op := map[string]any{
		"replicas": 3,
		"env_from": []any{"infra/shared-secrets"},
	}

	got := sentinelManifests(req, s, op)

	envFrom, _ := got[1].Spec["env_from"].([]string)
	if len(envFrom) != 2 {
		t.Fatalf("expected 2 env_from entries, got %d: %v", len(envFrom), envFrom)
	}

	if envFrom[0] != "infra/shared-secrets" {
		t.Errorf("operator entry should be first, got %q", envFrom[0])
	}

	if envFrom[1] != "clowk-lp/redis" {
		t.Errorf("monitor ref should be last (wins on collision), got %q", envFrom[1])
	}
}

// TestSentinelManifests_NoDuplicateMonitorInEnvFrom: if the
// operator (oddly) wrote `env_from = ["clowk-lp/redis"]` already
// pointing at the monitor target, mergeEnvFrom dedupes — final
// list has the monitor ref once, at the end.
func TestSentinelManifests_NoDuplicateMonitorInEnvFrom(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-ha"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

	op := map[string]any{
		"replicas": 3,
		"env_from": []any{"clowk-lp/redis"},
	}

	got := sentinelManifests(req, s, op)

	envFrom, _ := got[1].Spec["env_from"].([]string)
	if len(envFrom) != 1 || envFrom[0] != "clowk-lp/redis" {
		t.Errorf("duplicate monitor ref should be deduped, got %v", envFrom)
	}
}

// TestSentinelReplicas_DefaultIs3 confirms operator omitting
// `replicas` in sentinel mode falls to 3, NOT to data-redis's
// default of 1. Sentinel resource defaults differ because the
// HA story differs.
func TestSentinelReplicas_DefaultIs3(t *testing.T) {
	cases := []struct {
		spec map[string]any
		want int
	}{
		{nil, 3},
		{map[string]any{}, 3},
		{map[string]any{"image": "redis:8"}, 3}, // omitted
		{map[string]any{"replicas": 1}, 1},
		{map[string]any{"replicas": 5}, 5},
		{map[string]any{"replicas": float64(7)}, 7}, // JSON-decoded number
	}

	for _, tc := range cases {
		got := sentinelReplicas(tc.spec)
		if got != tc.want {
			t.Errorf("sentinelReplicas(%v) = %d, want %d", tc.spec, got, tc.want)
		}
	}
}

// TestSentinelManifests_PairShape: the sentinel-mode emit must
// produce exactly one asset + one statefulset, in that order.
// Asset before statefulset matters because the statefulset
// mounts the asset's files on first start — reversing the order
// would mean a race where the volume is bound before the file
// exists on the host.
func TestSentinelManifests_PairShape(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}
	op := map[string]any{"replicas": 3}

	got := sentinelManifests(req, s, op)

	if len(got) != 2 {
		t.Fatalf("expected 2 manifests (asset + statefulset), got %d", len(got))
	}

	if got[0].Kind != "asset" {
		t.Errorf("first manifest should be asset, got %q", got[0].Kind)
	}

	if got[1].Kind != "statefulset" {
		t.Errorf("second manifest should be statefulset, got %q", got[1].Kind)
	}

	for i, m := range got {
		if m.Scope != req.Scope || m.Name != req.Name {
			t.Errorf("manifest[%d] (scope, name) = (%q, %q), want (%q, %q)",
				i, m.Scope, m.Name, req.Scope, req.Name)
		}
	}
}

// TestSentinelManifests_AssetCarriesBothScripts confirms the
// asset emits BOTH the entrypoint AND the failover hook. Missing
// the hook would mean sentinel's client-reconfig-script directive
// points at a non-existent path → noisy logs, no failover sync.
func TestSentinelManifests_AssetCarriesBothScripts(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}

	got := sentinelManifests(req, s, map[string]any{"replicas": 3})

	files, _ := got[0].Spec["files"].(map[string]any)

	if _, ok := files[sentinelEntrypointAssetKey]; !ok {
		t.Errorf("asset missing %q file", sentinelEntrypointAssetKey)
	}

	if _, ok := files[sentinelHookAssetKey]; !ok {
		t.Errorf("asset missing %q file", sentinelHookAssetKey)
	}

	// Sanity: entrypoint must be the sentinel one, not a stale
	// data-redis entrypoint.
	entrypoint, _ := files[sentinelEntrypointAssetKey].(string)
	if !hasSentinelMonitorRef(entrypoint) {
		t.Errorf("entrypoint asset doesn't look like the sentinel script (no `sentinel monitor` directive)")
	}
}

// TestSentinelManifests_StatefulsetEnvMerged: the plugin's own
// VOODU_* keys must coexist with operator-declared env. Operator
// can't override the contract keys (they're plugin-private), but
// can ADD their own (e.g., LOG_LEVEL).
func TestSentinelManifests_StatefulsetEnvMerged(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}
	op := map[string]any{
		"replicas": 3,
		"env": map[string]any{
			"OPERATOR_VAR": "hello",
		},
	}

	got := sentinelManifests(req, s, op)

	env, _ := got[1].Spec["env"].(map[string]any)

	if env["OPERATOR_VAR"] != "hello" {
		t.Errorf("operator env var lost in merge; got: %v", env)
	}

	if env["VOODU_MONITOR_NAME"] != "redis" {
		t.Errorf("plugin contract env missing; got: %v", env)
	}
}

// TestSentinelManifests_StripsBlock: the sentinel block is parsed
// + validated then must not leak into the statefulset spec
// downstream — no consumer of statefulset spec knows what
// `sentinel: {...}` means.
func TestSentinelManifests_StripsBlock(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}
	op := map[string]any{
		"replicas": 3,
		"sentinel": map[string]any{"enabled": true, "monitor": "clowk-lp/redis"},
	}

	got := sentinelManifests(req, s, op)

	if _, present := got[1].Spec["sentinel"]; present {
		t.Errorf("sentinel block leaked into statefulset spec: %+v", got[1].Spec)
	}
}

// TestSentinelManifests_OperatorImageWins ensures the alias
// contract holds: operator's `image = "redis:8"` overrides the
// plugin's redis:7-alpine default. Same merge rule as data-redis.
func TestSentinelManifests_OperatorImageWins(t *testing.T) {
	req := expandRequest{Scope: "clowk-lp", Name: "redis-quorum"}
	s := &sentinelSpec{Enabled: true, Monitor: "clowk-lp/redis"}
	op := map[string]any{"image": "redis:8", "replicas": 3}

	got := sentinelManifests(req, s, op)

	if got[1].Spec["image"] != "redis:8" {
		t.Errorf("operator image override lost; got: %v", got[1].Spec["image"])
	}
}
