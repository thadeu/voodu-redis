# voodu-redis

Voodu macro plugin that expands a `redis { … }` HCL block into a statefulset manifest.

The plugin is a **dumb alias of statefulset with sensible defaults** — every statefulset attribute the operator declares wins; the plugin only fills in what's missing. Custom config (redis.conf, ACLs, TLS) flows through the `asset` kind, not via plugin knobs.

## Defaults

Bare block — `redis "data" "cache" {}` — produces:

```hcl
statefulset "data" "cache" {
  image    = "redis:7-alpine"
  replicas = 1

  command = ["redis-server", "--appendonly", "yes"]

  ports = ["6379"]

  volume_claim "data" {
    mount_path = "/data"
  }
}
```

Print the skeleton:

```bash
voodu-redis defaults
```

## Production hardening — via `asset`

Custom redis.conf, ACL files, TLS certs all flow through a sibling `asset` block:

```hcl
asset "data" "redis-config" {
  configuration = file("./redis/redis.conf")
  users_acl     = url("https://r2.example.com/redis/users.acl")
  ca_pem        = file("./redis/ca.pem")
}

redis "data" "cache" {
  image   = "redis:8"
  command = ["redis-server", "/etc/redis/redis.conf"]

  volumes = [
    "${asset.redis-config.configuration}:/etc/redis/redis.conf:ro",
    "${asset.redis-config.users_acl}:/etc/redis/users.acl:ro",
    "${asset.redis-config.ca_pem}:/etc/redis/ca.pem:ro",
  ]

  ports = ["10.0.0.5:6379:6379"]
}
```

The redis.conf you ship can carry every Redis 7+ knob — `maxmemory`, `maxmemory-policy`, `aclfile /etc/redis/users.acl`, `tls-port`, etc. The plugin doesn't try to abstract individual flags — the file IS the abstraction.

Server materialises the asset bytes, interpolates `${asset.redis-config.X}` to real host paths, mounts as bind volumes. Asset content drift triggers rolling restart automatically.

## Override anything

```hcl
redis "data" "cache" {
  ports = ["10.0.0.5:6379:6379"]

  # Skip persistent storage — tmpfs-only.
  volume_claims = []
}
```

## Connecting from another app

```bash
vd config set -s myapp REDIS_URL="redis://cache-0.data:6379/0"
vd apply -f voodu.hcl
```

## Storage

Each ordinal gets a docker volume `voodu-<scope>-<name>-data-<ordinal>`. AOF data persists across restarts, image bumps, and rolling restarts. Wipe explicitly:

```bash
vd delete statefulset/<scope>/<name> --prune
```

## Install

JIT-installed by `vd apply` on first apply containing a `redis { … }` block. Pin manually:

```bash
vd plugins:install redis --repo thadeu/voodu-redis
```

## Plugin contract

`expand` reads stdin and writes a statefulset envelope:

```bash
echo '{"kind":"redis","scope":"data","name":"cache"}' | voodu-redis expand
# {"status":"ok","data":{"kind":"statefulset",…}}
```

Merge rules: `env` deep, rest operator-wins shallow, empty values opt out.

## Development

```bash
make build
make cross
make test

echo '{"kind":"redis","scope":"data","name":"cache"}' | bin/voodu-redis expand
```

## License

MIT.
