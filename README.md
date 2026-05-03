# voodu-redis

Voodu macro plugin that expands a `redis { … }` HCL block into a **fan-out**: an `asset` carrying a production-ready `redis.conf` plus a `statefulset` that bind-mounts the config at `/etc/redis/redis.conf` and runs `redis-server` against it.

Bare block produces a hardened, single-node redis without the operator writing any config.

## Quick start

```hcl
redis "data" "cache" {}
```

That's it. Plugin emits:

1. **`asset "data" "cache"`** — file `redis_conf` (the production redis.conf shipped by this plugin)
2. **`statefulset "data" "cache"`** — image `redis:7-alpine`, port `6379`, volume_claim `data` at `/data`, command `["redis-server", "/etc/redis/redis.conf"]`, volumes mounting `${asset.cache.redis_conf}` at `/etc/redis/redis.conf:ro`

Apply:

```bash
vd apply -f voodu.hcl
```

Connect from another app:

```bash
vd config set -s myapp REDIS_URL="redis://cache-0.data:6379/0"
```

## What's in the production redis.conf

Inspect the exact bytes the plugin will mount:

```bash
voodu-redis get-conf
```

Highlights:

- **RDB-only persistence** — `save 900 1 / 300 10 / 60 10000` thresholds, AOF disabled (see "Why no AOF" below). Up to ~60s of writes lost on crash.
- **`maxclients 10000`** — generous connection pool
- **`tcp-keepalive 60`** — drops dead connections quickly
- **`timeout 0`** — never close idle clients (override if needed)
- **`protected-mode yes`** — refuses unauth'd external connections
- **`maxmemory-policy noeviction`** — safe default; pure cache use cases override (see below)

