# Custom ACL users for redis without breaking the plugin's password
# management or replication.
#
# Pattern: declare additional users via inline `user ...` directives
# in /etc/redis/conf.d/, NOT via Redis's `aclfile` directive.
#
# WHY NOT aclfile (the footgun this example exists to prevent):
#
#   When `aclfile <path>` is set in redis.conf, Redis loads the file
#   AFTER all conf parse — replacing every user it finds, INCLUDING
#   the default user. The plugin's `requirepass` directive (appended
#   at the end of redis.conf) sets the default user's password, but
#   the aclfile load wipes that and recreates default with whatever
#   the file says.
#
#   If the file doesn't define `user default`, Redis 7+ creates the
#   default user with `on nopass ~* +@all` — full open access, no
#   password. The cluster goes wide open silently and the operator
#   only finds out when something terrible happens.
#
#   If the file DOES define `user default >somepass ...`, that
#   password overrides the plugin's REDIS_PASSWORD. Replication
#   breaks (replicas auth with plugin's password, master expects
#   the file's). `vd redis:new-password` rotation also stops working.
#
# WHY this pattern (inline users in conf.d) is safe:
#
#   Files mounted into /etc/redis/conf.d/ are loaded via
#   `include /etc/redis/conf.d/*.conf` in the plugin's redis.conf,
#   which runs BEFORE the plugin's `requirepass` + `masterauth`
#   directives at the file's end. `user` directives parsed inline
#   set up custom users; the default user stays controlled by
#   requirepass at the very bottom (last-wins for that specific
#   directive — Redis treats it as the default user's password).
#
#   Replication keeps working. Password rotation keeps working.
#   Custom users get whatever permissions the operator grants.

# An external service serves the user definitions, fetched once at
# apply time. Could also be `file("./conf/users.conf")` for a
# vendored static file — both produce a string that gets mounted.
asset "clowk-lp" "redis-users" {
  defs = url("https://myapp.com?acls=users.conf", {
    timeout    = "10s"
    on_failure = "error"
  })
}

redis "clowk-lp" "redis" {
  image    = "redis:8"
  replicas = 2

  plugin {
    version = "latest"
  }

  volumes = [
    # Mounting at /etc/redis/conf.d/users.conf — the plugin's redis.conf
    # has `include /etc/redis/conf.d/*.conf`, so this file's directives
    # are parsed inline. CRITICAL: do NOT mount at /etc/redis/users.acl
    # via Redis's aclfile mechanism — see the long header comment.
    "${asset.clowk-lp.redis-users.defs}:/etc/redis/conf.d/users.conf:ro",
  ]
}
