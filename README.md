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

- **`appendonly yes`** + **`appendfsync everysec`** — durability across restarts
- **RDB snapshots** at 15min / 5min / 1min thresholds — fast recovery + AOF backup
- **`maxclients 10000`** — generous connection pool
- **`tcp-keepalive 60`** — drops dead connections quickly
- **`timeout 0`** — never close idle clients (override if needed)
- **`protected-mode yes`** — refuses unauth'd external connections
- **`maxmemory-policy noeviction`** — safe default; pure cache use cases override (see below)

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
