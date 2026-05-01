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

	connURL, _ := buildRedisURL(scope, name, spec, config)

	password := redisPasswordFromConfig(config)
	passwordSource := "config bucket (vd config get -s " + scope + " -n " + name + " REDIS_PASSWORD)"

	if password == "" {
		password = redisPasswordFromSpecEnv(spec)
		passwordSource = "HCL spec.env"
	}

	if password == "" {
		passwordSource = "(none — open auth, no requirepass set)"
	}

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

	out := strings.Builder{}

	fmt.Fprintf(&out, "redis/%s\n\n", refOrName(scope, name))
	fmt.Fprintf(&out, "  image:           %s\n", image)
	fmt.Fprintf(&out, "  host:            %s\n", host)
	fmt.Fprintf(&out, "  port:            %d\n", port)
	fmt.Fprintf(&out, "  data volume:     %s\n", dataVolume)
	fmt.Fprintln(&out)
	fmt.Fprintf(&out, "connect:\n")
	// Connection URL is shown verbatim (with the password) so
	// operator gets a copy/paste-ready string for the redis-cli
	// or app config. Trade-off: visible on screen-shares.
	// Operators wanting redaction can run `vd redis:info | sed
	// 's/:[^@]*@/:****@/'` on their side; the plugin opts for
	// utility over caution by default.
	fmt.Fprintf(&out, "  url:             %s\n", connURL)

	// Plain text on stdout — no envelope. Operator wants to
	// read this, not parse JSON. Server passes it through as
	// the `message` field of the dispatch response.
	fmt.Print(out.String())

	return nil
}

const infoHelp = `Usage: vd redis:info <scope/name>

Show connection info, password storage, and data volume for a
redis instance managed by voodu-redis. The connection URL is
emitted verbatim (with password) so it's copy/paste-ready for
redis-cli or app config — be aware on screen-shares.

Example:
  vd redis:info clowk-lp/redis`
