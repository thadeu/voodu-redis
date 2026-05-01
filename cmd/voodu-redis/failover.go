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

	positional, target, hasTarget := parseFailoverFlags(args)

	if len(positional) < 1 {
		return fmt.Errorf("usage: vd redis:failover <scope/name> --to <ordinal>")
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

	actions := []dispatchAction{
		{
			Type:  "config_set",
			Scope: scope,
			Name:  name,
			KV:    map[string]string{failoverMasterKey: strconv.Itoa(target)},
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

	msg := fmt.Sprintf(
		"redis %s: master ordinal %d → %d. Run `vd apply` next to roll the statefulset; the wrapper script reads REDIS_MASTER_ORDINAL at boot, so pods need to restart.",
		refOrName(scope, name), current, target,
	)

	if refreshed > 0 {
		msg = fmt.Sprintf("%s Refreshed %d linked consumer URL(s).", msg, refreshed)
	}

	return writeDispatchOutput(dispatchOutput{
		Message: msg,
		Actions: actions,
	})
}

// parseFailoverFlags extracts `--to <ordinal>` (space- or
// =-separated) and returns the rest as positional args. Order-
// agnostic — flag may precede or follow the positional ref.
//
// Returns hasTarget=false when --to is absent so the caller can
// distinguish "didn't pass --to" from "passed --to 0" (a valid
// target — pod-0 is the default master, so --to 0 is the
// recovery flow after a previous failover-to-1).
func parseFailoverFlags(args []string) (positional []string, target int, hasTarget bool) {
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

		positional = append(positional, a)
	}

	return positional, target, hasTarget
}

const failoverHelp = `Usage: vd redis:failover <scope/name> --to <ordinal>

Promote a specific ordinal to master. Flips REDIS_MASTER_ORDINAL
on the provider's config bucket and refreshes every linked
consumer's URL so writes route to the new master.

Sequence after the command runs:

  1. Run ` + "`" + `vd apply` + "`" + ` to roll the statefulset. The wrapper
     script reads REDIS_MASTER_ORDINAL at boot, so pods need
     to restart for the role flip to take effect.
  2. The old master comes back as a replica and re-syncs from
     the new master. Writes that the old master held but never
     replicated are LOST — async replication's documented
     data-loss window.

Operators wanting zero-loss failover should drain writes first
(app maintenance mode), then run failover + apply, then resume
traffic. Quorum-based graceful failover is the Sentinel feature
in F3.

Examples:
  vd redis:failover clowk-lp/redis --to 1
  vd redis:failover clowk-lp/redis --to=0   # recover after a failover-to-1`
