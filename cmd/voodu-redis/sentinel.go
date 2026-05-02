// Sentinel mode (F3) — HCL surface for declaring a redis resource
// that runs `redis-sentinel` instead of `redis-server`. Quorum
// against another (data) redis resource in the same scope.
//
// Operator-facing HCL shape:
//
//	redis "clowk-lp" "redis-quorum" {
//	  image    = "redis:8"
//	  replicas = 3
//	  sentinel {
//	    enabled = true
//	    monitor = "clowk-lp/redis"
//	  }
//	}
//
// The `sentinel` block is opt-in. Its presence flips the resource
// from "data redis" (running redis-server with replication) to
// "sentinel quorum" (running redis-sentinel watching a peer redis).
//
// Quorum is auto-derived from `replicas` at runtime — formula
// `(replicas / 2) + 1`. The HCL surface deliberately omits a
// `quorum` field so the operator can't get math wrong; raising
// or lowering quorum is done by changing replicas count.
//
// Cross-resource semantics:
//
//   - `monitor` is a "scope/name" reference to the data redis the
//     sentinels watch. MUST live in the same scope as the sentinel
//     resource (cross-scope is parked for a future milestone — opens
//     auth + network + asset-ref questions out of M-S0 scope).
//   - The target's existence + kind + non-cascade (target doesn't
//     itself declare sentinel mode) is enforced at sentinel boot
//     time in M-S1 by the entrypoint, not at apply time. Reason:
//     the plugin's expand command runs on a single-resource view
//     and doesn't (today) query the controller for peer specs.
//     Surfacing the error at boot is a reasonable trade — the
//     resource still applies but the sentinel pod fails fast with
//     a clear diagnostic.
//
// This file owns parse + apply-time-validate. The sentinel-mode
// expand emission (different defaults, command, ports, volumes
// bind for sentinel.conf) lands in M-S1.
package main

import (
	"errors"
	"fmt"
	"strings"
)

// sentinelSpec is the parsed form of the operator's `sentinel { }`
// block. Pointer return distinguishes "block omitted entirely"
// (nil) from "block present" (non-nil) so we don't accidentally
// flip data-redis resources into sentinel mode based on a
// missing-field default.
type sentinelSpec struct {
	// Enabled is the master switch. Defaults to TRUE when the
	// operator writes a `sentinel { }` block at all — the block's
	// presence IS the signal of intent, no point making the
	// operator write `enabled = true` explicitly.
	//
	// Explicit `enabled = false` is still honoured as a soft
	// toggle-off path: keep the block in HCL (planning, comment-
	// for-self) but disable runtime. Equivalent to commenting
	// the block out, but visible in `vd describe` / git diff.
	Enabled bool

	// Monitor is the "scope/name" reference to the data redis this
	// sentinel watches. Required when Enabled is true. Same scope
	// as the sentinel resource (validated post-parse).
	Monitor string
}

// parseSentinelSpec extracts the `sentinel` block from the
// operator's HCL spec (already JSON-decoded as map[string]any).
// Returns nil when the block is absent — caller treats that as
// "data redis mode, plugin behaviour unchanged". Returns a
// populated *sentinelSpec when the block is present, regardless
// of `enabled` value (so validation can run on disabled blocks
// too — e.g. flag a missing `monitor` even when enabled=false,
// which would surprise the operator on the next toggle).
//
// Errors are limited to shape mismatches (block isn't an object,
// fields have wrong types). Semantic checks (enabled requires
// monitor, scope alignment, replicas count) live in validate.
func parseSentinelSpec(operatorSpec map[string]any) (*sentinelSpec, error) {
	raw, ok := operatorSpec["sentinel"]
	if !ok {
		return nil, nil
	}

	// HCL → JSON renders the block as an object. List-of-objects
	// (HCL repeated block syntax) would be []any with one element;
	// we accept both shapes so the operator can write
	// `sentinel { ... }` (scalar block) without surprises.
	var obj map[string]any

	switch v := raw.(type) {
	case map[string]any:
		obj = v

	case []any:
		// Tolerate the list shape but only when it has exactly one
		// element. More than one `sentinel { }` block on the same
		// resource is a config error — sentinel is singleton.
		if len(v) != 1 {
			return nil, fmt.Errorf("sentinel block declared %d times; exactly one `sentinel { }` block is allowed per resource", len(v))
		}

		sub, isMap := v[0].(map[string]any)
		if !isMap {
			return nil, errors.New("sentinel block is malformed (expected object)")
		}

		obj = sub

	default:
		return nil, fmt.Errorf("sentinel block has unexpected type %T (expected object)", raw)
	}

	// Default Enabled=true: writing the block IS the intent signal.
	// Operator can opt OUT explicitly with `enabled = false` (soft
	// toggle-off path), which keeps the block visible in HCL but
	// drops the resource back to data-redis-mode validation.
	out := &sentinelSpec{Enabled: true}

	if v, present := obj["enabled"]; present {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("sentinel.enabled has unexpected type %T (expected bool)", v)
		}

		out.Enabled = b
	}

	if v, present := obj["monitor"]; present {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("sentinel.monitor has unexpected type %T (expected string in scope/name form)", v)
		}

		out.Monitor = s
	}

	return out, nil
}