**Why no AOF (since v0.13.0):** AOF takes precedence over RDB on Redis boot. With AOF on, `vd redis:restore` silently fails — the restored `dump.rdb` is ignored because Redis loads the AOF (which still has the operator's pre-restore writes). RDB-only persistence keeps backup/restore semantics simple: `dump.rdb` IS the truth.

If your workload needs hard durability (financial state, no-loss queues), override via `/etc/redis/conf.d/*.conf`:

```conf
appendonly yes
appendfsync everysec
```

But know that re-enabling AOF means future restores need manual AOF wipe — see "Restore" section below.

The conf is committed in this repo at `conf/redis.conf` — read it once, you'll know everything that runs.

## Customising

### Level 1 — bare defaults

```hcl
redis "data" "cache" {}
```

Hardened single-node redis. Done.

### Level 2 — override statefulset attrs

Anything declared on the redis block follows the alias contract: operator-wins shallow merge over plugin defaults.

```hcl
redis "data" "cache" {
  image    = "redis:8"           # operator override
  replicas = 1
  ports    = ["10.0.0.5:6379:6379"]   # bind on private interface
}
```

The redis.conf the plugin ships is still mounted; everything else is the operator's.

### Level 3 — edit the plugin's redis.conf in place

```bash
ssh server
sudo $EDITOR /opt/voodu/plugins/redis/conf/redis.conf
```

Next `vd apply` re-reads the file. The asset hash in the statefulset spec hash changes → rolling restart picks up new content automatically. **No rebuild, no redeploy of the plugin binary.**

### Level 4 — substitute `bin/get-conf` for a generator

The plugin invokes `bin/get-conf` at expand time and uses whatever stdout you emit. Replace the script for templating, env interpolation, anything dynamic:

```bash
#!/usr/bin/env bash
# /opt/voodu/plugins/redis/bin/get-conf
cat <<EOF
bind 0.0.0.0
port 6379
appendonly yes
maxmemory ${REDIS_MAXMEMORY:-512mb}
maxmemory-policy ${REDIS_POLICY:-noeviction}
EOF
```

`expand` calls this; whatever it prints becomes the redis.conf inside the container.

### Level 5 — declare your own asset, override volumes

For complex configs (multiple files, custom users, TLS certs), declare your asset block standalone and bypass the plugin's:

```hcl
asset "data" "redis-prod" {
  configuration = file("./redis-prod.conf")
  custom_users  = file("./conf/users.conf")
  ca_pem        = url("https://r2.example.com/redis-ca.pem")
}

redis "data" "cache" {
  command = ["redis-server", "/etc/redis/redis.conf"]
  volumes = [
    "${asset.redis-prod.configuration}:/etc/redis/redis.conf:ro",
    "${asset.redis-prod.custom_users}:/etc/redis/conf.d/users.conf:ro",
    "${asset.redis-prod.ca_pem}:/etc/redis/ca.pem:ro",
  ]
}
```

Operator volumes win; plugin's default asset (`asset/data/cache`) is still emitted but unmounted (~1KB orphan in `/opt/voodu/assets/data/cache/`). Acceptable for the flexibility.

### ⚠️ Custom users / ACLs

**Do NOT use Redis's `aclfile` directive on a voodu-redis-managed instance.** It silently overrides the plugin's `requirepass`, which either opens the cluster (no default user in the file) or breaks replication (default user with a different password). Use inline `user` directives at `/etc/redis/conf.d/<anything>.conf` instead — the plugin's password stays authoritative for the `default` user, replication keeps working, and `vd redis:new-password` stays automatic.

Full example with explanation: [`examples/custom-acls/`](examples/custom-acls/).

## High availability — Sentinel quorum

For automatic failover with quorum-based promotion, declare a **separate redis resource** with a `sentinel` block that watches a peer data redis:

```hcl
redis "scope" "redis" {
  replicas = 3
}

redis "scope" "redis-ha" {
  sentinel {
    monitor = "scope/redis"
  }
}
```

Minimal block — `monitor` is the only required field. `enabled = true` is implicit (block presence IS the intent), `replicas` defaults to 3 (HA minimum), quorum derives automatically.

Two-resource design — same plugin, different mode. Adding HA = applying one new resource. Removing HA = `vd apply --prune` the sentinel resource. No tear-down coordination, no churn on the existing data redis.

Quorum auto-derives from `replicas`: `(replicas / 2) + 1`. `replicas = 2` is rejected at apply (quorum math hostile); use 1 (observer-only) or ≥ 3 (HA). Default = 3.

Override sentinel directives (down-after-milliseconds, failover-timeout, etc.) by mounting extra `.conf` files at `/etc/sentinel/conf.d/`. Same pattern as ACL overrides for data redis at `/etc/redis/conf.d/`. The generated bootstrap ends with `include /etc/sentinel/conf.d/*.conf`.

Linking apps:

- `vd redis:link <provider-scope/redis-quorum> <consumer>` — emits `REDIS_URL` pointing at the current data master, refreshed via the failover hook on auto-failover.
- `vd redis:link --sentinel <provider-scope/redis-quorum> <consumer>` — also emits `REDIS_SENTINEL_HOSTS` + `REDIS_MASTER_NAME` for sentinel-aware clients (ioredis Sentinel, redis-py Sentinel, redis-rb sentinels, lettuce). Clients discover the master at runtime.

Manual failover (`vd redis:failover <ref> --replica <N>`) keeps working alongside sentinel — useful as an operator escape hatch. Pass `--no-restart` when you've already moved roles via redis-cli (incident recovery) and just want voodu's store to catch up.

Full pattern with troubleshooting and migration paths: [`examples/sentinel-ha/`](examples/sentinel-ha/).

## Real-world examples

### `redis.voodu` — data redis with custom ACLs from a remote source

```hcl
# Asset that fetches users.acl from an external endpoint at apply time.
# Useful when ACLs are managed centrally (CI artifact, secrets vault).
asset "clowk-lp" "externals" {
  acls = url("http://acl-source.internal/users.acl", {
    timeout    = "10s"
    on_failure = "error"
  })
}

# Data redis — 3-node replication. The default user's password is
# auto-generated and persisted by the plugin (REDIS_PASSWORD in the
# config bucket). The mounted users.conf adds custom users on top
# without breaking the default user — see examples/custom-acls.
redis "clowk-lp" "redis" {
  image    = "redis:8"
  replicas = 3

  plugin { version = "latest" }

  volumes = [
    "${asset.clowk-lp.externals.acls}:/etc/redis/conf.d/users.conf:ro"
  ]
}
```

### `redis-ha.voodu` — sentinel quorum monitoring the data redis

```hcl
# 3-pod sentinel quorum. enabled=true is implicit when the block
# is present; replicas defaults to 3 (HA minimum); quorum derives
# automatically as (replicas/2)+1 = 2.
#
# REDIS_PASSWORD + REDIS_MASTER_ORDINAL flow in via env_from from
# the monitored data redis bucket — no manual env wiring.
redis "clowk-lp" "redis-ha" {
  image = "redis:8"

  plugin { version = "latest" }

  sentinel {
    monitor = "clowk-lp/redis"
  }
}
```

Apply both:

```bash
vd apply -f redis.voodu
vd apply -f redis-ha.voodu
```

### Linking apps

```bash
# Single-pod or read-anywhere consumer — emits REDIS_URL pointing
# at the current master. App auto-restarts on failover.
vd redis:link clowk-lp/redis-ha clowk-lp/web

# Read-heavy consumer — adds REDIS_READ_URL on the round-robin
# replica pool, keeps REDIS_URL on the master.
vd redis:link --reads clowk-lp/redis-ha clowk-lp/dashboard

# Sentinel-aware client (ioredis with Sentinel(), redis-py Sentinel,
# redis-rb sentinels, lettuce). Adds REDIS_SENTINEL_HOSTS and
# REDIS_MASTER_NAME so the client discovers master at runtime —
# survives failover without env-driven restart.
vd redis:link --sentinel clowk-lp/redis-ha clowk-lp/api
```

Unlink (drop the consumer's REDIS_URL/READ_URL/SENTINEL_HOSTS):

```bash
vd redis:unlink clowk-lp/redis-ha clowk-lp/web
```

### Manual failover

The auto-failover hook handles common cases. For operator-driven flips:

```bash
# Promote replica ordinal 1 to master. Triggers rolling restart of
# data redis pods so each re-reads its role at boot. Linked consumer
# URLs auto-refresh.
vd redis:failover clowk-lp/redis --replica 1

# Skip the rolling restart. Use when sentinel auto-failover already
# happened and you just need voodu's store to catch up — or when
# you've manually rearranged roles via redis-cli during incident
# recovery.
vd redis:failover clowk-lp/redis --replica 1 --no-restart
```

### Bun + ioredis with sentinel discovery

`bun add ioredis`

```ts
import Redis from 'ioredis';

// Env from `vd redis:link --sentinel clowk-lp/redis-ha clowk-lp/api`:
//   REDIS_URL              — current master URL (fallback)
//   REDIS_READ_URL         — round-robin replica pool (if --reads)
//   REDIS_SENTINEL_HOSTS   — comma-separated <host:port> sentinel list
//   REDIS_MASTER_NAME      — "voodu-master"
const sentinels = (process.env.REDIS_SENTINEL_HOSTS ?? '')
  .split(',')
  .filter(Boolean)
  .map((hostPort) => {
    const [host, port] = hostPort.split(':');
    return { host, port: Number(port) };
  });

if (sentinels.length === 0) {
  throw new Error(
    'REDIS_SENTINEL_HOSTS not set — re-link with: vd redis:link --sentinel <provider> <consumer>',
  );
}

// ioredis Sentinel mode wants password as a separate field, not
// inside the URL. Extract from REDIS_URL (the plugin embeds it
// in the URL by default).
const url = new URL(process.env.REDIS_URL!);
const password = url.password ? decodeURIComponent(url.password) : undefined;

// Master connection — reconnects across failover via sentinel pubsub.
export const redis = new Redis({
  sentinels,
  name: process.env.REDIS_MASTER_NAME ?? 'voodu-master',
  password,
  enableReadyCheck: true,
  maxRetriesPerRequest: 3,
});

// Read replica connection (optional) — sentinel-discovered slave,
// useful for read-heavy paths to offload master.
export const redisRead = new Redis({
  sentinels,
  name: process.env.REDIS_MASTER_NAME ?? 'voodu-master',
  password,
  role: 'slave',
  enableReadyCheck: true,
});

redis.on('+switch-master', (info: string) => {
  console.log('[redis] sentinel switched master:', info);
});

redis.on('error', (err: Error) => {
  console.error('[redis] error:', err.message);
});

redis.on('ready', () => {
  console.log('[redis] connected, master discovered via sentinel');
});

// Use it
await redis.set('hello', 'voodu-redis HA');
const value = await redisRead.get('hello'); // reads from replica
console.log('value:', value);
```

**What happens during failover:**

| Path | Behaviour |
|---|---|
| `--sentinel` link (above) | ioredis assina pubsub `+switch-master` no sentinel, reconnecta sozinho. **No app restart.** |
| Plain `--reads` or no flag | Voodu hook updates store → env-change rolling restart of consumer container. **~15-30s blip.** |

For latency-critical apps, use `--sentinel`. For simple workers, plain `REDIS_URL` is fine.

## Backup & restore

Plugin owns the redis-side mechanics; operator owns scheduling and remote storage. Two commands:

```bash
# Dump RDB to a local file
vd redis:backup clowk-lp/redis --destination /var/backups/redis-snapshot.rdb

# Restore RDB into the master (replicas full-SYNC automatically)
vd redis:restore clowk-lp/redis --from /var/backups/redis-snapshot.rdb
```

### Backup

`vd redis:backup` picks the source pod automatically:

- **`replicas > 1`** → highest-ordinal replica (offloads the master)
- **`replicas = 1`** → ordinal 0 (the master)
- **`--source <ordinal>`** → force a specific pod (e.g. `--source 0` to snapshot directly from master)

The destination path is on the controller's host filesystem. Plugin doesn't opine on schedule or remote storage — three patterns the operator can pick:

#### Option A — Linux cron / systemd timer (simplest)

The `vd` CLI is already on the host. crontab calls it directly.

```cron
# /etc/crontab — root or whatever user runs voodu
0 */6 * * * root vd redis:backup clowk-lp/redis --destination /tmp/r.rdb && \
                  aws s3 cp /tmp/r.rdb s3://my-bucket/redis-$(date +\%Y\%m\%d-\%H\%M\%S).rdb && \
                  rm /tmp/r.rdb
```

(Note the escaped `\%` — crontab interprets `%` as newline otherwise.)

systemd timer for better logging + retry semantics:

```ini
# /etc/systemd/system/redis-backup.service
[Unit]
Description=voodu-redis backup to S3

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'vd redis:backup clowk-lp/redis --destination /tmp/r.rdb && aws s3 cp /tmp/r.rdb s3://my-bucket/redis-$(date +%%Y%%m%%d-%%H%%M%%S).rdb && rm /tmp/r.rdb'
```

```ini
# /etc/systemd/system/redis-backup.timer
[Unit]
Description=Run redis-backup every 6h

[Timer]
OnCalendar=*-*-* 00,06,12,18:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
sudo systemctl enable --now redis-backup.timer
sudo systemctl list-timers redis-backup
```

**Pros:** uses the plugin command directly, simplest debug, zero new HCL.
**Cons:** schedule lives outside HCL (not versioned with infra).

#### Option B — voodu `cronjob { }` that bypasses the plugin (HCL-native)

Schedule lives in HCL alongside redis. The cronjob container does the dump + upload directly via `redis-cli` and `aws-cli`, bypassing the plugin command entirely. `env_from` flows `REDIS_PASSWORD` from the data redis bucket — zero credential plumbing.

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "amazon/aws-cli:latest"   # has aws-cli, missing redis-cli (we install at boot)

  # REDIS_PASSWORD flows in from clowk-lp/redis bucket automatically.
  env_from = ["clowk-lp/redis"]

  env = {
    AWS_ACCESS_KEY_ID     = "..."   # set via: vd config set clowk-lp/redis-backup AWS_ACCESS_KEY_ID=...
    AWS_SECRET_ACCESS_KEY = "..."
    AWS_DEFAULT_REGION    = "us-east-1"
    # For Cloudflare R2: also set AWS_ENDPOINT_URL=https://<account>.r2.cloudflarestorage.com
  }

  command = ["sh", "-c", <<-EOT
    set -eu
    yum install -y redis6 -q
    # Stream dump straight from a replica → S3, no local file needed
    redis-cli -h redis-2.clowk-lp.voodu -a "$REDIS_PASSWORD" --no-auth-warning --rdb - | \
      aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb
  EOT
  ]
}
```

For Cloudflare R2, add `--endpoint-url $AWS_ENDPOINT_URL` to the `aws s3 cp`.

To use a smaller image (`alpine` + `apk add`) without yum:

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "alpine:latest"
  env_from = ["clowk-lp/redis"]
  env      = { /* AWS creds + region as above */ }

  command = ["sh", "-c", <<-EOT
    set -eu
    apk add --no-cache redis aws-cli > /dev/null
    redis-cli -h redis-2.clowk-lp.voodu -a "$REDIS_PASSWORD" --no-auth-warning --rdb - | \
      aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb
  EOT
  ]
}
```

For zero-install boot (faster cronjob startup), build a custom image once:

```dockerfile
# Dockerfile — build once, push to your registry
FROM alpine:latest
RUN apk add --no-cache redis aws-cli
```

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "registry.example.com/redis-backup:latest"
  env_from = ["clowk-lp/redis"]
  env      = { /* AWS creds */ }

  command = ["sh", "-c",
    "redis-cli -h redis-2.clowk-lp.voodu -a $REDIS_PASSWORD --no-auth-warning --rdb - | aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb"
  ]
}
```

**Pros:** schedule + creds versioned in HCL, env_from is elegant, no host-cron coupling.
**Cons:** doesn't use the plugin's auto source-selection (replica ordinal hardcoded in command), needs custom image for fast startup.

#### Option C — On-demand backup (manual, one-off)

For migrations, snapshot before risky deploys, ad-hoc:

```bash
vd redis:backup clowk-lp/redis --destination /tmp/before-migration.rdb
aws s3 cp /tmp/before-migration.rdb s3://my-bucket/migration-snapshot.rdb
```

The plugin command + manual upload. No schedule needed.

#### Choosing between A and B

| Use | Pick |
|---|---|
| Single-VM, operator OK with crontab/systemd | **A** — simplest, uses our plugin directly |
| Multi-tenant or want infra 100% in HCL | **B** — declarative, env_from handles credentials cleanly |
| Multi-VM with controller on one node only | **A** — `vd` CLI must run on the controller host |

### Restore

`vd redis:restore` swaps the master's `dump.rdb` and restarts. Three lines:

1. `docker stop` the master pod (graceful, 30s timeout — Redis flushes pending state)
2. `docker cp` the local RDB into `<master>:/data/dump.rdb`
3. `docker start` the master — Redis loads the new dump.rdb

Replicas detect the divergent replication ID after master restarts and perform full SYNC from the restored state. No replica orchestration needed.

Master is unavailable for writes during the ~3-5s restart. Replicas serve stale reads until full SYNC completes.

**Important — only works with default AOF-disabled config.** If you re-enable AOF via conf override (`appendonly yes`), Redis on boot loads the AOF (with all the operator's pre-restore writes) and ignores the freshly-imported `dump.rdb`. Restore appears to do nothing.

If you must run with AOF enabled, do the manual restore that wipes AOF:

```bash
PWD=$(vd config get clowk-lp/redis REDIS_PASSWORD)
ORD=$(vd config get clowk-lp/redis REDIS_MASTER_ORDINAL)
[ -z "$ORD" ] && ORD=0

