# Custom ACL users (without breaking replication)

This example shows the **safe pattern** for adding Redis users on top
of `voodu-redis`'s managed `default` user, avoiding two footguns that
silently break the cluster.

## TL;DR

Mount user directives at `/etc/redis/conf.d/<anything>.conf` — NEVER
use Redis's `aclfile` directive in a `voodu-redis`-managed instance.

```hcl
asset "scope" "redis-users" {
  defs = file("./conf/users.conf")
}

redis "scope" "redis" {
  replicas = 2
  volumes = [
    "${asset.scope.redis-users.defs}:/etc/redis/conf.d/users.conf:ro",
  ]
}
```

```conf
# conf/users.conf
user appwriter on >app-secret ~app:* +@write +@read
user readonly  on nopass     ~*      +@read

# Do NOT define `user default ...` here — see "Why not aclfile" below.
```

## Why this works (inline `user` in conf.d)

The plugin's `redis.conf` ends with:

```
include /etc/redis/conf.d/*.conf
requirepass <generated-or-operator-set-password>
masterauth  <same-password>
```

Files in `conf.d/` are parsed inline as part of the conf, in the order
Redis encounters them. `user` directives there set up custom users.
The plugin's `requirepass` at the bottom sets the password on the
`default` user — Redis's last-wins semantics for that specific
directive means the plugin keeps control of `default`.

Replication keeps working: replicas auth with `masterauth`, master
accepts because `default`'s password matches.

`vd redis:new-password` keeps working: rotates `requirepass` +
`masterauth` atomically, your custom users in `users.conf` are
untouched.

## Why NOT `aclfile` (the footgun this example exists to document)

```conf
# conf/aclfile.conf — DO NOT DO THIS
aclfile /etc/redis/users.acl
```

```hcl
# DO NOT DO THIS
volumes = [
  "${asset.x.acls}:/etc/redis/users.acl",                # mounted file
  "${asset.x.cfg}:/etc/redis/conf.d/aclfile.conf:ro",    # `aclfile` directive
]
```

When `aclfile <path>` is set, Redis loads that file **after** the
entire conf parse — replacing every user definition, **including
default**. The plugin's `requirepass` already ran at parse time, but
its effect on `default` is wiped by the aclfile load.

Two failure modes, both bad:

### Failure mode 1 — silent open access (CVE-grade footgun)

If `users.acl` doesn't define `user default ...`, Redis 7+ creates
the default user with `on nopass ~* +@all`:

```bash
$ redis-cli PING       # no auth, full access
PONG
$ redis-cli FLUSHALL
OK                     # bye data
```

`requirepass` looks correct in the conf, but it's a no-op after the
aclfile load. Cluster wide open, operator unaware.

### Failure mode 2 — replication broken silently

If `users.acl` defines `user default >somepass ...`, that password
overrides the plugin's `requirepass`. Master now expects `somepass`,
replicas auth with `<plugin-password>` (from `masterauth`):

```
1:M MASTER aborted replication with an error: WRONGPASS invalid
username-password pair or user is disabled.
```

Replicas retry forever, `master_link_status:down`. Writes to master
work, but data never replicates. Operator finds out hours later when
a replica is queried and missing recent data.

`vd redis:new-password` also stops working — it rotates
`requirepass` but the file's `default` user keeps the old password.

## Verification after applying this example

```bash
PASS=$(vd config get clowk-lp/redis | awk -F= '/REDIS_PASSWORD/{print $2}')

# Default user has password (not nopass)
docker exec clowk-lp-redis.0 redis-cli -a "$PASS" ACL GETUSER default | head -10
# Look for: "passwords" -> [<hash>], "flags" -> "on" "allcommands" "allkeys"
# Bad sign:  "flags" -> "on" "nopass" → cluster is wide open

# Custom users present
docker exec clowk-lp-redis.0 redis-cli -a "$PASS" ACL LIST
# Should list: default, appwriter, readonly, plus any others

# Replication healthy
docker exec clowk-lp-redis.1 redis-cli -a "$PASS" INFO replication | grep master_link_status
# Expected: master_link_status:up

# Reject unauth'd connections
docker exec clowk-lp-redis.0 redis-cli PING
# Expected: NOAUTH Authentication required.
```

## Files in this example

```
examples/custom-acls/
├── README.md          this doc
├── voodu.hcl          the HCL pattern (inline users.conf via conf.d/)
└── conf/
    └── users.conf     custom user definitions (no `user default`!)
```

To use:

```bash
cd examples/custom-acls
# edit conf/users.conf with your real users + passwords
vd apply -f voodu.hcl
```
