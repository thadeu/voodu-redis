# Sentinel HA — automatic failover for managed redis

This example shows the two-resource pattern voodu-redis uses to
add Redis Sentinel quorum on top of a regular replicated redis.
One resource for the data redis, one for the sentinels — they
share a scope and reference each other through the `monitor` field.

## TL;DR

```hcl
redis "clowk-lp" "redis" {
  replicas = 3
}

redis "clowk-lp" "redis-ha" {
  sentinel {
    monitor = "clowk-lp/redis"
  }
}
```

`vd apply` brings up 6 pods (3 redis + 3 sentinel). Failover is
automatic when the master goes down for >5s, with quorum=2.

The minimal sentinel block has just one required field — `monitor`.
`enabled = true` is implicit (block presence IS intent), `replicas`
defaults to 3 (HA minimum), quorum derives automatically.

## Why two resources

The original design considered expanding a single
`redis { sentinel { ... } }` block into two statefulsets under
the hood. We rejected that for three reasons:

- **Backward compat**: an existing `redis { }` resource without
  the `sentinel` block stays exactly as it was. No silent
  re-emission, no lifecycle change.
- **Lifecycle isolation**: adding HA = applying one new
  resource. Removing HA = `vd apply --prune` the sentinel
  resource. No tear-down coordination.
- **Visibility**: `vd ps` shows both statefulsets explicitly.
  Operators see what they own.

## Quorum

The quorum count derives from `replicas` automatically:
`(replicas / 2) + 1`. The HCL surface deliberately omits a
`quorum` field — the operator can't get math wrong.

| `replicas` | quorum | survives | notes |
|-----------:|-------:|---------:|-------|
| 1 | 1 | 0 outages | observer-only, NOT HA — accepted for prototyping |
| 2 | 2 | 0 outages | **rejected at apply** — strictly worse than 1 sentinel |
| 3 | 2 | 1 outage  | HA minimum |
| 5 | 3 | 2 outages | for cross-AZ deployments |

Always use an odd `replicas` when you care about HA.

## Overriding sentinel directives

The generated `/etc/sentinel/sentinel.conf` ends with:

```
include /etc/sentinel/conf.d/*.conf
```

Mount any extra `.conf` files via the HCL `volumes` block to
override defaults without editing the plugin's bootstrap. Same
pattern as `/etc/redis/conf.d/users.conf` for ACLs on the data
redis side.

```hcl
asset "clowk-lp" "redis-ha-overrides" {
  defs = file("./conf/sentinel-overrides.conf")
}

redis "clowk-lp" "redis-ha" {
  sentinel { monitor = "clowk-lp/redis" }

  volumes = [
    "${asset.clowk-lp.redis-ha-overrides.defs}:/etc/sentinel/conf.d/overrides.conf:ro",
  ]
}
```

With `./conf/sentinel-overrides.conf`:

```
# Tighten failover detection (default 5000ms)
sentinel down-after-milliseconds voodu-master 2000

# Shorter failover ceiling (default 60000ms)
sentinel failover-timeout voodu-master 30000

# Drain replicas one at a time during sync (default 1)
sentinel parallel-syncs voodu-master 1
```

Sentinel rewrites the MAIN sentinel.conf at runtime to record
observed topology, but the include is re-evaluated on every
restart — overrides survive sentinel's own conf rewrites.

## Linking apps

Two flavours, depending on whether your client speaks Sentinel:

### Without sentinel-aware client

```sh
vd redis:link clowk-lp/redis-quorum clowk-lp/web
```

Emits a single `REDIS_URL` pointing at the current data master.
Linked through the sentinel resource (which knows how to find
the master via the monitor field), but the consumer just sees a
plain URL.

When sentinel auto-failovers, the failover hook updates voodu's
store, which re-emits all linked consumers' URLs and restarts
their containers. Brief unavailability during the restart is
the cost of not having a sentinel-aware client.

### With sentinel-aware client

```sh
vd redis:link --sentinel clowk-lp/redis-quorum clowk-lp/web
```

Emits:

- `REDIS_URL` — current master, same as above (for fallback)
- `REDIS_SENTINEL_HOSTS` — `redis-quorum-0.clowk-lp.voodu:26379,redis-quorum-1.clowk-lp.voodu:26379,redis-quorum-2.clowk-lp.voodu:26379`
- `REDIS_MASTER_NAME` — `voodu-master`

Sentinel-aware clients (ioredis with `Sentinel(...)`,
redis-py `Sentinel(...)`, redis-rb `sentinels: [...]`, lettuce)
read `REDIS_SENTINEL_HOSTS`/`REDIS_MASTER_NAME` and discover
the master at runtime. Failover doesn't trigger an env-driven
restart — clients re-discover within seconds.

