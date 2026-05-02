// Plugin command implementations for `vd redis:link`,
// `vd redis:unlink`, and `vd redis:new-password`. Each command:
//
//  1. Reads its argv from os.Args[2:] (provider/consumer refs).
//  2. Reads invocation context from stdin (a small JSON
//     envelope the controller writes) — `controller_url` and
//     `plugin_dir` are the only fields we use today.
//  3. Calls back to the controller's HTTP API to fetch the
//     state it needs (provider's manifest spec + config).
//  4. Builds the connection URL, emits an `actions` list the
//     controller applies on the consumer's config bucket.
//
// The plugin is fully autonomous — it owns its argv shape,
// owns its state lookups, owns its action emission. CLI and
// server are dumb passthrough.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// invocationContext is the JSON envelope the controller writes
// to stdin. Plugin reads it once at startup to learn where to
// call back. Args don't appear here — they arrive via os.Args.
type invocationContext struct {
	Plugin        string `json:"plugin"`
	Command       string `json:"command"`
	ControllerURL string `json:"controller_url,omitempty"`
	PluginDir     string `json:"plugin_dir,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
}

// dispatchOutput is the envelope-data shape the controller
// expects on stdout. `message` is the operator-facing one-liner;
// `actions` is the queue the controller applies post-invoke.
type dispatchOutput struct {
	Message string           `json:"message"`
	Actions []dispatchAction `json:"actions"`
}

type dispatchAction struct {
	Type  string            `json:"type"`
	Scope string            `json:"scope"`
	Name  string            `json:"name"`
	KV    map[string]string `json:"kv,omitempty"`
	Keys  []string          `json:"keys,omitempty"`

	// SkipRestart asks the controller to apply this config write
	// WITHOUT triggering the usual restart fan-out on (Scope, Name).
	// Default false (omitted in JSON) — every existing action keeps
	// the historical "config_set → rolling restart" semantics.
	//
	// Used by `vd redis:failover --no-restart` for the sentinel
	// auto-failover callback path: sentinel has already moved
	// roles inside Redis (SLAVEOF NO ONE on new master, SLAVEOF X
	// on replicas), so restarting the redis pods would (a) drop
	// active connections needlessly and (b) risk a ping-pong with
	// sentinel re-electing while the master pod is mid-reboot.
	// Consumer URLs still need to refresh, so the consumer-targeted
	// actions in the same envelope keep SkipRestart=false.
	SkipRestart bool `json:"skip_restart,omitempty"`
}

// consumerEnvVar is the env-var name redis:link sets on the
// consumer to carry the connection URL. Matches the de facto
// standard for redis client libraries (REDIS_URL is what
// node-redis, redis-py, ioredis, go-redis all check first).
const consumerEnvVar = "REDIS_URL"

// consumerReadEnvVar is the read-pool URL emitted alongside
// REDIS_URL when the provider has replicas > 1 and the consumer
// did NOT pass --reads. The convention matches Sidekiq's
// REDIS_PROVIDER + REDIS_READ_URL pattern, picked up by Rails
// 6+ replica-aware connection pooling.
const consumerReadEnvVar = "REDIS_READ_URL"

// consumerSentinelHostsEnvVar lists the sentinel pod endpoints
// (comma-separated host:port). Emitted by `vd redis:link --sentinel`
// alongside REDIS_URL/REDIS_READ_URL — sentinel-aware clients
// (ioredis, redis-py with `Sentinel(...)`, lettuce, redis-rb
// `Redis.new(sentinels: [...])`) read this to discover the
// current master at runtime instead of trusting REDIS_URL through
// failover events.
const consumerSentinelHostsEnvVar = "REDIS_SENTINEL_HOSTS"

// consumerMasterNameEnvVar matches the `sentinel monitor <name>`
// directive in sentinel.conf — clients ask the sentinels "what's
// the address of <name>?" to resolve the current master. The
// plugin always uses the constant `mymaster` (matching the
// entrypoint's sentinel.conf), so this env value is fixed; it's
// emitted explicitly so consumers don't have to assume.
const consumerMasterNameEnvVar = "REDIS_MASTER_NAME"

// linkedConsumersKey is the config-bucket key on the provider
// (the redis itself) that lists every consumer currently linked
// to it. Comma-separated `scope/name` refs. Maintained by
// cmdLink (add) and cmdUnlink (remove); used by cmdNewPassword
// to auto-refresh every linked consumer's URL with the new
// password without the operator manually re-running link.
//
// Unscoped consumers ride as `/<name>` (leading slash) so the
// parser can recover both halves from the comma-split.
const linkedConsumersKey = "REDIS_LINKED_CONSUMERS"

// cmdLink wires a redis provider to a consumer.
//
// Args (os.Args[2:], flags interleaved):
//
//	[positional 0] provider scope/name (e.g. "clowk-lp/redis")
//	[positional 1] consumer scope/name (e.g. "clowk-lp/web")
//	--reads        consumer is read-only; emit a single
//	               REDIS_URL pointing at the round-robin
//	               shared alias instead of the master+read
//	               split.
//
// Reads invocation context from stdin to find controller_url,
// then calls /describe and /config to gather the provider's
// state. Builds the URL(s) and emits config_set actions on the
// consumer (the URL injection) plus on the provider (linked-
// consumers tracking, for password rotation auto-refresh).
//
// URL emission logic:
//
//   - replicas <= 1: single REDIS_URL pointing at the shared
//     alias (`<name>.<scope>.voodu`). Pre-M2 behaviour.
//
//   - replicas > 1, no --reads: REDIS_URL pins the master
//     (`<name>-0.<scope>.voodu` — pod-0 by convention),
//     REDIS_READ_URL points at the round-robin shared alias.
//     Apps using the dual-URL pattern (Rails replica-aware
//     pools, custom Sidekiq fan-out) read from the pool and
//     write to the master.
//
//   - replicas > 1, --reads: ONE REDIS_URL on the round-robin
//     pool. Use this for read-heavy consumers that shouldn't
//     have a separate read URL (caching workers, dashboards).
//
// `-h` / `--help` short-circuits the network calls and prints
// usage on stdout for the operator.
func cmdLink() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(linkHelp)
		return nil
	}

	positional, readsOnly, sentinelMode := parseLinkFlags(args)

	if len(positional) < 2 {
		return fmt.Errorf("usage: vd redis:link [--reads] [--sentinel] <provider-scope/name> <consumer-scope/name>")
	}

	providerRef := positional[0]
	consumerRef := positional[1]

	provScope, provName := splitScopeName(providerRef)
	consScope, consName := splitScopeName(consumerRef)

	if provName == "" {
		return fmt.Errorf("invalid provider ref %q (expected scope/name)", providerRef)
	}

	if consName == "" {
		return fmt.Errorf("invalid consumer ref %q (expected scope/name)", consumerRef)
	}

	ctx, err := readInvocationContext()
	if err != nil {
		return err
	}

	client := newControllerClient(ctx.ControllerURL)

	// Plugins that emit a statefulset (today: every voodu-redis-
	// like plugin) fetch the spec via /describe.
	provSpec, err := client.fetchSpec("statefulset", provScope, provName)
	if err != nil {
		return fmt.Errorf("describe %s: %w", providerRef, err)
	}

	// When the provider is a sentinel resource (detected by the
	// VOODU_MONITOR_NAME env baked at expand time), URL building
	// pivots on the MONITORED data redis: passwords, master ordinal,
	// connection FQDNs all derive from the target, not the sentinel
	// itself. The sentinel resource only contributes the discovery
	// endpoints (REDIS_SENTINEL_HOSTS) when --sentinel is passed.
	urlScope, urlName := provScope, provName
	urlSpec := provSpec
	urlConfig, err := client.fetchConfig(provScope, provName)

	if err != nil {
		return fmt.Errorf("config get %s: %w", providerRef, err)
	}

	monitorTarget, isSentinelResource := monitorTargetFromSpec(provSpec)

	if sentinelMode && !isSentinelResource {
		return fmt.Errorf("--sentinel requires the provider to be a sentinel resource (with `sentinel { enabled = true }` block); %s appears to be a data redis", providerRef)
	}

	if isSentinelResource {
		urlScope, urlName = monitorTarget.scope, monitorTarget.name

		urlSpec, err = client.fetchSpec("statefulset", urlScope, urlName)
		if err != nil {
			return fmt.Errorf("describe monitor target %s/%s: %w", urlScope, urlName, err)
		}

		urlConfig, err = client.fetchConfig(urlScope, urlName)
		if err != nil {
			return fmt.Errorf("config get monitor target %s/%s: %w", urlScope, urlName, err)
		}
	}

	urls := buildLinkURLs(urlScope, urlName, urlSpec, urlConfig, readsOnly)

	consumerKV := map[string]string{consumerEnvVar: urls.WriteURL}

	if urls.ReadURL != "" {
		consumerKV[consumerReadEnvVar] = urls.ReadURL
	}

	if sentinelMode {
		// Sentinel hosts derive from the SENTINEL resource's own
		// (scope, name, replicas) — not the monitor target. Each
		// sentinel pod has the standard <name>-<ord>.<scope>.voodu
		// FQDN, listening on 26379.
		hosts := buildSentinelHosts(provScope, provName, redisReplicas(provSpec))
		consumerKV[consumerSentinelHostsEnvVar] = hosts
		consumerKV[consumerMasterNameEnvVar] = sentinelMasterName
	}

	actions := []dispatchAction{
		{
			Type:  "config_set",
			Scope: consScope,
			Name:  consName,
			KV:    consumerKV,
		},
	}

	// Track the consumer on the provider's bucket so cmdNewPassword
	// can auto-refresh every linked consumer when the password
	// rotates. Idempotent on re-link (consumer already in the
	// list — no-op write of the same value).
	//
	// When the provider is a sentinel resource, we track the
	// consumer on the MONITOR TARGET's bucket — that's the bucket
	// password rotation lives on, and the consumer needs its URL
	// refreshed when the data redis's password changes.
	trackScope, trackName := provScope, provName
	if isSentinelResource {
		trackScope, trackName = monitorTarget.scope, monitorTarget.name
	}

	updatedList := addLinkedConsumer(urlConfig, consScope, consName)

	actions = append(actions, dispatchAction{
		Type:  "config_set",
		Scope: trackScope,
		Name:  trackName,
		KV:    map[string]string{linkedConsumersKey: updatedList},
	})

	out := dispatchOutput{
		Message: linkedMessage(provScope, provName, consScope, consName, urls, readsOnly),
		Actions: actions,
	}

	return writeDispatchOutput(out)
}

// parseLinkFlags is a tiny argv parser for cmdLink and
// cmdUnlink. Recognises a single boolean flag --reads and
// returns the rest as positional args. Order-agnostic: the
// flag may appear before, between, or after the positional
// args. Stop-at-`--` would be standard CLI courtesy but the
// command grammar is too small to need it.
//
// Mirrors Go's flag package for the simplest case; we don't
// import flag because the CLI's pass-through has already
// stripped any `vd`-level flags by the time argv reaches the
// plugin, and a custom 10-line parser is easier to reason
// about than configuring flag.NewFlagSet with a custom Usage.
func parseLinkFlags(args []string) (positional []string, readsOnly, sentinel bool) {
	positional = make([]string, 0, len(args))

	for _, a := range args {
		if a == "--reads" {
			readsOnly = true
			continue
		}

		if a == "--sentinel" {
			sentinel = true
			continue
		}

		positional = append(positional, a)
	}

	return positional, readsOnly, sentinel
}

// monitorTarget is the parsed (scope, name) of a sentinel
// resource's monitor field, surfaced via the env keys baked at
// expand time. When the spec doesn't carry sentinel-mode env
// (= it's a data redis, not a sentinel), isSentinel is false
// and the (scope, name) values are zero — caller treats the
// provider as a regular redis.
type monitorTargetRef struct {
	scope string
	name  string
}

// monitorTargetFromSpec inspects a statefulset spec for the
// VOODU_MONITOR_* env keys the sentinel-mode expand bakes in.
// Detection is by env contract because the original `sentinel`
// HCL block is stripped before manifest emission (see
// stripSentinelBlock) — the env is the durable trace that says
// "this resource is a sentinel quorum, not a data redis".
//
// Pure read of the spec map; no controller calls. Used by
// cmdLink to:
//
//   - reject `--sentinel` when the provider isn't a sentinel
//   - pivot URL building onto the monitored data redis when
//     the operator links a sentinel resource (keeps REDIS_URL
//     pointing at the actual master, not at sentinel:26379)
func monitorTargetFromSpec(spec map[string]any) (monitorTargetRef, bool) {
	env, ok := spec["env"].(map[string]any)
	if !ok {
		return monitorTargetRef{}, false
	}

	name, _ := env["VOODU_MONITOR_NAME"].(string)
	if name == "" {
		return monitorTargetRef{}, false
	}

	scope, _ := env["VOODU_MONITOR_SCOPE"].(string)

	return monitorTargetRef{scope: scope, name: name}, true
}

// buildSentinelHosts emits the comma-separated host:port list
// sentinel-aware clients expect on REDIS_SENTINEL_HOSTS. Each
// sentinel pod gets one entry, indexed 0..replicas-1 against
// the standard voodu0 FQDN scheme.
//
// Pure function (scope, name, replicas → string) — same inputs
// always produce the same output, stable across re-links.
func buildSentinelHosts(scope, name string, replicas int) string {
	if replicas < 1 {
		replicas = 1
	}

	hosts := make([]string, 0, replicas)
	for i := 0; i < replicas; i++ {
		host := fmt.Sprintf("%s-%d", strings.ToLower(strings.TrimSpace(name)), i)
		if scope != "" {
			host += "." + strings.ToLower(strings.TrimSpace(scope))
		}

		host += fmt.Sprintf(".voodu:%d", sentinelPort)
		hosts = append(hosts, host)
	}

	return strings.Join(hosts, ",")
}

// linkURLs is the small bag of URLs cmdLink emits onto the
// consumer's config bucket. WriteURL is always set; ReadURL
// is populated only when the provider has replicas > 1 AND
// the operator did NOT pass --reads.
type linkURLs struct {
	WriteURL string
	ReadURL  string
}

// buildLinkURLs is the single source of truth for the URL-emission
// matrix described on cmdLink. Splitting it out lets cmdNewPassword
// rebuild URLs for every linked consumer with the same logic the
// operator's original `vd redis:link` invocation used, without
// having to re-derive replicas/--reads on the rotation path.
//
// `--reads` is not stored anywhere (it lives on the operator's
// invocation, not the provider's spec), so cmdNewPassword can
// only know about it indirectly: a consumer linked with --reads
// today gets the same `--reads`-shaped URL on re-link tomorrow.
// We re-emit BOTH variants in the rotation path and let the
// consumer's existing config bucket dictate which one applies
// (ReadURL only set if it was set before — see refresh logic
// in cmdNewPassword).
func buildLinkURLs(scope, name string, spec, config map[string]any, readsOnly bool) linkURLs {
	password := redisPasswordFromConfig(config)
	if password == "" {
		password = redisPasswordFromSpecEnv(spec)
	}

	port := redisPort(spec)

	sharedHost := redisHost(scope, name)
	masterHost := redisMasterHost(scope, name, redisMasterOrdinal(config))

	replicas := redisReplicas(spec)

	// Single-pod or operator wants reads-only flat: just the
	// shared alias. Round-robin doesn't matter when there's
	// only one pod, but it produces the same URL the pre-M2
	// plugin emitted, so existing apps see no change.
	if replicas <= 1 || readsOnly {
		return linkURLs{
			WriteURL: composeURL(password, sharedHost, port),
		}
	}

	// Multi-replica + writer consumer: pin writes to the master,
	// fan out reads to the round-robin pool.
	return linkURLs{
		WriteURL: composeURL(password, masterHost, port),
		ReadURL:  composeURL(password, sharedHost, port),
	}
}

// composeURL builds the canonical redis:// URL. Password may be
// empty (no auth — emits redis://host:port verbatim).
func composeURL(password, host string, port int) string {
	u := &url.URL{
		Scheme: "redis",
		Host:   fmt.Sprintf("%s:%d", host, port),
	}

	if password != "" {
		u.User = url.UserPassword("default", password)
	}

	return u.String()
}

// redisMasterHost is the per-pod FQDN voodu0 resolves to a
// specific ordinal (`<name>-<n>.<scope>.voodu`). With M2
// replication, ordinal-0 is the master by convention; ordinal-N
// is replica N. Failover (M2.2) flips this via REDIS_MASTER_ORDINAL
// in the provider's config bucket.
func redisMasterHost(scope, name string, masterOrdinal int) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	name = strings.ToLower(strings.TrimSpace(name))

	pod := fmt.Sprintf("%s-%d", name, masterOrdinal)

	if scope == "" {
		return pod + ".voodu"
	}

	return pod + "." + scope + ".voodu"
}

// redisMasterOrdinal reads REDIS_MASTER_ORDINAL from the
// provider's config bucket. Defaults to 0 (pod-0 = master) on
// missing / unparseable values — matches the wrapper script's
// fallback so URL emission and the entrypoint always agree on
// who the master is.
func redisMasterOrdinal(config map[string]any) int {
	if config == nil {
		return 0
	}

	raw, ok := config["REDIS_MASTER_ORDINAL"].(string)
	if !ok || raw == "" {
		return 0
	}

	var n int

	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 0 {
		return 0
	}

	return n
}

// redisReplicas extracts the declared replica count from the
// statefulset spec. Plugin-side default is 1; the controller
// also clamps to 1 in statefulsetReplicas, so the value here
// matches the actual pod count.
func redisReplicas(spec map[string]any) int {
	if spec == nil {
		return 1
	}

	switch r := spec["replicas"].(type) {
	case float64:
		if r < 1 {
			return 1
		}

		return int(r)
	case int:
		if r < 1 {
			return 1
		}

		return r
	}

	return 1
}

// linkedMessage builds the operator-facing one-liner cmdLink
// emits. Branching on URL shape so the operator immediately
// sees whether they're in dual-URL mode or single-URL mode —
// useful for catching a wrong --reads invocation.
func linkedMessage(provScope, provName, consScope, consName string, urls linkURLs, readsOnly bool) string {
	if urls.ReadURL == "" {
		mode := "single-pod"
		if readsOnly {
			mode = "reads-only"
		}

		return fmt.Sprintf("linked %s → %s (%s, %s)",
			refOrName(provScope, provName),
			refOrName(consScope, consName),
			consumerEnvVar,
			mode)
	}

	return fmt.Sprintf("linked %s → %s (%s + %s, master at ordinal-0)",
		refOrName(provScope, provName),
		refOrName(consScope, consName),
		consumerEnvVar,
		consumerReadEnvVar)
}

// addLinkedConsumer appends a consumer ref to the provider's
// REDIS_LINKED_CONSUMERS list, deduped. Returns the new
// comma-joined value ready for a config_set action. Empty
// initial state produces a single-entry list.
func addLinkedConsumer(config map[string]any, scope, name string) string {
	ref := refOrName(scope, name)

	existing := parseLinkedConsumers(config)

	for _, e := range existing {
		if e == ref {
			// Already linked — re-emit the same list verbatim.
			return strings.Join(existing, ",")
		}
	}

	existing = append(existing, ref)

	return strings.Join(existing, ",")
}

// removeLinkedConsumer drops a consumer ref from the provider's
// REDIS_LINKED_CONSUMERS list, returning the new comma-joined
// value. Idempotent — removing a ref that isn't there is a
// no-op (returns the original list).
func removeLinkedConsumer(config map[string]any, scope, name string) string {
	ref := refOrName(scope, name)

	existing := parseLinkedConsumers(config)

	out := existing[:0]

	for _, e := range existing {
		if e == ref {
			continue
		}

		out = append(out, e)
	}

	return strings.Join(out, ",")
}

// parseLinkedConsumers decodes the comma-separated list. Empty
// values from a missing key, empty string, or stray ","
// collapse to nil so the slice is range-safe without a length
// check at every call site.
func parseLinkedConsumers(config map[string]any) []string {
	if config == nil {
		return nil
	}

	raw, ok := config[linkedConsumersKey].(string)
	if !ok || raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")

	out := make([]string, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		out = append(out, p)
	}

	return out
}

// cmdUnlink emits a config_unset action on the consumer plus
// (when the controller URL is reachable) a config_set on the
// provider to drop this consumer from REDIS_LINKED_CONSUMERS.
// The provider-side update is best-effort — if the controller
// is unreachable, the consumer is still cleared.
//
// Args (os.Args[2:]):
//
//	[0] provider scope/name (used for linked-consumers tracking)
//	[1] consumer scope/name to clear
//
// Both URLs (REDIS_URL and REDIS_READ_URL) are unset on the
// consumer, even if it was a single-URL link — config_unset
// is a no-op on missing keys, so it's safe to clear both
// every time.
func cmdUnlink() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(unlinkHelp)
		return nil
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: vd redis:unlink <provider-scope/name> <consumer-scope/name>")
	}

	provScope, provName := splitScopeName(args[0])
	consScope, consName := splitScopeName(args[1])

	if provName == "" {
		return fmt.Errorf("invalid provider ref %q (expected scope/name)", args[0])
	}

	if consName == "" {
		return fmt.Errorf("invalid consumer ref %q (expected scope/name)", args[1])
	}

	actions := []dispatchAction{
		{
			Type:  "config_unset",
			Scope: consScope,
			Name:  consName,
			Keys:  []string{consumerEnvVar, consumerReadEnvVar},
		},
	}

	// Best-effort: fetch provider config to update the linked-
	// consumers list. A controller fetch failure here drops the
	// list update — the consumer side still gets cleared, which
	// is the operator-visible side of unlink.
	ctx, err := readInvocationContext()
	if err == nil {
		client := newControllerClient(ctx.ControllerURL)

		if config, ferr := client.fetchConfig(provScope, provName); ferr == nil {
			updated := removeLinkedConsumer(config, consScope, consName)

			if updated == "" {
				// Empty list — drop the key entirely so a
				// `vd config get redis_provider` doesn't
				// surface a confusing empty value.
				actions = append(actions, dispatchAction{
					Type:  "config_unset",
					Scope: provScope,
					Name:  provName,
					Keys:  []string{linkedConsumersKey},
				})
			} else {
				actions = append(actions, dispatchAction{
					Type:  "config_set",
					Scope: provScope,
					Name:  provName,
					KV:    map[string]string{linkedConsumersKey: updated},
				})
			}
		}
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("unlinked %s (%s, %s removed)",
			refOrName(consScope, consName),
			consumerEnvVar,
			consumerReadEnvVar),
		Actions: actions,
	}

	return writeDispatchOutput(out)
}

// cmdNewPassword rotates the redis password and AUTOMATICALLY
// refreshes every linked consumer's URL with the new value.
//
// Args (os.Args[2:]):
//
//	[0] target scope/name (the redis to rotate)
//
// Sequence:
//
//  1. Generate a fresh random password.
//  2. Fetch the provider's config bucket to read
//     REDIS_LINKED_CONSUMERS (the list maintained by cmdLink/
//     cmdUnlink) and the current spec (port, replicas — drives
//     the URL shape).
//  3. Emit a config_set on the provider with the new password.
//  4. For each linked consumer, emit a config_set with refreshed
//     URLs. Each consumer's existing config bucket is fetched to
//     decide whether to re-emit REDIS_READ_URL: if it had one
//     before, keep it (replicas > 1, default link); if it didn't,
//     stay single-URL (--reads or replicas <= 1 originally).
//
// The operator still needs `vd apply` to propagate the new
// password into redis.conf — config_set on REDIS_PASSWORD is
// the data side; the asset re-render that bakes it into the
// running redis.conf only happens on apply. The auto-refresh
// here means consumers don't have to be manually re-linked.
func cmdNewPassword() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(newPasswordHelp)
		return nil
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: vd redis:new-password <scope/name>")
	}

	scope, name := splitScopeName(args[0])
	if name == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", args[0])
	}

	fresh, err := generatePassword()
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	actions := []dispatchAction{
		{
			Type:  "config_set",
			Scope: scope,
			Name:  name,
			KV:    map[string]string{passwordKey: fresh},
		},
	}

	// Best-effort consumer refresh. If the controller is
	// unreachable, fall back to the pre-M2 manual flow:
	// operator runs `vd redis:link` per consumer themselves.
	refreshed := 0

	ctx, err := readInvocationContext()
	if err == nil && ctx.ControllerURL != "" {
		client := newControllerClient(ctx.ControllerURL)

		spec, _ := client.fetchSpec("statefulset", scope, name)

		// Build a synthetic config map carrying the NEW password
		// (we haven't applied yet, so the controller's bucket
		// still has the old one). The URL builder reads
		// REDIS_PASSWORD from the config map argument; this
		// shortcut keeps the URL emission logic identical between
		// link-time and rotate-time without a special-case branch.
		providerConfig, _ := client.fetchConfig(scope, name)

		freshConfig := make(map[string]any, len(providerConfig)+1)

		for k, v := range providerConfig {
			freshConfig[k] = v
		}

		freshConfig[passwordKey] = fresh

		consumers := parseLinkedConsumers(providerConfig)

		for _, ref := range consumers {
			cScope, cName := splitScopeName(ref)
			if cName == "" {
				continue
			}

			// Determine whether THIS consumer was originally
			// linked with --reads (single URL) by inspecting
			// the existing config bucket on the consumer side.
			// Presence of REDIS_READ_URL → dual-URL link;
			// absence → single-URL (--reads or replicas <= 1).
			consumerCfg, _ := client.fetchConfig(cScope, cName)

			_, hadRead := consumerCfg[consumerReadEnvVar]

			urls := buildLinkURLs(scope, name, spec, freshConfig, !hadRead)

			kv := map[string]string{consumerEnvVar: urls.WriteURL}

			if urls.ReadURL != "" {
				kv[consumerReadEnvVar] = urls.ReadURL
			}

			actions = append(actions, dispatchAction{
				Type:  "config_set",
				Scope: cScope,
				Name:  cName,
				KV:    kv,
			})

			refreshed++
		}
	}

	msg := fmt.Sprintf("rotated REDIS_PASSWORD for %s — run `vd apply` to refresh redis.conf",
		refOrName(scope, name))

	if refreshed > 0 {
		msg = fmt.Sprintf("%s; auto-refreshed %d linked consumer(s) with the new URL",
			msg, refreshed)
	}

	out := dispatchOutput{
		Message: msg,
		Actions: actions,
	}

	return writeDispatchOutput(out)
}

// Help text for each command — emitted when -h/--help is in
// os.Args. The plugin owns its own help; the CLI is a dumb
// pass-through that doesn't intercept these flags.
const (
	linkHelp = `Usage: vd redis:link [--reads] [--sentinel] <provider-scope/name> <consumer-scope/name>

Inject the redis provider's connection URL into the consumer's
config bucket. The consumer auto-restarts to pick up the new env.

The provider can be either:
  - a data redis (regular replication setup), OR
  - a sentinel resource (declared with `+"`sentinel { enabled = true }`"+`),
    in which case the URL emission pivots on the MONITORED data
    redis but the consumer additionally gets sentinel discovery
    info when --sentinel is passed.

URLs emitted (data redis provider OR sentinel-pivoted target):

  replicas = 1
    REDIS_URL = redis://default:<pw>@<name>.<scope>.voodu:6379

  replicas > 1, no --reads
    REDIS_URL      = redis://default:<pw>@<name>-0.<scope>.voodu:6379  (master)
    REDIS_READ_URL = redis://default:<pw>@<name>.<scope>.voodu:6379    (round-robin)

  replicas > 1, --reads
    REDIS_URL      = redis://default:<pw>@<name>.<scope>.voodu:6379    (round-robin only)

Additional env when --sentinel is passed (provider must be a sentinel):

    REDIS_SENTINEL_HOSTS = sentinel-0.<scope>.voodu:26379,...,sentinel-N.<scope>.voodu:26379
    REDIS_MASTER_NAME    = mymaster

Use --sentinel for apps with sentinel-aware clients (ioredis with
'Sentinel(...)', redis-py 'Sentinel(...)', lettuce, redis-rb
with 'sentinels: [...]'). They discover the current master at
runtime, surviving failover events without env-driven restart.

Apps without a sentinel-aware client just use REDIS_URL and rely
on voodu's env-change rolling restart to pick up failover events
(post-failover, the controller updates REDIS_MASTER_ORDINAL on
the data redis bucket, which re-emits all linked consumer URLs).

The provider tracks linked consumers in REDIS_LINKED_CONSUMERS
so 'vd redis:new-password' auto-refreshes every consumer URL.
For sentinel resources, tracking lives on the monitored data
redis (where the password lives).

Examples:
  vd redis:link clowk-lp/redis clowk-lp/web
  vd redis:link --reads clowk-lp/redis clowk-lp/dashboard
  vd redis:link clowk-lp/redis-quorum clowk-lp/web                # via sentinel, REDIS_URL only
  vd redis:link --sentinel clowk-lp/redis-quorum clowk-lp/web     # via sentinel, full discovery
  vd redis:link --reads --sentinel clowk-lp/redis-quorum clowk-lp/dash`

	unlinkHelp = `Usage: vd redis:unlink <provider-scope/name> <consumer-scope/name>

Remove REDIS_URL and REDIS_READ_URL from the consumer's config
bucket and drop the consumer from the provider's linked-consumers
list. Consumer auto-restarts.

Example:
  vd redis:unlink clowk-lp/redis clowk-lp/web`

	newPasswordHelp = `Usage: vd redis:new-password <scope/name>

Rotate the redis password. Generates a fresh 256-bit random
password and stores it in the redis's config bucket, then
auto-refreshes every linked consumer's URL with the new password.
Operator runs 'vd apply' next to propagate the password into
redis.conf (asset re-materialise → rolling restart). Consumers
pick up the new URL on their auto-restart.

Example:
  vd redis:new-password clowk-lp/redis`
)

// hasHelpFlag reports whether -h or --help is anywhere in the
// args slice. CLI passes flags through verbatim, so the plugin
// is responsible for detecting and rendering its own help.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}

	return false
}

// buildRedisURL constructs the redis://… URL clients dial.
// Password resolution priority (matches the previous design):
//
//  1. config["REDIS_PASSWORD"] (operator's `vd config set`)
//  2. spec.env["REDIS_PASSWORD"] (operator's HCL env block)
//  3. None — emit no-auth URL
//
// Always returns the round-robin shared-alias URL (the
// pre-M2 default). Used by `vd redis:info` for the displayed
// connection URL — operator-facing one-liner that doesn't
// need to break out master vs read pools. cmdLink calls
// buildLinkURLs directly to get the dual-URL shape.
func buildRedisURL(scope, name string, spec, config map[string]any) (string, error) {
	password := redisPasswordFromConfig(config)
	if password == "" {
		password = redisPasswordFromSpecEnv(spec)
	}

	host := redisHost(scope, name)
	port := redisPort(spec)

	return composeURL(password, host, port), nil
}

// redisHost is the voodu0 FQDN: `<name>.<scope>.voodu` (or
// `<name>.voodu` when unscoped).
func redisHost(scope, name string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	name = strings.ToLower(strings.TrimSpace(name))

	if scope == "" {
		return name + ".voodu"
	}

	return name + "." + scope + ".voodu"
}

// redisPort extracts the connection port from the manifest spec.
// `spec.ports[0]` wins; falls back to 6379 (redis default).
// Strips any "host:" prefix so loopback-rewritten ports
// (`127.0.0.1:6379`) still produce 6379.
func redisPort(spec map[string]any) int {
	const defaultPort = 6379

	if spec == nil {
		return defaultPort
	}

	rawPorts, ok := spec["ports"].([]any)
	if !ok || len(rawPorts) == 0 {
		return defaultPort
	}

	first, ok := rawPorts[0].(string)
	if !ok || first == "" {
		return defaultPort
	}

	if i := strings.LastIndex(first, ":"); i >= 0 {
		first = first[i+1:]
	}

	var port int

	if _, err := fmt.Sscanf(first, "%d", &port); err != nil || port <= 0 {
		return defaultPort
	}

	return port
}

func redisPasswordFromConfig(config map[string]any) string {
	if config == nil {
		return ""
	}

	if v, ok := config["REDIS_PASSWORD"].(string); ok {
		return v
	}

	return ""
}

func redisPasswordFromSpecEnv(spec map[string]any) string {
	if spec == nil {
		return ""
	}

	env, ok := spec["env"].(map[string]any)
	if !ok {
		return ""
	}

	if v, ok := env["REDIS_PASSWORD"].(string); ok {
		return v
	}

	return ""
}

// readInvocationContext decodes the JSON envelope the controller
// wrote to stdin. Empty stdin is OK — falls back to env vars
// for direct CLI invocation (smoke testing).
func readInvocationContext() (*invocationContext, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}

	ctx := &invocationContext{}

	if len(raw) > 0 {
		if err := json.Unmarshal(raw, ctx); err != nil {
			return nil, fmt.Errorf("decode stdin: %w", err)
		}
	}

	// Fallbacks for direct invocation outside the controller —
	// useful for `voodu-redis link a b` smoke testing without
	// the dispatch endpoint.
	if ctx.ControllerURL == "" {
		ctx.ControllerURL = os.Getenv("VOODU_CONTROLLER_URL")
	}

	if ctx.PluginDir == "" {
		ctx.PluginDir = os.Getenv("VOODU_PLUGIN_DIR")
	}

	return ctx, nil
}

// writeDispatchOutput encodes the dispatch result inside the
// standard plugin envelope.
func writeDispatchOutput(out dispatchOutput) error {
	enc := json.NewEncoder(os.Stdout)

	return enc.Encode(envelope{Status: "ok", Data: out})
}

// splitScopeName parses "scope/name" or just "name". Empty
// scope when no slash. Mirrors splitJobRef in the CLI; kept
// independent so the plugin doesn't import voodu internals.
func splitScopeName(ref string) (scope, name string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}

	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i], ref[i+1:]
	}

	return "", ref
}

func refOrName(scope, name string) string {
	if scope == "" {
		return name
	}

	return scope + "/" + name
}

// controllerClient is the tiny HTTP client the plugin uses to
// call back into the controller. Just two endpoints today:
// /describe (manifest spec) and /config (env bucket).
type controllerClient struct {
	baseURL string
	http    *http.Client
}

func newControllerClient(baseURL string) *controllerClient {
	return &controllerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// fetchSpec calls GET /describe?kind=&scope=&name= and returns
// the manifest's spec field as a generic map.
func (c *controllerClient) fetchSpec(kind, scope, name string) (map[string]any, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("no controller_url available (set VOODU_CONTROLLER_URL or run via dispatch endpoint)")
	}

	u := fmt.Sprintf("%s/describe?kind=%s&scope=%s&name=%s",
		c.baseURL,
		url.QueryEscape(kind),
		url.QueryEscape(scope),
		url.QueryEscape(name),
	)

	resp, err := c.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("describe %s/%s/%s: HTTP %d: %s", kind, scope, name, resp.StatusCode, body)
	}

	var env struct {
		Data struct {
			Manifest struct {
				Spec map[string]any `json:"spec"`
			} `json:"manifest"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode describe response: %w", err)
	}

	return env.Data.Manifest.Spec, nil
}

// fetchConfig calls GET /config?scope=&name= and returns the
// merged config bucket as a string-typed map (REDIS_PASSWORD
// is a string, etc.) wrapped as map[string]any so the URL
// builder can use the same shape it expects from the spec.
//
// Wire shape on the controller side:
//
//	{"status":"ok","data":{"vars":{"KEY":"value", ...}}}
//
// We unwrap data.vars into a flat map. An empty bucket
// surfaces as an empty map, not nil — caller can range it
// safely.
func (c *controllerClient) fetchConfig(scope, name string) (map[string]any, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("no controller_url available")
	}

	u := fmt.Sprintf("%s/config?scope=%s&name=%s",
		c.baseURL,
		url.QueryEscape(scope),
		url.QueryEscape(name),
	)

	resp, err := c.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("config get %s/%s: HTTP %d: %s", scope, name, resp.StatusCode, body)
	}

	var env struct {
		Data struct {
			Vars map[string]string `json:"vars"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode config response: %w", err)
	}

	out := make(map[string]any, len(env.Data.Vars))
	for k, v := range env.Data.Vars {
		out[k] = v
	}

	return out, nil
}
