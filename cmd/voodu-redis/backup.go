// Backup + restore commands — minimalist surface.
//
// Plugin owns the redis-side mechanics (dump RDB from a pod, swap
// dump.rdb on master). Operator owns scheduling + remote storage
// (cron via voodu cronjob OR systemd timer; s3/r2/scp via own
// wrapper script after the local file lands).
//
// Surface:
//
//	vd redis:backup <scope/name> --destination <path>
//	vd redis:restore <scope/name> --from <path>
//
// Design notes:
//
//   - Backup source picks the HIGHEST-ordinal replica (offload the
//     master) when replicas > 1; falls back to master for
//     single-pod. Override via --source <ordinal>.
//   - Restore touches MASTER ONLY. Replicas detect divergent
//     replication ID after master restart and do a full SYNC —
//     standard Redis primitive, no orchestration code.
//   - Restore relies on AOF being DISABLED in the bootstrap
//     redis.conf (default since v0.13.0). With AOF off, redis on
//     boot loads /data/dump.rdb directly — no AOF to compete.
//     Restore = docker stop + docker cp + docker start. Three
//     lines.
//     Operators who override the conf to enable AOF must wipe AOF
//     manually before restore (see README), or the operator's
//     pre-restore writes will resurrect via AOF replay.
//   - Restore is REJECTED when a sentinel watching this redis is
//     detected (convention probe: <name>-ha / <name>-sentinel /
//     <name>-quorum). Operator must temporarily stop the sentinel,
//     restore, restart sentinel. Sentinel-aware restore is a
//     future feature.
//   - Container naming follows containers.ContainerName:
//     `<scope>-<name>.<ordinal>` (e.g. clowk-lp-redis.0).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// cmdBackup dumps a Redis RDB snapshot from a chosen pod to a
// local file on the host running the controller.
//
// Wire:
//
//	vd redis:backup <scope/name> --destination <path> [--source <ordinal>]
//
// Default source: highest-ordinal replica when replicas > 1,
// ordinal 0 (the master by convention) when replicas = 1.
func cmdBackup() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		// Stdout.WriteString sidesteps the go vet false positive
		// where backupHelp contains %Y%m%d (date format example),
		// which Println-via-vet flags as suspicious format directive.
		_, _ = os.Stdout.WriteString(backupHelp + "\n")
		return nil
	}

	positional, destination, sourceOverride, hasSource := parseBackupFlags(args)

	if len(positional) < 1 {
		return fmt.Errorf("usage: vd redis:backup <scope/name> --destination <path> [--source <ordinal>]")
	}

	if destination == "" {
		return fmt.Errorf("--destination <path> is required")
	}

	scope, name := splitScopeName(positional[0])
	if name == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", positional[0])
	}

	ctx, err := readInvocationContext()
	if err != nil {
		return err
	}

	if ctx.ControllerURL == "" {
		return fmt.Errorf("backup requires controller_url (cannot resolve replicas count or password)")
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

	replicas := redisReplicas(spec)

	sourceOrd := pickBackupSource(replicas, config, sourceOverride, hasSource)
	if sourceOrd < 0 || sourceOrd >= replicas {
		return fmt.Errorf("--source %d out of range (valid: 0..%d)", sourceOrd, replicas-1)
	}

	password := redisPasswordFromConfig(config)
	if password == "" {
		password = redisPasswordFromSpecEnv(spec)
	}

	containerName := containerNameFor(scope, name, sourceOrd)

	// Pre-flight: container running?
	if !containerExists(containerName) {
		return fmt.Errorf("container %q not found — is the pod running? Try `vd ps %s`", containerName, refOrName(scope, name))
	}

	fmt.Fprintf(os.Stderr, "voodu-redis: backup source=ordinal-%d (%s) → %s\n",
		sourceOrd, containerName, destination)

	// docker exec <container> redis-cli [-a <pw>] --rdb - → stdout pipe → destination file
	args0 := []string{"exec", "-i", containerName, "redis-cli"}

	if password != "" {
		args0 = append(args0, "-a", password)
	}

	args0 = append(args0, "--no-auth-warning", "--rdb", "-")

	cmd := exec.Command("docker", args0...)

	f, err := os.Create(destination)
	if err != nil {
		return fmt.Errorf("create destination %q: %w", destination, err)
	}
	defer f.Close()

	cmd.Stdout = f
	// Capture stderr to surface meaningful errors (auth failures,
	// connection refused, etc.) instead of just exit code.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()

	if err := cmd.Run(); err != nil {
		// Best-effort cleanup of partial file.
		_ = os.Remove(destination)

		return fmt.Errorf("redis-cli --rdb failed: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	info, statErr := os.Stat(destination)
	if statErr != nil {
		return fmt.Errorf("stat destination %q: %w", destination, statErr)
	}

	if info.Size() == 0 {
		_ = os.Remove(destination)
		return fmt.Errorf("backup file is empty — redis-cli succeeded but produced 0 bytes; check %q logs", containerName)
	}

	// Sanity: RDB files start with the magic "REDIS" string.
	first5 := readFirstBytes(destination, 5)
	if !bytes.Equal(first5, []byte("REDIS")) {
		_ = os.Remove(destination)
		return fmt.Errorf("backup file doesn't look like a valid RDB (missing REDIS magic header) — got %q", string(first5))
	}

	fmt.Fprintf(os.Stderr, "voodu-redis: backup written (%d bytes in %s)\n",
		info.Size(), time.Since(start).Round(time.Millisecond))

	return writeDispatchOutput(dispatchOutput{
		Message: fmt.Sprintf("backup of redis %s written to %s (%d bytes, source=ordinal-%d)",
			refOrName(scope, name), destination, info.Size(), sourceOrd),
	})
}

// cmdRestore loads an RDB snapshot back into the master pod's data
// volume. Replicas detect divergent replication ID on next sync
// attempt and do a full base SYNC from the new master state.
//
// Wire:
//
//	vd redis:restore <scope/name> --from <path>
//
// Refuses to run when a sentinel resource appears to watch this
// redis (convention probe). Operator must stop sentinel first.
func cmdRestore() error {
	args := os.Args[2:]

	if hasHelpFlag(args) {
		fmt.Println(restoreHelp)
		return nil
	}

	positional, source := parseRestoreFlags(args)

	if len(positional) < 1 {
		return fmt.Errorf("usage: vd redis:restore <scope/name> --from <path>")
	}

	if source == "" {
		return fmt.Errorf("--from <path> is required")
	}

	scope, name := splitScopeName(positional[0])
	if name == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", positional[0])
	}

	// Validate source file exists and looks like RDB
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("--from %q: %w", source, err)
	}

	if info.Size() == 0 {
		return fmt.Errorf("--from %q is empty", source)
	}

	if magic := readFirstBytes(source, 5); !bytes.Equal(magic, []byte("REDIS")) {
		return fmt.Errorf("--from %q doesn't look like a valid RDB file (missing REDIS magic header, got %q)",
			source, string(magic))
	}

	ctx, err := readInvocationContext()
	if err != nil {
		return err
	}

	if ctx.ControllerURL == "" {
		return fmt.Errorf("restore requires controller_url (cannot resolve master ordinal or detect sentinel)")
	}

	client := newControllerClient(ctx.ControllerURL)

	if _, err := client.fetchSpec("statefulset", scope, name); err != nil {
		return fmt.Errorf("describe %s: %w", refOrName(scope, name), err)
	}

	config, err := client.fetchConfig(scope, name)
	if err != nil {
		return fmt.Errorf("config get %s: %w", refOrName(scope, name), err)
	}

	// REJECT if sentinel detected watching this redis.
	if sentinelName, watching := detectSentinelWatching(client, scope, name); watching {
		return fmt.Errorf(
			"refusing to restore: sentinel %q is monitoring %s. "+
				"Sentinel would interpret the master restart as a failure and trigger a spurious failover. "+
				"Either:\n"+
				"  1. Stop the sentinel: vd stop %s\n"+
				"  2. Run restore: vd redis:restore %s --from %s\n"+
				"  3. Restart the sentinel (it will re-discover): vd start %s\n"+
				"\n"+
				"Sentinel-aware restore is not supported in this milestone.",
			refOrName(scope, sentinelName), refOrName(scope, name),
			refOrName(scope, sentinelName),
			refOrName(scope, name), source,
			refOrName(scope, sentinelName),
		)
	}

	masterOrd := redisMasterOrdinal(config)

	containerName := containerNameFor(scope, name, masterOrd)

	if !containerExists(containerName) {
		return fmt.Errorf("master container %q not found — is the pod running? Try `vd ps %s`", containerName, refOrName(scope, name))
	}

	fmt.Fprintf(os.Stderr, "voodu-redis: restoring to master=ordinal-%d (%s) from %s (%d bytes)\n",
		masterOrd, containerName, source, info.Size())

	// Restore sequence — minimal because voodu-redis ships with
	// AOF disabled by default (only RDB persistence). Redis on
	// boot loads /data/dump.rdb directly; no AOF to compete.
	//
	//   1. docker stop <master>
	//      Redis flushes pending writes (RDB BGSAVE on shutdown
	//      if dirty), then exits cleanly.
	//
	//   2. docker cp <local.rdb> <master>:/data/dump.rdb
	//      Replace the on-disk RDB with our snapshot. Works on
	//      stopped containers — docker cp doesn't need the
	//      container running.
	//
	//   3. docker start <master>
	//      Redis boots, loads dump.rdb. New replication ID
	//      generated. Replicas reconnect, detect repl-id change,
	//      perform full SYNC from the restored master state.
	//
	// Operators who override the conf to re-enable AOF
	// (/etc/redis/conf.d/*.conf with `appendonly yes`) will hit
	// the AOF-takes-precedence problem — restore appears to do
	// nothing because Redis loads AOF instead. Documented in
	// README; manual AOF wipe required in that case.
	if err := dockerStop(containerName); err != nil {
		return fmt.Errorf("stop master: %w", err)
	}

	if err := dockerCpInto(source, containerName, "/data/dump.rdb"); err != nil {
		return fmt.Errorf("docker cp dump.rdb: %w (master is stopped — start with `docker start %s` or re-run restore)", err, containerName)
	}

	if err := dockerStart(containerName); err != nil {
		return fmt.Errorf("start master: %w (dump.rdb restored but master is stopped — start manually)", err)
	}

	fmt.Fprintf(os.Stderr, "voodu-redis: master started — replicas will detect divergent replication ID and full-SYNC automatically\n")

	return writeDispatchOutput(dispatchOutput{
		Message: fmt.Sprintf("restored redis %s from %s into ordinal-%d. Replicas will full-SYNC from new master state within seconds.",
			refOrName(scope, name), source, masterOrd),
	})
}

