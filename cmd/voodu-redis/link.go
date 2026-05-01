// Plugin command implementations for `vd redis:link` and
// `vd redis:unlink`. The dispatch endpoint on the controller
// (POST /plugin/redis/link) feeds the plugin a JSON envelope on
// stdin describing the provider (`from`) and consumer (`to`)
// manifests; this file decodes that envelope, builds the
// connection URL, and emits an `actions` list the controller
// applies on the consumer's config bucket.
//
// The plugin stays purely transformative — no controller HTTP
// calls, no store access. Every store mutation is described by
// the `actions` list in the response and executed by the
// controller. Same posture as `expand`: input → output.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// dispatchInput mirrors the JSON shape the controller writes to
// the plugin's stdin in handlers_plugin_dispatch.go. We don't
// share types with the controller (separate repo, separate
// binary) so the schema is repeated here. Field names + JSON
// tags MUST match — operator-facing breakage would surface as
// "plugin link silently does nothing".
type dispatchInput struct {
	Plugin  string                 `json:"plugin"`
	Command string                 `json:"command"`
	From    *dispatchRefWithState  `json:"from,omitempty"`
	To      *dispatchRefWithState  `json:"to,omitempty"`
	Args    []string               `json:"args,omitempty"`
	Extra   map[string]any         `json:"extra,omitempty"`
}

type dispatchRefWithState struct {
	Kind   string         `json:"kind,omitempty"`
	Scope  string         `json:"scope,omitempty"`
	Name   string         `json:"name"`
	Spec   map[string]any `json:"spec,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// dispatchOutput is the envelope-data shape the controller
// expects on stdout. `message` is the operator-facing one-liner
// the CLI prints after success; `actions` is the queue the
// controller applies post-invoke.
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
}

// consumerEnvVar is the env-var name redis:link sets on the
// consumer to carry the connection URL. Matches the de facto
// standard for redis client libraries (REDIS_URL is what
// node-redis, redis-py, ioredis, go-redis all check first).
const consumerEnvVar = "REDIS_URL"

// cmdLink reads the dispatch envelope from stdin, builds the
// connection URL using the provider's spec + config, and emits
// a config_set action the controller applies on the consumer.
//
// Password resolution priority (matches the description in the
// link/unlink discussion):
//   1. `from.config["REDIS_PASSWORD"]` — operator set via
//      `vd config set -s <provider-scope> -n <provider-name>
//      REDIS_PASSWORD=...`. Most common shape for secrets.
//   2. `from.spec.env["REDIS_PASSWORD"]` — declared inline in
//      the operator's HCL `env = {...}` block.
//   3. None — emit the URL without auth (`redis://host:port`).
//      Operator is using an open redis. Common in dev / behind
//      private networks.
//
// User defaults to "default" (Redis 6+ ACL convention) so
// `redis://default:password@host:port` is the form clients
// expect when a password is set.
//
// Host is the FQDN voodu0 publishes for the provider's scoped
// alias: `<name>.<scope>.voodu`. Port comes from the spec's
// declared ports[0] (operator-supplied or plugin default 6379).
func cmdLink() error {
	in, err := readDispatchInput()
	if err != nil {
		return err
	}

	if in.From == nil {
		return fmt.Errorf("link: missing `from` (provider)")
	}

	if in.To == nil {
		return fmt.Errorf("link: missing `to` (consumer)")
	}

	if in.From.Name == "" {
		return fmt.Errorf("link: provider name must be non-empty")
	}

	if in.To.Name == "" {
		return fmt.Errorf("link: consumer name must be non-empty")
	}

	connURL, err := buildRedisURL(in.From)
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("linked %s → %s (%s)",
			refOrName(in.From.Scope, in.From.Name),
			refOrName(in.To.Scope, in.To.Name),
			consumerEnvVar),
		Actions: []dispatchAction{
			{
				Type:  "config_set",
				Scope: in.To.Scope,
				Name:  in.To.Name,
				KV:    map[string]string{consumerEnvVar: connURL},
			},
		},
	}

	return writeDispatchOutput(out)
}

// cmdNewPassword rotates the redis password. Generates a fresh
// random password and emits a config_set action so the next
// `vd apply` of the redis manifest picks it up via cmdExpand's
// idempotent password resolution and re-materialises the asset
// with the new requirepass directive — that asset content
// change cascades through the stamping pipeline as a normal
// rolling restart.
//
// Operator workflow:
//
//   vd redis:new-password clowk-lp/redis     # rotate the secret
//   vd apply -f infra/redis                   # propagate to redis.conf
//   vd redis:link clowk-lp/redis clowk-lp/web # re-emit URL with new pwd
//
// The two follow-up steps are manual on purpose for v1: the
// plugin doesn't track linked consumers yet (no
// REDIS_LINKED_CONSUMERS bookkeeping), so it can't auto-reissue
// every consumer's URL. Future iteration: track + auto-reissue.
//
// Reads `from` (the redis being rotated). `to` is intentionally
// not consulted — rotate is a unary verb.
func cmdNewPassword() error {
	in, err := readDispatchInput()
	if err != nil {
		return err
	}

	if in.From == nil {
		return fmt.Errorf("new-password: missing target (`from`)")
	}

	if in.From.Name == "" {
		return fmt.Errorf("new-password: target name must be non-empty")
	}

	fresh, err := generatePassword()
	if err != nil {
		return fmt.Errorf("new-password: %w", err)
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("rotated REDIS_PASSWORD for %s — run `vd apply -f <your-hcl>` to refresh redis.conf, then `vd redis:link` for each consumer that needs the new URL",
			refOrName(in.From.Scope, in.From.Name)),
		Actions: []dispatchAction{
			{
				Type:  "config_set",
				Scope: in.From.Scope,
				Name:  in.From.Name,
				KV:    map[string]string{passwordKey: fresh},
			},
		},
	}

	return writeDispatchOutput(out)
}

