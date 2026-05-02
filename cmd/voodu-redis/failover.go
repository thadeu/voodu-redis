// Manual failover (M2.2) — flips REDIS_MASTER_ORDINAL on the
// provider's config bucket and refreshes every linked consumer's
// URL so writes route to the new master ordinal.
//
// The wrapper script reads REDIS_MASTER_ORDINAL at boot, so the
// flip alone doesn't change runtime behaviour: the operator must
// run `vd apply` next to rolling-restart the statefulset, which
// makes every pod re-evaluate its role. The new master pod boots
// without --replicaof; the old master comes back as a replica
// pointed at the new master and re-syncs via PSYNC (which
// discards any writes the old master held but didn't replicate
// — async replication's documented data-loss window).
//
// Why a dedicated command and not just `vd config set`:
//
//   - Validation: target ordinal must be in [0, replicas-1] and
//     not equal to the current master. Hand-editing the config
//     bucket loses these guards.
//   - URL refresh: every linked consumer's REDIS_URL needs to
//     re-emit against the new master FQDN. cmdFailover walks the
//     REDIS_LINKED_CONSUMERS list and re-runs the link-time URL
//     builder so the apply isn't followed by N manual re-links.
//   - Operator messaging: the post-action one-liner spells out
//     the next step (`vd apply`) and the split-brain caveat
//     (operator should drain writes first if data integrity
//     matters more than availability).
//
// Automatic graceful failover with quorum-based promotion is
// the Sentinel feature in F3 — out of scope for F2.2.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// failoverMasterKey mirrors the wrapper script's REDIS_MASTER_ORDINAL
// read. Centralised here so the failover command and URL builder
// (which calls redisMasterOrdinal directly) can't drift on the
// key name without one of them surfacing the typo.
const failoverMasterKey = "REDIS_MASTER_ORDINAL"

// cmdFailover promotes a specific ordinal to master.
//
// Args (os.Args[2:], flags interleaved):
//
//	[positional 0] target scope/name (the redis to flip)
//	--to <ordinal> the ordinal to promote. Required.
//
// Sequence:
//
//  1. Fetch spec to learn replicas count.
//  2. Fetch config to read the current REDIS_MASTER_ORDINAL.
//  3. Validate target in range and not already master.
//  4. Emit config_set REDIS_MASTER_ORDINAL=<target> on provider.
//  5. For each linked consumer (REDIS_LINKED_CONSUMERS list),
//     re-emit URLs against the new master via buildLinkURLs.
//  6. Operator-facing message: "run vd apply to roll the
//     statefulset; consumers picked up the new URL".
//
// `-h` / `--help` short-circuits the network calls and prints
// usage on stdout for the operator.
func cmdFailover() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(failoverHelp)
		return nil
	}

	positional, target, hasTarget, noRestart := parseFailoverFlags(args)

	if len(positional) < 1 {
		return fmt.Errorf("usage: vd redis:failover <scope/name> --to <ordinal> [--no-restart]")
	}

	if !hasTarget {
		return fmt.Errorf("--to <ordinal> is required (the ordinal to promote to master)")
	}

	scope, name := splitScopeName(positional[0])
	if name == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", positional[0])
	}

	ctx, err := readInvocationContext()
	if err != nil {
		return err
	}

	if ctx.ControllerURL == "" {
		return fmt.Errorf("failover requires controller_url (no offline mode — the command needs replicas count + linked-consumers list from the controller)")
	}

	client := newControllerClient(ctx.ControllerURL)

	spec, err := client.fetchSpec("statefulset", scope, name)
	if err != nil {
		return fmt.Errorf("describe %s: %w", refOrName(scope, name), err)
	}

	config, err := client.fetchConfig(scope, name)
	if err != nil {
		return fmt.Errorf("config get %s: %w", refOrName(scope, name), err)
	}

	replicas := redisReplicas(spec)

	if replicas <= 1 {
		return fmt.Errorf("redis %s has replicas=%d; failover requires replicas > 1 (raise the count and re-apply first)",
			refOrName(scope, name), replicas)
	}

	if target < 0 || target >= replicas {
		return fmt.Errorf("--to %d out of range (valid: 0..%d)", target, replicas-1)
	}

	current := redisMasterOrdinal(config)

	if target == current {
		return fmt.Errorf("redis %s already has master at ordinal %d — no-op",
			refOrName(scope, name), current)
	}

	// Synthetic config carrying the post-failover ordinal so the
	// URL builder pins the new master host. Same trick as
	// cmdNewPassword's freshConfig — we haven't written the new
	// value to the controller yet (it lands as one of THIS
	// command's config_set actions), but the linked-consumer
	// URLs in the same envelope must reflect the post-failover
	// state, not the pre-failover one.
	newConfig := make(map[string]any, len(config)+1)

	for k, v := range config {
		newConfig[k] = v
	}

	newConfig[failoverMasterKey] = strconv.Itoa(target)

	// The redis-itself action carries SkipRestart when --no-restart
	// was passed. This is the sentinel auto-failover path: roles
	// have already moved inside Redis (SLAVEOF NO ONE on the new
	// master), so we just want to record the new ordinal in the
	// store WITHOUT triggering a rolling restart that would (a)
	// drop active connections on the freshly promoted master and
	// (b) risk a ping-pong with sentinel re-electing during the
	// reboot window. Consumer URL refreshes (below) keep the
	// default SkipRestart=false because consumers still need to
	// pick up the new URL.
	actions := []dispatchAction{
		{
			Type:        "config_set",
			Scope:       scope,
			Name:        name,
			KV:          map[string]string{failoverMasterKey: strconv.Itoa(target)},
			SkipRestart: noRestart,
		},
	}

	// Refresh every linked consumer's URLs — same shape decision
	// the original cmdLink made (single REDIS_URL vs dual
	// REDIS_URL+REDIS_READ_URL). Presence of REDIS_READ_URL on
	// the consumer's bucket signals the dual-URL link mode;
	// absence signals --reads or single-pod (re-emit single).
	refreshed := 0

	consumers := parseLinkedConsumers(config)

	for _, ref := range consumers {
		cScope, cName := splitScopeName(ref)
		if cName == "" {
			continue
		}

		consumerCfg, _ := client.fetchConfig(cScope, cName)

		_, hadRead := consumerCfg[consumerReadEnvVar]

		urls := buildLinkURLs(scope, name, spec, newConfig, !hadRead)

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

	// The controller's config_set fan-out re-fires the statefulset's
	// apply, and the apply path's env-change branch rolls every pod
	// top-down. So the operator-visible flow is one-shot: failover
	// → URLs refreshed → pods restarting → new master live. No
	// trailing `vd apply` needed.
	//
	// Under --no-restart, the redis pods are NOT rolled (sentinel
	// has already done the in-memory role flip). The store still
	// records the new ordinal for crash-recovery boots; consumer
	// URLs still refresh.
	msg := fmt.Sprintf(
		"redis %s: master ordinal %d → %d. Pods are rolling top-down; the new master picks up reads/writes once ordinal-%d finishes restarting.",
		refOrName(scope, name), current, target, target,
	)

	if noRestart {
		msg = fmt.Sprintf(
			"redis %s: master ordinal %d → %d (--no-restart: store updated, redis pods NOT rolled — sentinel-driven role change assumed).",
			refOrName(scope, name), current, target,
		)
	}

	if refreshed > 0 {
		msg = fmt.Sprintf("%s Refreshed %d linked consumer URL(s).", msg, refreshed)
	}

	return writeDispatchOutput(dispatchOutput{
		Message: msg,
		Actions: actions,
	})
}

