// Password generation + insertion helpers shared by `cmdExpand`
// (idempotent reuse / first-apply generation) and
// `cmdNewPassword` (manual rotate).
//
// All randomness comes from crypto/rand — no math/rand fallback.
// Plugin authors who want a deterministic password (e.g. for a
// dev environment) can pre-set REDIS_PASSWORD via
// `vd config set` before the first apply; the plugin reads from
// config first and only generates when the bucket is empty.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// passwordKey is the config bucket key holding the generated
// password. Same name on both sides of the contract: cmdExpand
// writes it, cmdLink reads it, vd config get|set surfaces it.
const passwordKey = "REDIS_PASSWORD"

// passwordEntropyBytes is the number of random bytes used when
// generating a fresh password (hex-encoded → 2x chars on the
// wire). 32 bytes = 256 bits = far more than redis's password
// strength check needs, and the hex form is alphanumeric so it
// round-trips through redis.conf, env files, URLs, and the
// redis-cli prompt without escaping.
const passwordEntropyBytes = 32

// resolveOrGeneratePassword reads REDIS_PASSWORD from the
// controller-supplied config bucket and returns it. When the
// bucket has no password (first apply), generates a fresh one
// from crypto/rand and returns isNew=true so the caller emits
// a config_set action to persist it.
//
// Empty-string passwords in the bucket are treated as absent —
// operators who explicitly want no auth must remove the key
// entirely (`vd config unset REDIS_PASSWORD`), not set it to "".
func resolveOrGeneratePassword(config map[string]string) (password string, isNew bool, err error) {
	if existing, ok := config[passwordKey]; ok && existing != "" {
		return existing, false, nil
	}

	fresh, err := generatePassword()
	if err != nil {
		return "", false, err
	}

	return fresh, true, nil
}

// generatePassword returns a hex-encoded 256-bit random string
// suitable for redis's `requirepass` directive and a redis://
// URL's userinfo. crypto/rand fails closed: any error from the
// reader bubbles up (we don't fall back to a weaker source).
func generatePassword() (string, error) {
	buf := make([]byte, passwordEntropyBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// appendRequirepass inserts `requirepass <password>` AND
// `masterauth <password>` at the END of the redis.conf bytes.
// Redis's directive-parsing rule is "last wins", so even if the
// operator's get-conf script already contained these directives
// with different values, ours override cleanly.
//
// Why both directives:
//
//   - requirepass: clients (and replicas connecting AS clients to
//     the master) must authenticate with this password.
//   - masterauth: the SAME password is used by replicas to dial
//     the master after PSYNC. With requirepass set on the master,
//     a replica without masterauth gets WRONGPASS during the
//     resync handshake and never catches up. Always set both,
//     always to the same value — this is the only configuration
//     that works for the "1 master + N replicas, all on the same
//     password" topology the plugin emits.
//
// We add a leading newline only if the existing bytes don't
// already end with one — covers both `\n`-terminated configs
// (the conventional unix shape) and rare get-conf scripts
// that drop the trailing newline.
//
// A managed-by marker comment goes above the directive so an
// operator inspecting the file knows where it came from
// ("the plugin appended this; don't edit by hand"). Inline
// comments on the directive line itself are NOT used — Redis
// 7+ rejects `requirepass foo # comment` with "Invalid save
// parameters" because it tries to parse the # as part of the
// password.
func appendRequirepass(conf []byte, password string) []byte {
	var b strings.Builder

	b.Grow(len(conf) + len(password)*2 + 128)
	b.Write(conf)

	if len(conf) > 0 && conf[len(conf)-1] != '\n' {
		b.WriteByte('\n')
	}

	b.WriteString("\n# requirepass + masterauth injected by voodu-redis plugin\n")
	b.WriteString("# (do not edit; see `vd redis:new-password`)\n")
	b.WriteString("requirepass ")
	b.WriteString(password)
	b.WriteByte('\n')
	b.WriteString("masterauth ")
	b.WriteString(password)
	b.WriteByte('\n')

	return []byte(b.String())
}
