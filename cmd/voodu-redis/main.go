// Command voodu-redis expands a `redis "<scope>" "<name>" { … }`
// HCL block into a fan-out manifest pair: an `asset` carrying a
// production-ready redis.conf, plus a `statefulset` that
// bind-mounts that conf and runs `redis-server` against it.
//
// The conf bytes come from `bin/get-conf` (a bash script in
// the plugin dir) — operators can edit the conf in-place or
// substitute the script for a templated generator without
// rebuilding this binary.
//
// # Plugin contract
//
// stdin (one JSON object — the standard plugin expand request):
//
//	{ "kind": "redis", "scope": "...", "name": "...", "spec": {…} }
//
// stdout (envelope wrapping an array of two manifests):
//
//	{
//	  "status": "ok",
//	  "data": [
//	    { "kind": "asset",       "scope": "...", "name": "...", "spec": { "files": { "redis_conf": "<bytes>" } } },
//	    { "kind": "statefulset", "scope": "...", "name": "...", "spec": { … } }
//	  ]
//	}
//
// # Defaults (alias-contract: operator-wins for declared keys)
//
//	image       = "redis:7-alpine"
//	replicas    = 1
//	command     = ["redis-server", "/etc/redis/redis.conf"]
//	ports       = ["6379"]
//	volume_claim "data" { mount_path = "/data" }
//	volumes     = ["${asset.<name>.redis_conf}:/etc/redis/redis.conf:ro"]
//
// To inspect the manifest a bare block produces, run:
//
//	echo '{"kind":"redis","name":"x"}' | voodu-redis expand
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var version = "dev"

const defaultImage = "redis:7-alpine"

type expandRequest struct {
	Kind  string          `json:"kind"`
	Scope string          `json:"scope,omitempty"`
	Name  string          `json:"name"`
	Spec  json.RawMessage `json:"spec,omitempty"`

	// Config is the merged config bucket the controller pre-
	// fetched for (scope, name). Plugin uses this to read
	// existing state — notably REDIS_PASSWORD on re-applies so
	// the password stays stable across `vd apply` runs. Empty
	// on first apply; plugin generates state then and emits a
	// config_set action so the next apply sees it.
	Config map[string]string `json:"config,omitempty"`
}

