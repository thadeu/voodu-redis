# Sentinel HA example — auto-failover for redis with quorum-based promotion.
#
# Two resources in the same scope:
#
#   1. `redis "clowk-lp" "redis"` — the data redis (3 replicas, async
#      replication). One master + 2 read replicas. Standard layout.
#
#   2. `redis "clowk-lp" "redis-quorum"` — the sentinel quorum (3 pods
#      running redis-sentinel). They watch the data redis, vote on
#      master health, and trigger automatic failover if the master
#      goes down for >5s. The `monitor = "clowk-lp/redis"` field
#      ties this resource to the data redis above.
#
# Auto-failover loop:
#
#   Master crashes → sentinels detect within ~5s → quorum (2-of-3)
#   votes a replica as new master → sentinel runs SLAVEOF NO ONE on
#   the chosen replica → other replicas re-pointed at new master →
#   sentinel calls back to voodu controller via the failover hook
#   (vd redis:failover --no-restart) → REDIS_MASTER_ORDINAL updates
#   in voodu store → linked consumers' REDIS_URL refreshes → apps
#   restart with new URL.
#
# Linking apps:
#
#   Two flavours:
#
#   - vd redis:link clowk-lp/redis-quorum clowk-lp/web
#       Emits REDIS_URL pointing at the current data master. Failover
#       updates the store; voodu's env-change rolling restart picks
#       up the new URL on consumers.
#
#   - vd redis:link --sentinel clowk-lp/redis-quorum clowk-lp/web
#       Same REDIS_URL plus REDIS_SENTINEL_HOSTS + REDIS_MASTER_NAME.
#       For sentinel-aware clients (ioredis Sentinel(...), redis-py
#       Sentinel(...), redis-rb sentinels: [...], lettuce). They
#       discover the master at runtime — no env-driven restart on
#       failover.

# ── Data redis ──────────────────────────────────────────────────────────────
# Standard 3-node setup. Pod-0 is master by convention; pods 1-2
# are async replicas. Failover (manual via `vd redis:failover --replica N`
# or automatic via sentinel below) flips REDIS_MASTER_ORDINAL.
redis "clowk-lp" "redis" {
  image    = "redis:8"
  replicas = 3
}

# ── Sentinel quorum ─────────────────────────────────────────────────────────
# Same kind (`redis`), but the `sentinel { }` block flips this resource
# to run redis-sentinel instead of redis-server. Quorum is implicit:
# (replicas / 2) + 1 → 3 pods → quorum 2 (survives 1 outage).
#
# Block presence IS the enable signal — `enabled = true` is implicit.
# Use `enabled = false` to soft-toggle off without deleting the block.
#
# replicas:
#   1     observer-only (NOT HA, useful for prototyping)
#   2     REJECTED at apply (quorum math hostile)
#   3+    HA. Default = 3.
redis "clowk-lp" "redis-quorum" {
  image = "redis:8"

  sentinel {
    monitor = "clowk-lp/redis"  # the data redis above
  }

  # Inject the controller URL so the failover hook can call back
  # to /plugin/redis/failover when sentinel auto-failovers. Without
  # this, sentinel still does the role flip in Redis but the voodu
  # store stays stale until the operator runs `vd redis:failover`
  # manually.
  #
  # The URL must be reachable from inside the sentinel container.
  # On a single-VM voodu install, host.docker.internal usually works;
  # on multi-VM, point at the controller's private IP.
  env = {
    VOODU_CONTROLLER_URL = "http://host.docker.internal:8080"
  }
}

# ── Optional: override sentinel directives via include ──────────────────────
# The generated /etc/sentinel/sentinel.conf ends with
# `include /etc/sentinel/conf.d/*.conf`. Drop any extra .conf
# files here to override down-after-milliseconds, failover-timeout,
# parallel-syncs, etc., without editing the bootstrap.
#
# Example: tighten the down-after window from 5s to 2s for
# latency-sensitive workloads:
#
#   asset "clowk-lp" "redis-quorum-overrides" {
#     defs = file("./conf/sentinel-overrides.conf")
#   }
#
#   redis "clowk-lp" "redis-quorum" {
#     # ... sentinel block as above ...
#     volumes = [
#       "${asset.clowk-lp.redis-quorum-overrides.defs}:/etc/sentinel/conf.d/overrides.conf:ro",
#     ]
#   }
#
# With ./conf/sentinel-overrides.conf:
#
#   sentinel down-after-milliseconds mymaster 2000
#   sentinel failover-timeout mymaster 30000