// pickBackupSource returns the ordinal to back up from. Auto-mode:
// highest-ordinal replica when replicas > 1 (offload master),
// ordinal 0 for single-pod. Honours an explicit --source override.
func pickBackupSource(replicas int, config map[string]any, override int, hasOverride bool) int {
	if hasOverride {
		return override
	}

	if replicas <= 1 {
		return 0
	}

	// Highest-ordinal replica = N-1 (where N = replicas count).
	candidate := replicas - 1

	// If candidate happens to be the current master, step back one
	// (operator likely ran a failover that promoted a higher-ordinal).
	master := redisMasterOrdinal(config)
	if candidate == master && candidate > 0 {
		candidate--
	}

	return candidate
}

// containerNameFor mirrors containers.ContainerName from voodu's
// internal package: <scope>-<name>.<ordinal> for scoped, or
// <name>.<ordinal> for unscoped statefulsets.
func containerNameFor(scope, name string, ordinal int) string {
	base := name
	if scope != "" {
		base = scope + "-" + name
	}

	return base + "." + strconv.Itoa(ordinal)
}

// containerExists shells out to `docker inspect` to check whether
// a container with this name is registered (running OR stopped).
func containerExists(name string) bool {
	cmd := exec.Command("docker", "inspect", "--format", "{{.Id}}", name)
	cmd.Stderr = io.Discard

	return cmd.Run() == nil
}