// parseFailoverFlags extracts `--to <ordinal>` (space- or
// =-separated) and `--no-restart` (boolean), returning the rest
// as positional args. Order-agnostic — flags may precede or
// follow the positional ref.
//
// Returns hasTarget=false when --to is absent so the caller can
// distinguish "didn't pass --to" from "passed --to 0" (a valid
// target — pod-0 is the default master, so --to 0 is the
// recovery flow after a previous failover-to-1).
//
// noRestart=true means "skip the rolling restart of the redis
// pods after recording the new ordinal". Used by the sentinel
// auto-failover hook (sentinel has already moved roles inside
// Redis; rolling pods would drop connections needlessly).
func parseFailoverFlags(args []string) (positional []string, target int, hasTarget bool, noRestart bool) {
	positional = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Long-form `--to N`
		if a == "--to" {
			if i+1 < len(args) {
				i++

				if n, err := strconv.Atoi(args[i]); err == nil {
					target = n
					hasTarget = true
				}
			}

			continue
		}

		// Equals-form `--to=N`
		if strings.HasPrefix(a, "--to=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--to=")); err == nil {
				target = n
				hasTarget = true
			}

			continue
		}

		if a == "--no-restart" {
			noRestart = true
			continue
		}

		positional = append(positional, a)
	}

	return positional, target, hasTarget, noRestart
}

const failoverHelp = `Usage: vd redis:failover <scope/name> --to <ordinal> [--no-restart]

Promote a specific ordinal to master. Flips REDIS_MASTER_ORDINAL
on the provider's config bucket, refreshes every linked consumer's
URL, and rolls the statefulset top-down so the new master takes
over. One-shot — no trailing ` + "`" + `vd apply` + "`" + ` needed.

What happens:

  1. REDIS_MASTER_ORDINAL on the provider's bucket flips to <ordinal>.
  2. Every linked consumer's REDIS_URL re-emits against the new
     master's per-pod FQDN. Apps auto-restart on env change.
  3. The controller's config-change fan-out re-fires the statefulset's
     apply. The apply's env-change branch rolls every pod top-down.
     The wrapper script reads REDIS_MASTER_ORDINAL at boot and picks
     the new role.
  4. The old master comes back as a replica and re-syncs from the
     new master. Writes the old master held but never replicated
     are LOST — async replication's documented data-loss window.

Operators wanting zero-loss failover should drain writes first
(app maintenance mode), run the failover, then resume traffic.
Quorum-based graceful failover is the Sentinel feature in F3.

--no-restart: skip the rolling restart of the redis statefulset.
The store still updates with the new master ordinal (so consumers'
URLs refresh and crash-recovery boots pick the right role), but
the redis pods themselves are NOT rolled.

Used by the sentinel auto-failover hook: sentinel has already
flipped roles inside Redis (SLAVEOF NO ONE on the new master);
rolling the pods would drop active connections AND risk a
ping-pong with sentinel re-electing during the reboot window.

Operators can also use --no-restart when they've manually moved
roles via redis-cli (incident recovery) and just want voodu to
catch up.

Examples:
  vd redis:failover clowk-lp/redis --to 1
  vd redis:failover clowk-lp/redis --to=0                # recover after a failover-to-1
  vd redis:failover clowk-lp/redis --to 1 --no-restart   # sentinel-driven, store-only sync`
