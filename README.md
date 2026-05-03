# voodu-redis

Voodu macro plugin that expands a `redis { … }` HCL block into a **fan-out**: an `asset` carrying a production-ready `redis.conf` plus a `statefulset` that bind-mounts the config at `/etc/redis/redis.conf` and runs `redis-server` against it.

Bare block produces a hardened, single-node redis without the operator writing any config. Same plugin, with a `sentinel { }` block, runs Redis Sentinel for HA. With backup/restore commands for snapshot-based disaster recovery.

## Table of contents

- [Quick start](#quick-start)
- [Configuration](#configuration)
  - [Production defaults](#production-defaults)
  - [Persistence model — RDB only](#persistence-model--rdb-only)
  - [Customising — 5 levels](#customising--5-levels)
  - [Custom users / ACLs](#custom-users--acls)
- [High availability — Sentinel quorum](#high-availability--sentinel-quorum)
- [Backup & restore](#backup--restore)
  - [Commands](#commands)
  - [Restore caveats](#restore-caveats)
  - [Automation patterns](#automation-patterns)
- [Real-world examples](#real-world-examples)
  - [Data redis with custom ACLs](#data-redis-with-custom-acls)
  - [Sentinel HA quorum](#sentinel-ha-quorum)
  - [Linking apps](#linking-apps)
  - [Manual failover](#manual-failover)
  - [Bun + ioredis with sentinel discovery](#bun--ioredis-with-sentinel-discovery)
- [Plugin reference](#plugin-reference)
  - [Commands](#commands-1)
  - [Plugin contract](#plugin-contract)
  - [Repo layout](#repo-layout)
  - [Development](#development)
- [Install & upgrade](#install--upgrade)
- [Storage](#storage)
- [License](#license)

---

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
vd config myapp set REDIS_URL="redis://cache-0.data:6379/0"
```

---

## Configuration

### Production defaults

Inspect the exact bytes the plugin will mount:

```bash
voodu-redis get-conf
```

Highlights:

- **RDB-only persistence** — `save 900 1 / 300 10 / 60 10000` thresholds, AOF disabled. Up to ~60s of writes lost on crash.
- **`maxclients 10000`** — generous connection pool
- **`tcp-keepalive 60`** — drops dead connections quickly
- **`timeout 0`** — never close idle clients (override if needed)
- **`protected-mode yes`** — refuses unauth'd external connections
- **`maxmemory-policy noeviction`** — safe default; pure cache use cases override

The conf is committed in this repo at [`conf/redis.conf`](conf/redis.conf) — read it once, you'll know everything that runs.

### Persistence model — RDB only

Since `v0.13.0` the plugin ships with **AOF disabled and RDB-only persistence**. Why:

AOF takes precedence over RDB on Redis boot. With AOF on, `vd redis:restore` silently fails — the restored `dump.rdb` is ignored because Redis loads the AOF (which still has the operator's pre-restore writes). RDB-only persistence keeps backup/restore semantics simple: `dump.rdb` IS the truth.

**Trade-off:** up to ~60s of write loss on crash (per the tightest `save N M` threshold).

If your workload needs hard durability (financial state, no-loss queues), override via `/etc/redis/conf.d/*.conf`:

```conf
appendonly yes
appendfsync everysec
```

But know that re-enabling AOF means future `vd redis:restore` calls need manual AOF wipe — see [Restore caveats](#restore-caveats).

### Customising — 5 levels

#### Level 1 — bare defaults

```hcl
redis "data" "cache" {}
```

Hardened single-node redis. Done.

#### Level 2 — override statefulset attrs

Anything declared on the redis block follows the alias contract: operator-wins shallow merge over plugin defaults.

```hcl
redis "data" "cache" {
  image    = "redis:8"                  # operator override
  replicas = 1
  ports    = ["10.0.0.5:6379:6379"]     # bind on private interface
}
```

The redis.conf the plugin ships is still mounted; everything else is the operator's.

#### Level 3 — edit the plugin's redis.conf in place

```bash
ssh server
sudo $EDITOR /opt/voodu/plugins/redis/conf/redis.conf
```

Next `vd apply` re-reads the file. The asset hash in the statefulset spec hash changes → rolling restart picks up new content automatically. **No rebuild, no redeploy of the plugin binary.**

#### Level 4 — substitute `bin/get-conf` for a generator

The plugin invokes `bin/get-conf` at expand time and uses whatever stdout you emit. Replace the script for templating, env interpolation, anything dynamic:

```bash
#!/usr/bin/env bash
# /opt/voodu/plugins/redis/bin/get-conf
cat <<EOF
bind 0.0.0.0
port 6379
maxmemory ${REDIS_MAXMEMORY:-512mb}
maxmemory-policy ${REDIS_POLICY:-noeviction}
EOF
```

`expand` calls this; whatever it prints becomes the redis.conf inside the container.

#### Level 5 — declare your own asset, override volumes

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

### Custom users / ACLs

> ⚠️ **Do NOT use Redis's `aclfile` directive on a voodu-redis-managed instance.**
>
> It silently overrides the plugin's `requirepass`, which either opens the cluster (no default user in the file) or breaks replication (default user with a different password). Use inline `user` directives at `/etc/redis/conf.d/<anything>.conf` instead — the plugin's password stays authoritative for the `default` user, replication keeps working, and `vd redis:new-password` stays automatic.

Full example with explanation: [`examples/custom-acls/`](examples/custom-acls/).

---

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

**Two-resource design** — same plugin, different mode. Adding HA = applying one new resource. Removing HA = `vd apply --prune` the sentinel resource. No tear-down coordination, no churn on the existing data redis.

**Quorum** auto-derives from `replicas`: `(replicas / 2) + 1`. `replicas = 2` is rejected at apply (quorum math hostile); use 1 (observer-only) or ≥ 3 (HA). Default = 3.

**Override sentinel directives** (down-after-milliseconds, failover-timeout, etc.) by mounting extra `.conf` files at `/etc/sentinel/conf.d/`. Same pattern as ACL overrides for data redis at `/etc/redis/conf.d/`. The generated bootstrap ends with `include /etc/sentinel/conf.d/*.conf`.

Full pattern with troubleshooting and migration paths: [`examples/sentinel-ha/`](examples/sentinel-ha/).

---

## Backup & restore

Plugin owns the redis-side mechanics; operator owns scheduling and remote storage.

### Commands

```bash
# Dump RDB to a local file
vd redis:backup clowk-lp/redis --destination /var/backups/redis-snapshot.rdb

# Restore RDB into the master (replicas full-SYNC automatically)
vd redis:restore clowk-lp/redis --from /var/backups/redis-snapshot.rdb
```

**Backup source picks the source pod automatically:**

- **`replicas > 1`** → highest-ordinal replica (offloads the master)
- **`replicas = 1`** → ordinal 0 (the master)
- **`--source <ordinal>`** → force a specific pod (e.g. `--source 0` to snapshot directly from master)

**Restore is three docker calls:**

1. `docker stop` the master pod (graceful, 30s timeout — Redis flushes pending state)
2. `docker cp` the local RDB into `<master>:/data/dump.rdb`
3. `docker start` the master — Redis loads the new dump.rdb

Replicas detect the divergent replication ID after master restarts and perform full SYNC from the restored state. No replica orchestration needed.

Master is unavailable for writes during the ~3-5s restart. Replicas serve stale reads until full SYNC completes.

### Restore caveats

**Sentinel watching this redis → restore REFUSED.** Convention probe for `<name>-ha`, `<name>-sentinel`, `<name>-quorum`. Sentinel would interpret the master restart as a failure and trigger a spurious failover to a stale replica. Stop the sentinel temporarily first:

```bash
vd stop clowk-lp/redis-ha
vd redis:restore clowk-lp/redis --from /var/backups/snap.rdb
vd start clowk-lp/redis-ha   # sentinel re-discovers the new master state
```

Sentinel-aware restore (auto-coordinate sentinel pause/resume) is a future feature.

**AOF re-enabled (operator override) → restore appears to do nothing.** Redis on boot loads the AOF (with all the operator's pre-restore writes) and ignores the freshly-imported `dump.rdb`. Manual restore that wipes AOF:

```bash
PWD=$(vd config clowk-lp/redis get REDIS_PASSWORD -o json | jq -r .REDIS_PASSWORD)
ORD=$(vd config clowk-lp/redis get REDIS_MASTER_ORDINAL -o json | jq -r '.REDIS_MASTER_ORDINAL // "0"')

docker stop -t 30 clowk-lp-redis.$ORD

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

### Automation patterns

The plugin doesn't opine on schedule or remote storage — three patterns the operator can pick:

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

#### Option B — voodu `cronjob { }` declarative (HCL-native)

Schedule lives in HCL alongside redis. The cronjob container does the dump + upload directly via `redis-cli` and `aws-cli`, bypassing the plugin command. Credentials flow via `env_from` — pair the cronjob with both the redis bucket (for `REDIS_PASSWORD`) AND a shared `aws/cli` bucket (for AWS keys + region).

**Pre-step — declare a shared AWS credentials bucket once:**

```bash
vd config aws/cli set AWS_ACCESS_KEY_ID=AKIAxxx
vd config aws/cli set AWS_SECRET_ACCESS_KEY=secretxxx
vd config aws/cli set AWS_DEFAULT_REGION=us-east-1
```

Now any cronjob/job that needs AWS does `env_from = ["aws/cli"]` — no credential duplication across HCL files.

**Cronjob using `amazon/aws-cli` image:**

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "amazon/aws-cli:latest"   # has aws-cli, missing redis-cli (install at boot)

  # REDIS_PASSWORD ← clowk-lp/redis bucket
  # AWS_*          ← aws/cli bucket
  env_from = ["clowk-lp/redis", "aws/cli"]

  env = {
    # R2 endpoint override (omit for AWS S3)
    AWS_ENDPOINT_URL = "https://<account>.r2.cloudflarestorage.com"
  }

  command = ["sh", "-c", <<-EOT
    set -eu
    yum install -y redis6 -q
    # Stream dump from a replica → S3, no local file
    redis-cli -h redis-2.clowk-lp.voodu -a "$REDIS_PASSWORD" --no-auth-warning --rdb - | \
      aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb
  EOT
  ]
}
```

**Smaller image variant (`alpine` + `apk add`):**

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "alpine:latest"
  env_from = ["clowk-lp/redis", "aws/cli"]

  command = ["sh", "-c", <<-EOT
    set -eu
    apk add --no-cache redis aws-cli > /dev/null
    redis-cli -h redis-2.clowk-lp.voodu -a "$REDIS_PASSWORD" --no-auth-warning --rdb - | \
      aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb
  EOT
  ]
}
```

**Pre-built custom image (zero-install boot, fastest):**

```dockerfile
# Dockerfile — build once, push to your registry
FROM alpine:latest
RUN apk add --no-cache redis aws-cli
```

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule = "0 */6 * * *"
  image    = "registry.example.com/redis-backup:latest"
  env_from = ["clowk-lp/redis", "aws/cli"]

  command = ["sh", "-c",
    "redis-cli -h redis-2.clowk-lp.voodu -a $REDIS_PASSWORD --no-auth-warning --rdb - | aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb"
  ]
}
```

**Build inline via voodu's `dockerfile` attribute** (no external registry needed):

```hcl
cronjob "clowk-lp" "redis-backup" {
  schedule   = "0 */6 * * *"

  # Voodu builds the image at apply time from your local Dockerfile
  dockerfile = "Dockerfile.backup"
  path       = "./infra/redis-backup"   # build context

  env_from = ["clowk-lp/redis", "aws/cli"]
  command  = ["sh", "/app/backup.sh"]
}

asset "clowk-lp" "backup-script" {
  entry = file("./infra/redis-backup/backup.sh")
}
```

`Dockerfile.backup`:

```dockerfile
FROM alpine:latest
RUN apk add --no-cache redis aws-cli bash
WORKDIR /app
```

`./infra/redis-backup/backup.sh`:

```bash
#!/bin/sh
set -eu
redis-cli -h redis-2.clowk-lp.voodu -a "$REDIS_PASSWORD" --no-auth-warning --rdb - | \
  aws s3 cp - s3://my-bucket/redis-$(date +%Y%m%d-%H%M%S).rdb
```

**Pros:** schedule + script + credentials all versioned in HCL, env_from is elegant, no host-cron coupling.
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

---

## Real-world examples

### Data redis with custom ACLs

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

### Sentinel HA quorum

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
| `--sentinel` link (above) | ioredis subscribes to `+switch-master` pubsub, reconnects on its own. **No app restart.** |
| Plain `--reads` or no flag | Voodu hook updates store → env-change rolling restart of consumer container. **~15-30s blip.** |

For latency-critical apps, use `--sentinel`. For simple workers, plain `REDIS_URL` is fine.

---

## Plugin reference

### Commands

| Command | Purpose |
|---|---|
| `vd redis:link [--reads] [--sentinel] <provider> <consumer>` | Inject REDIS_URL (and optionally REDIS_READ_URL + REDIS_SENTINEL_HOSTS) into the consumer's config |
| `vd redis:unlink <provider> <consumer>` | Remove previously-injected URLs |
| `vd redis:new-password <ref>` | Rotate the redis password; linked consumer URLs auto-refresh |
| `vd redis:failover <ref> --replica <N> [--no-restart]` | Promote ordinal N to master (manually). `--no-restart` for sentinel-driven scenarios |
| `vd redis:info <ref>` | Show connection info, replication topology, linked consumers |
| `vd redis:backup <ref> --destination <path> [--source <ord>]` | Dump RDB snapshot to a local file |
| `vd redis:restore <ref> --from <path>` | Restore RDB into master (replicas full-SYNC automatically) |

Per-command help:

```bash
vd redis:<command> -h
```

### Plugin contract

```bash
# expand — invoked by the controller during `vd apply`
echo '{"kind":"redis","scope":"data","name":"cache"}' | voodu-redis expand
# → { "status": "ok", "data": { "manifests": [<asset>, <statefulset>], "actions": [...] } }

# get-conf — print the production redis.conf
voodu-redis get-conf

# version
voodu-redis --version
```

### Repo layout

```
voodu-redis/
├── plugin.yml                       # plugin manifest (commands, version, description)
├── cmd/voodu-redis/                 # Go source
│   ├── main.go                      # subcommand dispatcher
│   ├── sentinel.go                  # sentinel HCL parse + validation
│   ├── sentinel_entrypoint.go       # entrypoint script + hook + manifest emission
│   ├── backup.go                    # backup + restore commands
│   ├── failover.go                  # vd redis:failover
│   ├── link.go                      # vd redis:link / unlink
│   ├── password.go                  # vd redis:new-password
│   ├── info.go                      # vd redis:info
│   └── *_test.go                    # parser + behaviour tests
├── bin/                             # bash wrappers + built binary
│   ├── expand, link, failover, ... # one wrapper per command
│   ├── get-conf                     # bash: cat ../conf/redis.conf
│   └── voodu-redis                  # built Go binary (CI release / make build)
├── conf/
│   └── redis.conf                   # production-ready, edit-in-place customisation
├── examples/
│   ├── custom-acls/                 # users.conf override pattern
│   └── sentinel-ha/                 # full HA example with troubleshooting
├── install / uninstall              # lifecycle hooks (download binary on install)
├── Makefile                         # build / cross / install-local
└── .github/workflows/release.yml    # tag-driven binary publishing
```

### Development

```bash
make build
make cross
make test

# Direct invocations (bypassing the controller)
echo '{"kind":"redis","name":"cache"}' | bin/voodu-redis expand
bin/get-conf
bin/voodu-redis --version
```

---

## Install & upgrade

JIT-installed by `vd apply` on first apply containing a `redis { … }` block. Pin manually:

```bash
vd plugins:install thadeu/voodu-redis
```

**Upgrading from `< v0.13.0` to `v0.13.0+`:** AOF default flipped from `yes` to `no`. After upgrade, Redis on next restart ignores existing AOF files in `/data/appendonlydir/`. To avoid losing writes that exist only in AOF (up to ~60s window), force a `BGSAVE` on each pod BEFORE the upgrade:

```bash
PWD=$(vd config clowk-lp/redis get REDIS_PASSWORD -o json | jq -r .REDIS_PASSWORD)
for i in 0 1 2; do
  docker exec clowk-lp-redis.$i redis-cli -a "$PWD" --no-auth-warning BGSAVE
done
sleep 5    # let BGSAVE complete
# Now upgrade plugin + vd apply
```

---

## Storage

Each ordinal of the underlying statefulset gets a docker volume `voodu-<scope>-<name>-data-<ordinal>`. RDB persists across restarts, image bumps, and rolling restarts. Wipe explicitly:

```bash
vd delete statefulset/<scope>/<name> --prune
```

For sentinel resources, the runtime `sentinel.conf` lives in `voodu-<scope>-<name>-state-<ordinal>` so discovered peers + replicas survive pod restarts. Wipe with the same `--prune` flag.

---

## License

MIT.