// validateSentinelSpec runs the apply-time semantic checks for a
// sentinel-mode resource. Pure function — given the parsed spec,
// the request envelope, and the operator's spec map, returns a
// clear error or nil. No I/O, no controller calls.
//
// Checks (per F3 design):
//
//  1. enabled=true requires `monitor`.
//  2. `monitor` must be in "scope/name" form, with scope matching
//     the request scope (same-scope only in MVP).
//  3. `monitor` cannot point at the resource itself (sentinel
//     watching its own quorum is meaningless).
//  4. replicas count: 1 is permitted (degenerate, not HA), 2 is
//     rejected (quorum math degenerates — quorum=2 with replicas=2
//     means losing one vote breaks failover, worse than no sentinel
//     at all), 3+ is fine.
//
// Cross-resource checks (target exists, is a redis, doesn't
// itself declare sentinel mode) are intentionally NOT here —
// see the package doc comment for why they live at runtime.
func validateSentinelSpec(s *sentinelSpec, req expandRequest, operatorSpec map[string]any) error {
	if s == nil || !s.Enabled {
		return nil
	}

	if strings.TrimSpace(s.Monitor) == "" {
		return errors.New("sentinel.enabled = true requires sentinel.monitor (the \"scope/name\" of the redis to watch)")
	}

	scope, name, err := splitMonitorRef(s.Monitor)
	if err != nil {
		return fmt.Errorf("sentinel.monitor %q: %w", s.Monitor, err)
	}

	if scope != req.Scope {
		return fmt.Errorf(
			"sentinel.monitor %q points at scope %q but this sentinel is in scope %q — cross-scope monitoring is not supported in this milestone (declare both resources in the same scope)",
			s.Monitor, scope, req.Scope,
		)
	}

	if name == req.Name {
		return fmt.Errorf("sentinel.monitor %q points at the sentinel resource itself; pick a different (data) redis to monitor", s.Monitor)
	}

	if err := checkSentinelReplicas(operatorSpec); err != nil {
		return err
	}

	return nil
}

// splitMonitorRef parses "scope/name" into its parts. Rejects
// shapes the operator might typo into — empty string, missing
// slash, leading/trailing slash, multiple slashes.
func splitMonitorRef(ref string) (scope, name string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", errors.New("monitor reference is empty (expected \"scope/name\")")
	}

	parts := strings.Split(ref, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("expected exactly one \"/\" separating scope and name, got %d parts", len(parts))
	}

	scope, name = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if scope == "" || name == "" {
		return "", "", errors.New("scope and name must both be non-empty")
	}

	return scope, name, nil
}

// checkSentinelReplicas validates the operator's replicas value
// when the resource is in sentinel mode. Read from operatorSpec
// (not the post-merge map) because the merge default is 1 — and
// "operator omitted replicas in sentinel mode" should fall to
// the SENTINEL default (3), not the data-redis default (1).
//
// Returns:
//
//   - nil for replicas=1 (degenerate, observer-only — accepted
//     so the operator can prototype, with future milestones
//     surfacing a stderr warning when M-S2 lands)
//   - nil for replicas >= 3
//   - nil when replicas is omitted entirely (caller will treat
//     the missing value as "use sentinel default = 3" at expand
//     time in M-S1)
//   - error for replicas=2 (quorum math hostile: quorum=(2/2)+1=2,
//     so any single-pod outage kills failover capability — strictly
//     worse than running a single sentinel)
//   - error for replicas <= 0 (operator wrote nonsense)
func checkSentinelReplicas(operatorSpec map[string]any) error {
	raw, present := operatorSpec["replicas"]
	if !present {
		return nil
	}

	n, ok := asInt(raw)
	if !ok {
		return fmt.Errorf("replicas has unexpected type %T (expected integer)", raw)
	}

	switch {
	case n <= 0:
		return fmt.Errorf("replicas = %d is not allowed for sentinel resources (must be 1 or >= 3)", n)
	case n == 2:
		return errors.New("replicas = 2 is not allowed for sentinel resources: quorum would be 2, so losing any single pod breaks failover (strictly worse than 1 sentinel). Use 1 (observer-only, not HA) or >= 3 (HA).")
	}

	return nil
}

// asInt accepts the JSON-decoder's flavours of "integer-shaped
// number" — float64 from `encoding/json` is the common case;
// HCL2 → JSON sometimes round-trips integers through json.Number
// or int directly depending on the path. Returns false for
// non-numeric inputs so the caller can surface a typed error.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}

		return 0, false
	}

	return 0, false
}

// stripSentinelBlock removes the `sentinel` key from the merged
// spec so it doesn't leak into the emitted statefulset manifest.
// Useful for M-S0 where the sentinel-mode emission is stubbed —
// when M-S1 lands, the sentinel-mode path will branch BEFORE
// the manifest emit and never call this; the data-redis path
// also calls it as a defensive no-op so a malformed spec with
// `sentinel { enabled = false }` doesn't ship the block to the
// statefulset machinery downstream.
func stripSentinelBlock(merged map[string]any) {
	delete(merged, "sentinel")
}
