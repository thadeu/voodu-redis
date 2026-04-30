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
)

var version = "dev"

const defaultImage = "redis:7-alpine"

type expandRequest struct {
	Kind  string          `json:"kind"`
	Scope string          `json:"scope,omitempty"`
	Name  string          `json:"name"`
	Spec  json.RawMessage `json:"spec,omitempty"`
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
		emitErr("usage: voodu-redis <expand|--version>")
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

	default:
		emitErr(fmt.Sprintf("unknown subcommand %q (want expand)", os.Args[1]))
		os.Exit(1)
	}
}

// cmdExpand reads the operator's block spec from stdin, merges
// it on top of plugin defaults, fetches the redis.conf via
// `bin/get-conf` (a sibling script in the plugin dir), and
// emits an [asset, statefulset] pair.
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

	merged := mergeSpec(composeDefaults(), operatorSpec)

	// Plugin owns the volume bind + command pointing at the
	// asset, BUT only when operator hasn't supplied their own.
	// Operator-wins shallow merge already handled `command` /
	// `volumes` if they're in operatorSpec — these branches
	// just inject the plugin defaults for the absent case.
	if _, op := operatorSpec["volumes"]; !op {
		merged["volumes"] = []any{
			"${asset." + req.Name + ".redis_conf}:/etc/redis/redis.conf:ro",
		}
	}

	if _, op := operatorSpec["command"]; !op {
		merged["command"] = []any{"redis-server", "/etc/redis/redis.conf"}
	}

	asset := manifest{
		Kind:  "asset",
		Scope: req.Scope,
		Name:  req.Name,
		Spec: map[string]any{
			"files": map[string]any{
				"redis_conf": string(confBytes),
			},
		},
	}

	statefulset := manifest{
		Kind:  "statefulset",
		Scope: req.Scope,
		Name:  req.Name,
		Spec:  merged,
	}

	emitOK([]manifest{asset, statefulset})

	return nil
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
// here is overridable via the alias contract: operator declares
// the same key on the redis block, operator wins.
func composeDefaults() map[string]any {
	return map[string]any{
		"image":    defaultImage,
		"replicas": 1,
		"ports":    []any{"6379"},
		"volume_claims": []any{
			map[string]any{
				"name":       "data",
				"mount_path": "/data",
			},
		},
	}
}

// mergeSpec applies operator overrides on top of plugin
// defaults. Shallow merge — operator wins outright per key —
// except for `env`, which deep-merges.
//
// Empty-but-present operator values (e.g. `volume_claims = []`)
// are honoured verbatim.
func mergeSpec(defaults, operator map[string]any) map[string]any {
	out := make(map[string]any, len(defaults))

	for k, v := range defaults {
		out[k] = v
	}

	for k, v := range operator {
		if k == "env" {
			out[k] = mergeEnv(out[k], v)
			continue
		}

		out[k] = v
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

func emitOK(data any) {
	enc := json.NewEncoder(os.Stdout)

	_ = enc.Encode(envelope{Status: "ok", Data: data})
}

func emitErr(msg string) {
	enc := json.NewEncoder(os.Stderr)

	_ = enc.Encode(envelope{Status: "error", Error: msg})
}