For read-heavy workloads, add `--reads`:

```sh
vd redis:link --reads --sentinel clowk-lp/redis-quorum clowk-lp/dashboard
```

## Manual failover with sentinel active

`vd redis:failover` keeps working — operator's escape hatch
when they need to force a specific ordinal:

```sh
vd redis:failover clowk-lp/redis --replica 2
```

Sentinel detects the new role via `INFO replication` and respects
it (no ping-pong). The classic flow rolls the redis pods top-down
so each pod re-reads `REDIS_MASTER_ORDINAL`.

If you've already moved roles inside Redis manually (incident
recovery via redis-cli), pass `--no-restart` to update voodu
store WITHOUT touching the running pods:

```sh
vd redis:failover clowk-lp/redis --replica 2 --no-restart
```

This is also the path the sentinel auto-failover hook takes
internally.

## VOODU_CONTROLLER_URL

The sentinel pod's failover hook calls back into the voodu
controller to update the store after auto-failover. The hook
needs `VOODU_CONTROLLER_URL` set in the sentinel container's
env — wire it via the HCL `env = {}` block on the sentinel
resource:

```hcl
redis "clowk-lp" "redis-quorum" {
  replicas = 3
  sentinel { enabled = true, monitor = "clowk-lp/redis" }

  env = {
    VOODU_CONTROLLER_URL = "http://host.docker.internal:8080"
  }
}
```

If unset:

- Sentinel still failovers correctly inside Redis (sentinel is
  self-contained on that path).
- The voodu store stays stale until the operator runs a manual
  `vd redis:failover --replica <new-ordinal>`.
- Apps using `REDIS_URL` (no `--sentinel`) keep talking to the
  old master FQDN, which now resolves to a replica → connection
  errors → manual fix needed.
- Apps using `--sentinel` are unaffected (they discover via
  sentinel directly).

So: **set VOODU_CONTROLLER_URL on the sentinel resource if you
want auto-failover to be transparent to apps using plain
`REDIS_URL`**.

## Migration paths

### Adding sentinel to an existing redis

1. Edit your HCL to add the new sentinel resource (don't touch
   the existing `redis { }` block).
2. `vd apply` — brings up the 3 sentinel pods. The data redis
   is untouched (zero churn).
3. (Optional) Re-link consumers with `--sentinel` to switch to
   sentinel-aware discovery.

The data redis's volume, password, and master ordinal are all
preserved. No data migration.

### Removing sentinel

1. Delete the sentinel resource block from HCL.
2. `vd apply --prune` — sentinel pods are removed. Data redis
   stays running.
3. Future failovers go back to manual via
   `vd redis:failover --replica <N>`.

If you had used `--sentinel` on consumer links, re-run them
without the flag (or `vd redis:unlink` then re-link). The plain
`REDIS_URL` keeps working.

## Failure modes & recovery

### Single master crash (the common case)

The whole reason sentinel exists. Timeline:

| T | Event |
|---|-------|
| 0s | Master pod crashes |
| ~5s | Sentinels mark master `+sdown` (pings fail for `down-after-milliseconds`) |
| ~5s | Quorum (2/3) agrees → `+odown` |
| ~7s | Leader sentinel elected |
| ~10s | Replica selected, promoted (`SLAVEOF NO ONE`) |
| ~12s | Other replicas reconfigured to new master |
| ~13s | `+switch-master` fires, hook posts to voodu controller |
| ~13s | `REDIS_MASTER_ORDINAL` updated in voodu store |
| ~15-30s | Linked consumers' env files re-emitted, containers rolling-restart |
| ~30-60s | Voodu reconciler respawns the dead pod, joins as replica of new master |

**Operator does nothing.** Apps using `--sentinel`-aware client reconnect within seconds; apps using plain `REDIS_URL` see ~15-30s blip from container restart.

### Master + all replicas down (catastrophic, very unlikely)

If somehow every redis pod is dead simultaneously, sentinel can't promote — no candidate. Recovery:

```bash
# 1. Identify any pod that's actually running (or bring one back)
docker ps --filter "name=clowk-lp-redis" --filter "status=running"
# Or force-spawn one via vd
vd start clowk-lp/redis.0

# 2. Tell voodu the survivor is now master
vd redis:failover clowk-lp/redis --replica 0 --no-restart

# 3. Roll the data redis pods so they re-read the new master ordinal
vd restart clowk-lp/redis
```

### Sentinel state divergent from reality (after rapid chained failures)