type envelope struct {
	Status string `json:"status"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
}

type manifest struct {
	Kind  string         `json:"kind"`
	Scope string         `json:"scope,omitempty"`
	Name  string         `json:"name"`
	Spec  map[string]any `json:"spec"`
}

func main() {
	if len(os.Args) < 2 {
		emitErr("usage: voodu-redis <expand|link|unlink|new-password|info|help|--version>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "--version", "-v", "version":
		fmt.Println(version)

	case "expand":
		if err := cmdExpand(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	case "link":
		if err := cmdLink(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	case "unlink":
		if err := cmdUnlink(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	case "new-password":
		if err := cmdNewPassword(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	case "info":
		if err := cmdInfo(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	case "help":
		// `vd redis -h` / `vd redis --help` reaches us here
		// (CLI synthesizes a "help" command call). Plugin owns
		// its own overview text. No envelope — operator wants
		// plain text on stdout.
		printPluginOverview()

	default:
		emitErr(fmt.Sprintf("unknown subcommand %q (want expand|link|unlink|new-password|info|help)", os.Args[1]))
		os.Exit(1)
	}
}

// printPluginOverview emits the plugin-level help — what
// commands voodu-redis exposes, brief description of each, how
// to invoke. This is what `vd redis -h` shows the operator.
//
// The CLI doesn't auto-render from plugin.yml metadata —
// passthrough means the plugin author owns the help text
// verbatim, so operators see the real example invocations
// (with redis, not <plugin> placeholder), the actual
// arg shapes, and any caveats specific to this plugin.
func printPluginOverview() {
	fmt.Println(`voodu-redis — managed redis instances via the voodu plugin contract

Commands:
  vd redis:link <provider> <consumer>
      Inject the redis URL into the consumer's config.
      Consumer auto-restarts to pick up the new env.

  vd redis:unlink <provider> <consumer>
      Remove the previously-injected REDIS_URL from the consumer.

  vd redis:new-password <ref>
      Rotate the redis password. Operator runs 'vd apply' next
      to propagate to redis.conf, then 'vd redis:link' per
      consumer to refresh URLs.

  vd redis:info <ref>
      Show connection info for a redis instance: URL, port,
      data volume, password storage location.

Per-command help:
  vd redis:<command> -h

The plugin is invoked by the controller; operators don't run
this binary directly. See https://github.com/thadeu/voodu-redis
for source.`)
}

// cmdExpand reads the operator's block spec from stdin, merges
// it on top of plugin defaults, fetches the redis.conf via
// `bin/get-conf` (a sibling script in the plugin dir), and
// emits an [asset, statefulset] pair.
//
// Password lifecycle (idempotent across re-applies):
//
//   - Read REDIS_PASSWORD from req.Config (controller pre-fetched
//     the merged bucket).
//   - If present: reuse — append `requirepass <existing>` to the
//     conf bytes. No action emitted (already stored).
//   - If absent: generate a strong random password, append
//     requirepass with the new value, AND emit a config_set
//     action so the controller persists it. Subsequent expands
//     see the persisted value and the password stays stable.
//
// This means the FIRST `vd apply` of a redis writes the password
// once and every later apply replays the same bytes — the asset
// digest doesn't churn unless something else changes.
//
// The subprocess call to get-conf is deliberate — it lets
// operators substitute the script with a templated generator
// without rebuilding this Go binary. The contract is "stdout
// of get-conf is the redis.conf bytes, verbatim".
func cmdExpand() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	var req expandRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("decode expand request: %w", err)
	}

	if req.Name == "" {
		return errors.New("expand request missing required field 'name'")
	}

	confBytes, err := readGeneratedConf()
	if err != nil {
		return fmt.Errorf("get-conf: %w", err)
	}

	if len(confBytes) == 0 {
		return errors.New("get-conf returned empty output (redis.conf must not be empty)")
	}

	var operatorSpec map[string]any

	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &operatorSpec); err != nil {
			return fmt.Errorf("decode block spec: %w", err)
		}
	}

	merged := mergeSpec(composeDefaults(req.Scope, req.Name), operatorSpec)

	password, isNew, err := resolveOrGeneratePassword(req.Config)
	if err != nil {
		return fmt.Errorf("resolve password: %w", err)
	}

	confWithAuth := appendRequirepass(confBytes, password)

	// The entrypoint script is rendered with the instance's
	// (scope, name) baked in so the master FQDN inside the
	// script is the right one for THIS redis. Pure function —
	// same inputs always produce the same bytes, so the asset
	// digest stays stable across replays unless scope or name
	// change (which would re-emit anyway).
	entrypointBytes := renderEntrypointScript(req.Scope, req.Name)

	asset := manifest{
		Kind:  "asset",
		Scope: req.Scope,
		Name:  req.Name,
		Spec: map[string]any{
			"files": map[string]any{
				"redis_conf":       string(confWithAuth),
				entrypointAssetKey: entrypointBytes,
			},
		},
	}

	// Note: the asset kind writes files with mode 0644, so the
	// wrapper script lands non-executable on the host. We don't
	// need the executable bit because composeDefaults invokes
	// the script via `sh <path>` — sh doesn't care about the
	// +x bit, only that the file is readable.

	statefulset := manifest{
		Kind:  "statefulset",
		Scope: req.Scope,
		Name:  req.Name,
		Spec:  merged,
	}

	out := expandedPayload{
		Manifests: []manifest{asset, statefulset},
	}

	if isNew {
		// Persist the freshly-generated password so later expands
		// pick it up via Config and stay idempotent. Action lands
		// on the same (scope, name) the redis itself uses; the
		// dispatch endpoint pulls REDIS_PASSWORD from there
		// when an operator runs `vd redis:link`.
		out.Actions = []dispatchAction{
			{
				Type:  "config_set",
				Scope: req.Scope,
				Name:  req.Name,
				KV:    map[string]string{"REDIS_PASSWORD": password},
			},
		}
	}

	emitOK(out)

	return nil
}

// expandedPayload is the new envelope-data shape voodu-redis
// emits. Compatible with the controller's decodeExpandedPayload
// dispatcher: the {manifests, actions} object form is recognised
// alongside the legacy array shape.
type expandedPayload struct {
	Manifests []manifest       `json:"manifests"`
	Actions   []dispatchAction `json:"actions,omitempty"`
}

// readGeneratedConf invokes bin/get-conf in the plugin directory
// and returns its stdout. VOODU_PLUGIN_DIR is injected by the
// controller when running plugins; falls back to the directory
// containing the binary itself for direct CLI invocation
// (smoke testing, `voodu-redis expand < req.json`).
func readGeneratedConf() ([]byte, error) {
	dir := os.Getenv("VOODU_PLUGIN_DIR")
	if dir == "" {
		exe, err := os.Executable()
		if err == nil {
			dir = filepath.Dir(filepath.Dir(exe))
		}
	}

	if dir == "" {
		return nil, errors.New("plugin dir not resolvable (VOODU_PLUGIN_DIR unset and exe path lookup failed)")
	}

	script := filepath.Join(dir, "bin", "get-conf")

	out, err := exec.Command(script).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s exited %d: %s", script, exitErr.ExitCode(), exitErr.Stderr)
		}

		return nil, fmt.Errorf("%s: %w", script, err)
	}

	return out, nil
}

// composeDefaults is the single source of truth for what the
// plugin contributes when the operator omits a field. Everything
// here is overridable per the merge rules in mergeSpec:
//
//   - `env` deep-merges (operator + plugin coexist by key)
//   - `volumes` additive-merges by destination path (plugin's
//     defaults always present unless operator declares the same
//     destination, in which case operator wins for that one
//     entry)
//   - everything else: operator-wins outright (alias contract)
//
// `volumes` and `command` are scope+name parameterised so the
// 4-segment asset ref `${asset.<scope>.<name>.redis_conf}`
// addresses the asset emitted alongside in the same expand call.
//
// Replication topology (M2):
//
//   - The wrapper script at /usr/local/bin/voodu-redis-entrypoint
//     becomes the container's command. It reads VOODU_REPLICA_ORDINAL
//     (per-pod, set by the controller) and REDIS_MASTER_ORDINAL
//     (config bucket var, default 0) and execs redis-server with
//     the right --replicaof flag. Single-replica deployments take
//     the master branch and behave identically to the pre-M2 plugin.
//   - The wrapper is shipped as a second asset key alongside
//     redis_conf, so both files come from the same asset emission
//     and are version-coupled (re-rendering one always re-renders
//     the other).
func composeDefaults(scope, name string) map[string]any {
	return map[string]any{
		"image":    defaultImage,
		"replicas": 1,
		"ports":    []any{"6379"},
		// Invoke the wrapper via `sh` so the asset's default
		// 0644 mode is enough — the controller's atomicWrite
		// doesn't currently support a per-file mode override,
		// and `sh <script>` works regardless of the executable
		// bit. If/when the asset kind grows file_modes, this
		// can drop the explicit `sh`.
		"command": []any{"sh", entrypointMountPath},
		"volumes": []any{
			"${asset." + scope + "." + name + ".redis_conf}:/etc/redis/redis.conf:ro",
			"${asset." + scope + "." + name + "." + entrypointAssetKey + "}:" + entrypointMountPath + ":ro",
		},
		"volume_claims": []any{
			map[string]any{
				"name":       "data",
				"mount_path": "/data",
			},
		},
	}
}

// mergeSpec applies operator overrides on top of plugin
// defaults. Per-key strategy:
//
//   - `env` deep-merges so operator vars and plugin vars coexist
//   - `volumes` additive-merges by destination path: plugin's
//     defaults are always preserved (operator can ADD without
//     losing the redis.conf bind), and operator entries with the
//     same destination as a plugin default REPLACE that single
//     default (granular override). Avoids docker's
//     "duplicate mount point" error too — same target appears
//     once in the final list.
//   - everything else: operator-wins outright (alias contract)
//
// Empty-but-present operator values (e.g. `volume_claims = []`)
// are honoured verbatim.
func mergeSpec(defaults, operator map[string]any) map[string]any {
	out := make(map[string]any, len(defaults))

	for k, v := range defaults {
		out[k] = v
	}

	for k, v := range operator {
		switch k {
		case "env":
			out[k] = mergeEnv(out[k], v)

		case "volumes":
			out[k] = mergeVolumes(out[k], v)

		default:
			out[k] = v
		}
	}

	return out
}

func mergeEnv(defaultEnv, operatorEnv any) any {
	a, _ := defaultEnv.(map[string]any)
	b, _ := operatorEnv.(map[string]any)

	if len(a) == 0 && len(b) == 0 {
		return nil
	}

	out := make(map[string]any, len(a)+len(b))

	for k, v := range a {
		out[k] = v
	}

	for k, v := range b {
		out[k] = v
	}

	return out
}

// mergeVolumes performs additive merge by destination path.
// Plugin defaults appear first (preserve their order); operator
// entries either ADD (new destination) or REPLACE (existing
// destination — operator wins for that single entry, position
// preserved from where the original was).
//
// Why dedup matters: docker rejects `docker run` when two -v
// flags target the same in-container path with
// "Duplicate mount point: /path". Without this dedup the
// operator would have to remove the plugin default manually,
// defeating the "always-on default + selective override" intent.
//
// Format expected: "src:dst[:mode]" (Linux convention). Entries
// that don't parse (single-token, missing colon) are kept
// verbatim under their literal key — better to surface as a
// downstream error than silently coerce.
func mergeVolumes(defaultVols, operatorVols any) any {
	a, _ := defaultVols.([]any)
	b, _ := operatorVols.([]any)

	if len(a) == 0 && len(b) == 0 {
		return nil
	}

	type entry struct {
		raw    string
		target string
	}

	byTarget := make(map[string]int, len(a)+len(b))
	order := make([]entry, 0, len(a)+len(b))

	addOrReplace := func(s string) {
		t := volumeTarget(s)

		if t == "" {
			// Unparseable — keep verbatim as a unique entry,
			// indexed by the raw string so operator's identical
			// duplicate doesn't double-add. Downstream docker
			// will surface the malformed mount as the real
			// error.
			t = "_raw:" + s
		}

		if idx, exists := byTarget[t]; exists {
			order[idx] = entry{raw: s, target: t}
			return
		}

		byTarget[t] = len(order)
		order = append(order, entry{raw: s, target: t})
	}

	for _, v := range a {
		if s, ok := v.(string); ok {
			addOrReplace(s)
		}
	}

	for _, v := range b {
		if s, ok := v.(string); ok {
			addOrReplace(s)
		}
	}

	out := make([]any, 0, len(order))
	for _, e := range order {
		out = append(out, e.raw)
	}

	return out
}

// volumeTarget extracts the in-container destination path from
// a "src:dst[:mode]" volume spec. Returns "" when the spec is
// malformed (no colon).
func volumeTarget(s string) string {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return ""
	}

	return parts[1]
}

func emitOK(data any) {
	enc := json.NewEncoder(os.Stdout)

	_ = enc.Encode(envelope{Status: "ok", Data: data})
}

func emitErr(msg string) {
	enc := json.NewEncoder(os.Stderr)

	_ = enc.Encode(envelope{Status: "error", Error: msg})
}
