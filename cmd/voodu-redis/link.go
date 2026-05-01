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
}

// consumerEnvVar is the env-var name redis:link sets on the
// consumer to carry the connection URL. Matches the de facto
// standard for redis client libraries (REDIS_URL is what
// node-redis, redis-py, ioredis, go-redis all check first).
const consumerEnvVar = "REDIS_URL"

// cmdLink wires a redis provider to a consumer.
//
// Args (os.Args[2:]):
//
//	[0] provider scope/name (e.g. "clowk-lp/redis")
//	[1] consumer scope/name (e.g. "clowk-lp/web")
//
// Reads invocation context from stdin to find controller_url,
// then calls /describe and /config to gather the provider's
// state. Builds redis://default:<password>@<host>:<port> and
// emits a config_set action on the consumer.
//
// `-h` / `--help` short-circuits the network calls and prints
// usage on stdout for the operator.
func cmdLink() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(linkHelp)
		return nil
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: vd redis:link <provider-scope/name> <consumer-scope/name>")
	}

	providerRef := args[0]
	consumerRef := args[1]

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
	spec, err := client.fetchSpec("statefulset", provScope, provName)
	if err != nil {
		return fmt.Errorf("describe %s: %w", providerRef, err)
	}

	config, err := client.fetchConfig(provScope, provName)
	if err != nil {
		return fmt.Errorf("config get %s: %w", providerRef, err)
	}

	connURL, err := buildRedisURL(provScope, provName, spec, config)
	if err != nil {
		return fmt.Errorf("build url: %w", err)
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("linked %s → %s (%s)",
			refOrName(provScope, provName),
			refOrName(consScope, consName),
			consumerEnvVar),
		Actions: []dispatchAction{
			{
				Type:  "config_set",
				Scope: consScope,
				Name:  consName,
				KV:    map[string]string{consumerEnvVar: connURL},
			},
		},
	}

	return writeDispatchOutput(out)
}

// cmdUnlink emits a config_unset action on the consumer.
// Doesn't require provider state — just the consumer ref.
//
// Args (os.Args[2:]):
//
//	[0] (ignored, kept for symmetry with link) provider ref
//	[1] consumer scope/name to clear
func cmdUnlink() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(unlinkHelp)
		return nil
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: vd redis:unlink <provider-scope/name> <consumer-scope/name>")
	}

	consScope, consName := splitScopeName(args[1])
	if consName == "" {
		return fmt.Errorf("invalid consumer ref %q (expected scope/name)", args[1])
	}

	out := dispatchOutput{
		Message: fmt.Sprintf("unlinked %s (%s removed)",
			refOrName(consScope, consName),
			consumerEnvVar),
		Actions: []dispatchAction{
			{
				Type:  "config_unset",
				Scope: consScope,
				Name:  consName,
				Keys:  []string{consumerEnvVar},
			},
		},
	}

	return writeDispatchOutput(out)
}

// cmdNewPassword rotates the redis password.
//
// Args (os.Args[2:]):
//
//	[0] target scope/name (the redis to rotate)
//
// Generates a fresh random password, emits a config_set action
// on the redis itself. Operator runs `vd apply` next to
// propagate to redis.conf, then `vd redis:link` per consumer
// to refresh URLs.
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

	out := dispatchOutput{
		Message: fmt.Sprintf("rotated REDIS_PASSWORD for %s — run `vd apply` to refresh redis.conf, then `vd redis:link` per consumer to refresh URLs",
			refOrName(scope, name)),
		Actions: []dispatchAction{
			{
				Type:  "config_set",
				Scope: scope,
				Name:  name,
				KV:    map[string]string{passwordKey: fresh},
			},
		},
	}

	return writeDispatchOutput(out)
}

// Help text for each command — emitted when -h/--help is in
// os.Args. The plugin owns its own help; the CLI is a dumb
// pass-through that doesn't intercept these flags.
const (
	linkHelp = `Usage: vd redis:link <provider-scope/name> <consumer-scope/name>

Inject the redis provider's connection URL into the consumer's
config bucket. The consumer auto-restarts to pick up the new env.

Example:
  vd redis:link clowk-lp/redis clowk-lp/web`

	unlinkHelp = `Usage: vd redis:unlink <provider-scope/name> <consumer-scope/name>

Remove the previously-injected REDIS_URL from the consumer's
config bucket. Consumer auto-restarts.

Example:
  vd redis:unlink clowk-lp/redis clowk-lp/web`

	newPasswordHelp = `Usage: vd redis:new-password <scope/name>

Rotate the redis password. Generates a fresh 256-bit random
password and stores it in the redis's config bucket. Operator
runs 'vd apply' next to propagate to redis.conf (asset re-
materialise → rolling restart), then 'vd redis:link' for each
consumer that needs the new URL.

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
func buildRedisURL(scope, name string, spec, config map[string]any) (string, error) {
	host := redisHost(scope, name)
	port := redisPort(spec)

	password := redisPasswordFromConfig(config)
	if password == "" {
		password = redisPasswordFromSpecEnv(spec)
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
		Data map[string]string `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode config response: %w", err)
	}

	out := make(map[string]any, len(env.Data))
	for k, v := range env.Data {
		out[k] = v
	}

	return out, nil
}