// cmdUnlink emits a config_unset action on the consumer to
// remove the previously-injected REDIS_URL. Doesn't actually
// require provider state — the consumer ref alone is enough —
// but we accept the same input shape as `link` for symmetry,
// so the operator's call shape is identical between the two.
func cmdUnlink() error {
	in, err := readDispatchInput()
	if err != nil {
		return err
	}

	if in.To == nil {
		return fmt.Errorf("unlink: missing `to` (consumer)")
	}

	if in.To.Name == "" {
		return fmt.Errorf("unlink: consumer name must be non-empty")
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("unlinked %s (%s removed)",
			refOrName(in.To.Scope, in.To.Name),
			consumerEnvVar),
		Actions: []dispatchAction{
			{
				Type:  "config_unset",
				Scope: in.To.Scope,
				Name:  in.To.Name,
				Keys:  []string{consumerEnvVar},
			},
		},
	}

	return writeDispatchOutput(out)
}

// buildRedisURL constructs the connection URL clients should
// dial. Returns an error only on outright invalid state (no
// host derivable); falls through to a no-auth URL when the
// password is just absent (legitimate use case).
func buildRedisURL(from *dispatchRefWithState) (string, error) {
	host := redisHost(from.Scope, from.Name)
	port := redisPort(from.Spec)

	password := redisPasswordFromConfig(from.Config)
	if password == "" {
		password = redisPasswordFromSpecEnv(from.Spec)
	}

	u := &url.URL{
		Scheme: "redis",
		Host:   fmt.Sprintf("%s:%d", host, port),
	}

	if password != "" {
		u.User = url.UserPassword("default", password)
	}

	return u.String(), nil
}

// redisHost is the voodu0 FQDN for a scoped statefulset:
// `<name>.<scope>.voodu`. Unscoped (singleton) plugins fall
// back to `<name>.voodu` — same convention BuildNetworkAliases
// in the controller emits.
func redisHost(scope, name string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	name = strings.ToLower(strings.TrimSpace(name))

	if scope == "" {
		return name + ".voodu"
	}

	return name + "." + scope + ".voodu"
}

// redisPort returns the port the consumer should dial. Reads
// `spec.ports[0]`; falls back to 6379 (redis default) when the
// spec didn't declare any. Plugin defaults already include
// `["6379"]` so this is mostly belt-and-braces.
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

	// Strip "host:" prefix if operator wrote loopback-only form
	// like "127.0.0.1:6379". The container always listens on
	// the bare port; the host bind has no bearing on the
	// container-network URL we're building here.
	if i := strings.LastIndex(first, ":"); i >= 0 {
		first = first[i+1:]
	}

	var port int

	if _, err := fmt.Sscanf(first, "%d", &port); err != nil || port <= 0 {
		return defaultPort
	}

	return port
}

// redisPasswordFromConfig pulls REDIS_PASSWORD from the merged
// config bucket the controller pre-fetched. Returns "" when
// absent — caller falls through to the spec.env path next.
func redisPasswordFromConfig(config map[string]any) string {
	if config == nil {
		return ""
	}

	if v, ok := config["REDIS_PASSWORD"].(string); ok {
		return v
	}

	return ""
}

// redisPasswordFromSpecEnv pulls REDIS_PASSWORD from the
// provider's spec.env (HCL-declared). Returns "" when absent.
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

// readDispatchInput reads the controller's stdin payload and
// decodes it into the typed shape. Empty stdin is an error —
// the controller always sends at least `{plugin, command}`.
func readDispatchInput() (*dispatchInput, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("empty stdin (controller should always supply a JSON payload)")
	}

	var in dispatchInput

	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode stdin: %w", err)
	}

	return &in, nil
}

// writeDispatchOutput encodes the dispatch result inside the
// standard plugin envelope (status=ok, data=<output>) so the
// controller's envelope-parsing layer picks it up cleanly.
func writeDispatchOutput(out dispatchOutput) error {
	enc := json.NewEncoder(os.Stdout)

	return enc.Encode(envelope{Status: "ok", Data: out})
}

// refOrName produces the operator-facing identifier for a
// manifest. Scoped resources render as "scope/name"; unscoped
// (rare for redis) as just "name". Used in the success message
// so operators see the shape they typed in.
func refOrName(scope, name string) string {
	if scope == "" {
		return name
	}

	return scope + "/" + name
}
