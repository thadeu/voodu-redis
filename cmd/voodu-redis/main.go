// Command voodu-redis expands a `redis "<scope>" "<name>" { … }`
// HCL block into a statefulset manifest. The macro is an ALIAS
// of statefulset with sensible defaults: every statefulset
// attribute the operator declares wins; the plugin only fills
// in what's missing.
//
// Custom redis.conf / ACLs / TLS files are supplied via the
// `asset` kind separately and referenced in `volumes` — the
// plugin doesn't carry knobs for them. Same posture as every
// other macro plugin: dumb alias, smart asset kind.
//
// # Plugin contract
//
// stdin: { kind, scope, name, spec } — same shape voodu's
// controller persists for any kind.
//
// stdout: envelope with data = a statefulset Manifest.
//
// # Defaults the plugin contributes
//
//	image       = "redis:7-alpine"
//	replicas    = 1
//	command     = ["redis-server", "--appendonly", "yes"]
//	ports       = ["6379"]
//	volume_claim "data" { mount_path = "/data" }
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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

type statefulset struct {
	Kind  string         `json:"kind"`
	Scope string         `json:"scope,omitempty"`
	Name  string         `json:"name"`
	Spec  map[string]any `json:"spec"`
}

func main() {
	if len(os.Args) < 2 {
		emitErr("usage: voodu-redis <expand|defaults|--version>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "--version", "-v", "version":
		fmt.Println(version)

	case "defaults":
		emitOK(composeDefaults())

	case "expand":
		if err := cmdExpand(); err != nil {
			emitErr(err.Error())
			os.Exit(1)
		}

	default:
		emitErr(fmt.Sprintf("unknown subcommand %q (want expand|defaults)", os.Args[1]))
		os.Exit(1)
	}
}

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
		return fmt.Errorf("expand request missing required field 'name'")
	}

	var operatorSpec map[string]any

	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &operatorSpec); err != nil {
			return fmt.Errorf("decode block spec: %w", err)
		}
	}

	merged := mergeSpec(composeDefaults(), operatorSpec)

	emitOK(statefulset{
		Kind:  "statefulset",
		Scope: req.Scope,
		Name:  req.Name,
		Spec:  merged,
	})

	return nil
}

// composeDefaults is the single source of truth for what the
// plugin contributes when the operator omits a field. The
// command runs `redis-server --appendonly yes` so a zero-config
// block (`redis "data" "cache" {}`) gets a durable cache out of
// the box. Operator who needs a real config supplies their own
// `command` + a redis.conf via an `asset` block + `volumes`.
func composeDefaults() map[string]any {
	return map[string]any{
		"image":    defaultImage,
		"replicas": 1,
		"command":  []any{"redis-server", "--appendonly", "yes"},
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