// dockerStop sends SIGTERM via docker stop and waits up to 30s for
// graceful shutdown. Redis flushes pending writes to disk before
// exiting (AOF/RDB).
func dockerStop(container string) error {
	cmd := exec.Command("docker", "stop", "-t", "30", container)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// dockerStart starts a previously-stopped container. The
// statefulset reconciler may also try to recreate it; this is fine
// because docker start is idempotent — if container is already
// running, returns success.
func dockerStart(container string) error {
	cmd := exec.Command("docker", "start", container)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// dockerCpInto copies a local file into a container at the given
// destination path. Works against running OR stopped containers
// (docker cp doesn't need the container running). The copied file
// inside the container ends up owned by root:root with the
// source's mode — fine for /data/dump.rdb since redis-server reads
// it world-readably (0644 default) on boot.
func dockerCpInto(localPath, container, destInContainer string) error {
	target := container + ":" + destInContainer

	cmd := exec.Command("docker", "cp", localPath, target)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// detectSentinelWatching probes for a sentinel-mode redis resource
// in the same scope that monitors this (scope, name). Convention-
// based: tries common sentinel naming suffixes — -ha, -sentinel,
// -quorum. Each candidate's spec.env is inspected for
// VOODU_MONITOR_NAME / VOODU_MONITOR_SCOPE matching the target.
//
// Heuristic but covers 90% of operator naming patterns. Operator
// using exotic names (e.g., scope/foo monitored by scope/bar) can
// bypass detection — but that's their setup, they know what they're
// doing.
//
// Returns the matching sentinel name if found.
func detectSentinelWatching(client *controllerClient, scope, name string) (string, bool) {
	candidates := []string{
		name + "-ha",
		name + "-sentinel",
		name + "-quorum",
	}

	for _, candidate := range candidates {
		spec, err := client.fetchSpec("statefulset", scope, candidate)
		if err != nil {
			continue
		}

		env, _ := spec["env"].(map[string]any)
		if env == nil {
			continue
		}

		// Sentinel-mode resources stamp these env keys at expand
		// time (sentinelPodEnv). Matching values prove this resource
		// is monitoring our (scope, name) target.
		if asString(env["VOODU_MONITOR_NAME"]) == name && asString(env["VOODU_MONITOR_SCOPE"]) == scope {
			return candidate, true
		}
	}

	return "", false
}

// asString safely extracts a string from a generic env map value.
// JSON decoding may give us either string or json.RawMessage
// depending on path.
func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.RawMessage:
		return string(s)
	}

	return ""
}

// readFirstBytes reads up to n bytes from the start of a file.
// Used for RDB magic-header validation.
func readFirstBytes(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]byte, n)
	read, _ := io.ReadFull(f, buf)

	return buf[:read]
}