docker stop -t 30 clowk-lp-redis.$ORD

# Wipe ALL AOF state + replace dump.rdb in one ephemeral container
docker run --rm \
  --volumes-from clowk-lp-redis.$ORD \
  -v /tmp:/backup:ro \
  busybox sh -c "
    rm -rf /data/appendonlydir /data/appendonly.aof
    cp /backup/snap.rdb /data/dump.rdb
    chown -R 999:999 /data
  "

docker start clowk-lp-redis.$ORD
```

**Restore is REFUSED when a sentinel resource is watching this redis** (convention probe for `<name>-ha`, `<name>-sentinel`, `<name>-quorum`). Sentinel would interpret the master restart as a failure and trigger a spurious failover to a stale replica. Stop the sentinel temporarily first:

```bash
vd stop clowk-lp/redis-ha
vd redis:restore clowk-lp/redis --from /var/backups/snap.rdb
vd start clowk-lp/redis-ha   # sentinel re-discovers the new master state
```

Sentinel-aware restore (auto-coordinate sentinel pause/resume) is a future feature.

## Plugin contract

```bash
# expand — invoked by the controller during `vd apply`
echo '{"kind":"redis","scope":"data","name":"cache"}' | voodu-redis expand
# → { "status": "ok", "data": [ <asset>, <statefulset> ] }

