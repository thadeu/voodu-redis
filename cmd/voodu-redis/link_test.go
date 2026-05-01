package main

import (
	"strings"
	"testing"
)

// TestRedisHost pins the FQDN convention. The CLI dispatcher
// + plugin agree on `<name>.<scope>.voodu` for scoped resources;
// breaking this would silently produce URLs the operator can't
// dial inside the voodu0 network.
func TestRedisHost(t *testing.T) {
	cases := []struct {
		scope, name, want string
	}{
		{"clowk-lp", "redis", "redis.clowk-lp.voodu"},
		{"data", "cache", "cache.data.voodu"},
		// Unscoped — singleton on the host.
		{"", "cache", "cache.voodu"},
		// Trims and lowercases on both ends.
		{" CLOWK-LP ", "  Redis  ", "redis.clowk-lp.voodu"},
	}

	for _, tc := range cases {
		got := redisHost(tc.scope, tc.name)
		if got != tc.want {
			t.Errorf("host(%q, %q) = %q, want %q", tc.scope, tc.name, got, tc.want)
		}
	}
}

// TestRedisPort covers the spec parser: takes ports[0], strips
// any "host:" prefix, falls back to 6379 on absent / malformed
// values. Operators write `ports = ["6379"]` and expect that
// to flow through; voodu's loopback-by-default may have rewritten
// it to `127.0.0.1:6379` and the URL still needs the bare port.
func TestRedisPort(t *testing.T) {
	cases := []struct {
		spec map[string]any
		want int
	}{
		// Empty / nil → default.
		{nil, 6379},
		{map[string]any{}, 6379},
		// Bare port.
		{map[string]any{"ports": []any{"6379"}}, 6379},
		// Port-only with non-default value.
		{map[string]any{"ports": []any{"6380"}}, 6380},
		// Loopback host:port form (voodu's default rewrite).
		{map[string]any{"ports": []any{"127.0.0.1:6379"}}, 6379},
		// Explicit-public form.
		{map[string]any{"ports": []any{"0.0.0.0:6380"}}, 6380},
		// Multiple ports — first wins.
		{map[string]any{"ports": []any{"6379", "16379"}}, 6379},
		// Garbage — fall through.
		{map[string]any{"ports": []any{"not-a-port"}}, 6379},
		{map[string]any{"ports": []any{}}, 6379},
		{map[string]any{"ports": "not-a-list"}, 6379},
	}

	for _, tc := range cases {
		got := redisPort(tc.spec)
		if got != tc.want {
			t.Errorf("port(%+v) = %d, want %d", tc.spec, got, tc.want)
		}
	}
}

// TestRedisPasswordFromConfig + TestRedisPasswordFromSpecEnv
// pin the lookup priority. Without these tests, a future
// refactor could reorder lookup or drop a source and the
// operator's password would silently fall through to a
// no-auth URL.
func TestRedisPasswordFromConfig(t *testing.T) {
	cases := []struct {
		config map[string]any
		want   string
	}{
		{nil, ""},
		{map[string]any{}, ""},
		{map[string]any{"REDIS_PASSWORD": "s3cret"}, "s3cret"},
		{map[string]any{"OTHER_KEY": "x"}, ""},
		// Type mismatch — non-string value treated as missing.
		{map[string]any{"REDIS_PASSWORD": 123}, ""},
	}

	for _, tc := range cases {
		got := redisPasswordFromConfig(tc.config)
		if got != tc.want {
			t.Errorf("config(%+v) = %q, want %q", tc.config, got, tc.want)
		}
	}
}

func TestRedisPasswordFromSpecEnv(t *testing.T) {
	cases := []struct {
		spec map[string]any
		want string
	}{
		{nil, ""},
		{map[string]any{}, ""},
		{map[string]any{"env": map[string]any{}}, ""},
		{map[string]any{"env": map[string]any{"REDIS_PASSWORD": "s3cret"}}, "s3cret"},
		{map[string]any{"env": map[string]any{"OTHER": "x"}}, ""},
		// env not a map — treated as absent.
		{map[string]any{"env": "weird"}, ""},
	}

	for _, tc := range cases {
		got := redisPasswordFromSpecEnv(tc.spec)
		if got != tc.want {
			t.Errorf("specEnv(%+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// TestBuildRedisURL_PriorityOrder is the integration of the
// password-source priority: config wins over spec.env, both
// can be absent and the URL falls through to no-auth.
func TestBuildRedisURL_PriorityOrder(t *testing.T) {
	cases := []struct {
		name                string
		scope, providerName string
		spec, config        map[string]any
		want                string
	}{
		{
			name: "config wins over spec.env",
			scope: "clowk-lp", providerName: "redis",
			spec: map[string]any{
				"ports": []any{"6379"},
				"env":   map[string]any{"REDIS_PASSWORD": "from-env"},
			},
			config: map[string]any{"REDIS_PASSWORD": "from-config"},
			want:   "redis://default:from-config@redis.clowk-lp.voodu:6379",
		},
		{
			name: "spec.env when no config",
			scope: "clowk-lp", providerName: "redis",
			spec: map[string]any{
				"ports": []any{"6379"},
				"env":   map[string]any{"REDIS_PASSWORD": "from-env"},
			},
			want: "redis://default:from-env@redis.clowk-lp.voodu:6379",
		},
		{
			name: "no auth when neither set",
			scope: "clowk-lp", providerName: "redis",
			spec: map[string]any{"ports": []any{"6379"}},
			want: "redis://redis.clowk-lp.voodu:6379",
		},
		{
			name: "non-default port carries through",
			scope: "data", providerName: "cache",
			spec:   map[string]any{"ports": []any{"6380"}},
			config: map[string]any{"REDIS_PASSWORD": "p"},
			want:   "redis://default:p@cache.data.voodu:6380",
		},
		{
			name: "unscoped redis (rare but legal)",
			scope: "", providerName: "cache",
			spec: map[string]any{"ports": []any{"6379"}},
			want: "redis://cache.voodu:6379",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildRedisURL(tc.scope, tc.providerName, tc.spec, tc.config)
			if err != nil {
				t.Fatalf("err: %v", err)
			}

			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestBuildRedisURL_PasswordWithSpecialChars: passwords often
// contain `:`, `@`, `/`, `?`, `#` — characters that have URL
// meaning. net/url's UserPassword does the percent-escape, so
// the URL stays parseable. Pin the behaviour so we don't drift
// into manual concatenation later.
func TestBuildRedisURL_PasswordWithSpecialChars(t *testing.T) {
	got, err := buildRedisURL(
		"clowk-lp", "redis",
		map[string]any{"ports": []any{"6379"}},
		map[string]any{"REDIS_PASSWORD": "p@ss/word:#1"},
	)
	if err != nil {
		t.Fatal(err)
	}

	// The exact percent-escape sequence depends on net/url
	// behaviour but the password chars MUST be escaped — they
	// don't appear verbatim.
	if strings.Contains(got, "p@ss/word:#1") {
		t.Errorf("password should be percent-escaped in URL, got %q", got)
	}

	// Sanity: still starts with the right scheme + user prefix.
	if !strings.HasPrefix(got, "redis://default:") {
		t.Errorf("URL should start with redis://default:, got %q", got)
	}

	// And the host:port arrives unscathed.
	if !strings.HasSuffix(got, "@redis.clowk-lp.voodu:6379") {
		t.Errorf("URL should end with the right host:port, got %q", got)
	}
}