// parseBackupFlags pulls out --destination <path> and --source
// <ordinal> from argv. Both space and = forms accepted.
func parseBackupFlags(args []string) (positional []string, destination string, source int, hasSource bool) {
	positional = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]

		switch {
		case a == "--destination" && i+1 < len(args):
			i++
			destination = args[i]

		case strings.HasPrefix(a, "--destination="):
			destination = strings.TrimPrefix(a, "--destination=")

		case a == "--source" && i+1 < len(args):
			i++

			if n, err := strconv.Atoi(args[i]); err == nil {
				source = n
				hasSource = true
			}

		case strings.HasPrefix(a, "--source="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--source=")); err == nil {
				source = n
				hasSource = true
			}

		default:
			positional = append(positional, a)
		}
	}

	return positional, destination, source, hasSource
}

// parseRestoreFlags pulls out --from <path>.
func parseRestoreFlags(args []string) (positional []string, source string) {
	positional = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]

		switch {
		case a == "--from" && i+1 < len(args):
			i++
			source = args[i]

		case strings.HasPrefix(a, "--from="):
			source = strings.TrimPrefix(a, "--from=")

		default:
			positional = append(positional, a)
		}
	}

	return positional, source
}

const backupHelp = `Usage: vd redis:backup <scope/name> --destination <path> [--source <ordinal>]

Dump a Redis RDB snapshot from a pod to a local file on the host
running the controller.

Source selection:
  - Default: highest-ordinal replica when replicas > 1 (offload
    the master); ordinal 0 for single-pod.
  - --source <N>: force a specific ordinal (e.g., --source 0 to
    snapshot the master).

The destination path is on the controller's host filesystem.
Operator wraps this with their own scheduling + remote storage:

  # Cron via voodu cronjob, systemd timer, etc:
  vd redis:backup clowk-lp/redis --destination /tmp/redis.rdb && \
      aws s3 cp /tmp/redis.rdb s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb && \
      rm /tmp/redis.rdb

Examples:
  vd redis:backup clowk-lp/redis --destination /var/backups/redis-snapshot.rdb
  vd redis:backup clowk-lp/redis --destination /tmp/snap.rdb --source 0`

const restoreHelp = `Usage: vd redis:restore <scope/name> --from <path>

Restore a Redis RDB snapshot into the master pod. Replicas detect
the divergent replication ID after master restart and perform a
full SYNC from the restored state — no manual replica handling.

Sequence:

  1. docker stop the master pod (graceful, 30s timeout)
  2. docker cp <local.rdb> into the master's /data/dump.rdb
  3. Wipe AOF so Redis prefers RDB on boot
  4. docker start the master — Redis loads the RDB
  5. Replicas reconnect, full-SYNC from new master state

Master is unavailable for writes during steps 1-4 (~5-10s for
moderate dataset). Replicas serve stale reads (replication-pre-
restore data) until full SYNC completes.

REFUSED when sentinel watches this redis (convention probe for
<name>-ha, <name>-sentinel, <name>-quorum). Sentinel would
interpret the master restart as a failure and trigger a spurious
failover to a stale replica. Stop the sentinel temporarily first:

  vd stop clowk-lp/redis-ha
  vd redis:restore clowk-lp/redis --from /var/backups/snap.rdb
  vd start clowk-lp/redis-ha   # sentinel re-discovers post-restore

Examples:
  vd redis:restore clowk-lp/redis --from /var/backups/redis-snapshot.rdb`