# get-conf — print the production redis.conf
voodu-redis get-conf

# version
voodu-redis --version
```

## Install

JIT-installed by `vd apply` on first apply containing a `redis { … }` block. Pin manually:

```bash
vd plugins:install redis --repo thadeu/voodu-redis
```

## Storage

Each ordinal of the underlying statefulset gets a docker volume `voodu-<scope>-<name>-data-<ordinal>`. AOF + RDB persist across restarts, image bumps, and rolling restarts. Wipe explicitly:

```bash
vd delete statefulset/<scope>/<name> --prune
```

## Repo layout

```
voodu-redis/
├── plugin.yml                 # 3 commands: expand, get-conf, --version
├── cmd/voodu-redis/main.go    # Go: expand
├── bin/
│   ├── expand                 # bash wrapper → voodu-redis binary
│   ├── get-conf               # bash: cat ../conf/redis.conf
│   └── voodu-redis            # built binary (CI release / make build)
├── conf/
│   └── redis.conf             # production-ready, edit-in-place customisation
├── install / uninstall        # lifecycle hooks
├── Makefile                   # build / cross / install-local
└── .github/workflows/release.yml
```

## Development

```bash
make build
make cross
make test

# direct invocations
echo '{"kind":"redis","name":"cache"}' | bin/voodu-redis expand
bin/get-conf
```

## License

MIT.
