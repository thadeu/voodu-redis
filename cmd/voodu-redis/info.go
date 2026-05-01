package main

import (
	"fmt"
	"os"
	"strings"
)

// cmdInfo prints connection + storage info for a redis instance.
// It's the plugin's answer to `vd describe pod` — operator-
// readable details specific to redis (connection URL, password
// storage, data volume) rather than the generic statefulset
// fields the core describe shows.
//
// Args (os.Args[2:]):
//
//	[0] target scope/name (e.g. "clowk-lp/redis")
//
// Reads invocation context from stdin (controller_url) and
// calls /describe + /config to gather state.
func cmdInfo() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(infoHelp)
		return nil
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: vd redis:info <scope/name>")
	}

	scope, name := splitScopeName(args[0])
	if name == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", args[0])
	}

	ctx, err := readInvocationContext()
	if err != nil {
		return err
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

	host := redisHost(scope, name)
	port := redisPort(spec)

	// Same URL builder cmdLink uses for non-`--reads` consumers.
	// Single-pod → one URL on the shared alias. Multi-pod → write
	// URL pinned to the master ordinal, read URL on the round-
	// robin pool. Mirrors what consumers actually see in their
	// REDIS_URL / REDIS_READ_URL env vars so operator's view of
	// "what to point a client at" matches the linked apps' view.
	urls := buildLinkURLs(scope, name, spec, config, false)

	// Data volume the plugin's defaults set up. Operator can
	// override volume_claims; we read the resolved spec to show
	// what's actually configured.
	dataVolume := "/data (default voodu-redis volume_claim)"

	if claims, ok := spec["volume_claims"].([]any); ok && len(claims) > 0 {
		if firstClaim, ok := claims[0].(map[string]any); ok {
			mp, _ := firstClaim["mount_path"].(string)
			cn, _ := firstClaim["name"].(string)

			if mp != "" {
				dataVolume = fmt.Sprintf("%s (claim: %s)", mp, cn)
			}
		}
	}

	image, _ := spec["image"].(string)
	if image == "" {
		image = "(default — see voodu-redis composeDefaults)"
	}

	replicas := redisReplicas(spec)
	masterOrd := redisMasterOrdinal(config)

	out := strings.Builder{}

	fmt.Fprintf(&out, "redis/%s\n\n", refOrName(scope, name))
	fmt.Fprintf(&out, "  plugin:          voodu-redis v%s\n", version)
	fmt.Fprintf(&out, "  image:           %s\n", image)
	fmt.Fprintf(&out, "  host:            %s\n", host)
	fmt.Fprintf(&out, "  port:            %d\n", port)
	fmt.Fprintf(&out, "  data volume:     %s\n", dataVolume)
	fmt.Fprintln(&out)

	// Topology section — replicas, master role, per-pod FQDNs
	// so the operator can run `redis-cli -h <pod>.<scope>.voodu`
	// against a specific replica without guessing the alias
	// shape.
	fmt.Fprintf(&out, "topology:\n")
	fmt.Fprintf(&out, "  replicas:        %d\n", replicas)

	if replicas == 1 {
		fmt.Fprintf(&out, "  role:            single-pod (no replication)\n")
	} else {
		fmt.Fprintf(&out, "  master ordinal:  %d (REDIS_MASTER_ORDINAL)\n", masterOrd)
		fmt.Fprintf(&out, "  master host:     %s\n", redisMasterHost(scope, name, masterOrd))

		fmt.Fprintf(&out, "  replica hosts:   ")

		first := true
		for n := 0; n < replicas; n++ {
			if n == masterOrd {
				continue
			}

			if !first {
				fmt.Fprintf(&out, ", ")
			}

			fmt.Fprintf(&out, "%s", redisMasterHost(scope, name, n))
			first = false
		}

		fmt.Fprintln(&out)
	}

	fmt.Fprintln(&out)
	fmt.Fprintf(&out, "connect:\n")
	// Connection URLs are shown verbatim (with the password) so
	// operator gets copy/paste-ready strings for redis-cli or
	// app config. Trade-off: visible on screen-shares. Operators
	// wanting redaction can run `vd redis:info | sed
	// 's/:[^@]*@/:****@/'` on their side; the plugin opts for
	// utility over caution by default.
	//
	// URL emission matches cmdLink's matrix:
	//
	//   - replicas == 1: just `url`, on the round-robin shared
	//     alias (effectively a single pod).
	//   - replicas  > 1: `write url` pins the master pod (writes
	//     route directly) and `read url` fans out across every
	//     replica via the round-robin alias. Apps using the
	//     dual-URL pattern read from the pool, write to master.
	if urls.ReadURL == "" {
		fmt.Fprintf(&out, "  url:             %s\n", urls.WriteURL)
	} else {
		fmt.Fprintf(&out, "  write url:       %s\n", urls.WriteURL)
		fmt.Fprintf(&out, "  read url:        %s\n", urls.ReadURL)
	}

	// Linked consumers: surface the list maintained by
	// cmdLink/cmdUnlink so the operator can see "what breaks if
	// I rotate the password" at a glance. Empty list is shown
	// explicitly rather than omitted — better signal than an
	// absent section ("none yet" vs "feature missing").
	consumers := parseLinkedConsumers(config)

	fmt.Fprintln(&out)
	fmt.Fprintf(&out, "linked consumers (REDIS_LINKED_CONSUMERS):\n")

	if len(consumers) == 0 {
		fmt.Fprintf(&out, "  (none — run `vd redis:link %s/%s <consumer>` to add one)\n",
			scope, name)
	} else {
		for _, c := range consumers {
			fmt.Fprintf(&out, "  - %s\n", c)
		}
	}

	// Plain text on stdout — no envelope. Operator wants to
	// read this, not parse JSON. Server passes it through as
	// the `message` field of the dispatch response.
	fmt.Print(out.String())

	return nil
}

const infoHelp = `Usage: vd redis:info <scope/name>

Show topology, connection URLs, and linked consumer list for a
redis instance managed by voodu-redis. URLs are emitted verbatim
(with password) so they're copy/paste-ready for redis-cli or
app config — be aware on screen-shares.

URL display matches what 'vd redis:link' injects on consumers:

  replicas == 1   → single 'url' on the round-robin shared alias
  replicas  > 1   → 'write url' pinned to the master ordinal +
                    'read url' on the round-robin pool

Example:
  vd redis:info clowk-lp/redis`