If you've killed several masters in succession (testing or real cascade failure), sentinel can end up with:
- Wrong "current master" in its memory
- Replicas marked `s_down` permanently
- `flags = master,disconnected` stuck without progressing to `+sdown`
- `vd redis:info` for sentinel showing different ordinal than for data redis

Sentinel's internal state has too much accumulated history. Reset:

```bash
# Force every sentinel to forget its current view and re-discover
for i in 0 1 2; do
  vd exec clowk-lp/redis-ha.$i -- redis-cli -p 26379 SENTINEL RESET voodu-master
done

# Wait 15-30s — sentinels will re-discover via INFO replication and pubsub
sleep 30

# Verify state is healthy
vd exec clowk-lp/redis-ha.0 -- redis-cli -p 26379 SENTINEL master voodu-master | grep -A1 "^flags|^num-slaves|^num-other-sentinels"
```

If still stuck after `SENTINEL RESET`, manual catch-up:

```bash
# Find which redis pod is actually master (or pick any healthy one)
PWD=$(vd config get clowk-lp/redis REDIS_PASSWORD)
for i in 0 1 2; do
  echo "═══ redis-$i ═══"
  vd exec clowk-lp/redis.$i -- redis-cli -a "$PWD" INFO replication 2>/dev/null | grep "^role"
done

# Force voodu store + reconfigure pods
vd redis:failover clowk-lp/redis --replica <healthy-ord> --no-restart
vd restart clowk-lp/redis
```

### Controller restart endpoint hangs

If `vd restart clowk-lp/redis-ha` times out with `context deadline exceeded`, the controller is blocked on a docker operation. Recovery:

```bash
# Force-remove stuck containers
docker rm -f $(docker ps -aq --filter "name=clowk-lp-redis-ha")

# Restart the controller to release internal locks
sudo systemctl restart voodu-controller

# Re-apply to spawn fresh containers
vd apply -f sentinel.hcl
```

This is rare — only happens after invasive manual interventions (chained `docker kill` or `docker rm` while reconciler is mid-flight).

### Why sentinel struggles with rapid failures

Sentinel was designed for **occasional failover** (master crashes once a month/year). Its state machine accumulates:
- `known-replica` for every replica it's ever seen
- `known-sentinel` for every peer
- Epoch bumps on every election
- Cooldowns to prevent flapping

Rapid sequential failures (testing or genuine cascade) overflow these state buffers and Sentinel can lose track. **In production this almost never happens** — failures are sparse enough for sentinel to digest each one cleanly. For chaos testing, expect to use `SENTINEL RESET` or manual `vd redis:failover --no-restart` as recovery levers.

## Same-VM vs multi-VM

This example assumes everything runs on one VM with voodu0 as
the docker bridge. Pods reach each other via `<name>-<ord>.<scope>.voodu`
hostnames, sentinel reaches the data redis the same way.

Multi-VM (sentinels and redis pods on different hosts) requires
voodu's general cross-VM pod-to-pod networking — out of scope
for the F3 milestone. Until that lands, declare `redis-quorum`
to land on the same host as `redis` (single-VM).

## Troubleshooting

### "voodu-sentinel: WARNING — monitor target not found"

The sentinel pod logs this at boot when it can't find the data
redis in the voodu store. Common causes:

- Typo in `monitor = "scope/name"` — the value must exactly match
  the data redis's HCL labels.
- Apply order — the sentinel pod started before the data redis
  was applied. Re-apply or wait; sentinel will retry.
- Cross-scope reference — not supported in this milestone. Both
  resources must share a scope.

### Sentinel never promotes a replica

Check sentinel logs for `+sdown` (subjectively down) events. If
sentinels see the master as down but the quorum doesn't agree
(`+odown` never fires), you're below quorum. Verify with:

```sh
vd logs clowk-lp/redis-quorum
```

If you see `Can't connect to master` continuously and the data
redis pods are healthy, sentinel might be dialing the wrong
host — check the `monitor` field and the FQDN it computes
(visible in the boot log line: `voodu-sentinel: monitoring
... master=<host>:6379 quorum=<n>`).

### Auto-failover happens but consumer URLs go stale

The failover hook couldn't reach the controller. Verify:

```sh
vd logs clowk-lp/redis-quorum | grep voodu-sentinel-hook
```

Look for `gave up after 5 attempts` — that's the hook telling
you the callback failed. Common cause: `VOODU_CONTROLLER_URL`
isn't set or points at an unreachable address from inside the
sentinel container. Fix the env, re-apply, and run a manual
`vd redis:failover --replica <new-ordinal>` to catch the store up.
