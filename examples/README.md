# Examples

- [caddy](https://docs.incus-compose.org/examples/caddy) — Caddy as a reverse-proxy front door, split in two — a host-facing instance and an internal one, sharing one certificate store.
- [gitea](https://docs.incus-compose.org/examples/gitea/) — Gitea, a lightweight self-hosted Git service, backed by Postgres.
- [hugo](https://docs.incus-compose.org/examples/hugo/) — Hugo is one of the most popular open-source static site generators — fast builds, no runtime dependencies.
- [immich](https://docs.incus-compose.org/examples/immich/) — Immich, a self-hosted photo and video backup solution.
- [kimai](https://docs.incus-compose.org/examples/kimai/) — Kimai, an open-source time-tracking application, backed by MariaDB.
- [leafwiki](https://docs.incus-compose.org/examples/leafwiki/) — LeafWiki — a self-hosted wiki as a single Go binary, Markdown + SQLite on disk, no external database.
- [many-dependencies](many-dependencies/) — Testbed and example for a deep service dependency graph, exercising `depends_on` with `condition: service_healthy`.
- [oci-registry-cache](https://docs.incus-compose.org/examples/oci-registry-cache/) — Runs distribution registry instances as pull-through caches, one per upstream registry, so container images are fetched once and served locally on subsequent pulls.
- [pdns](https://docs.incus-compose.org/examples/pdns/) — A split-horizon home resolver: `dnscrypt-proxy` as the client-facing resolver, PowerDNS as the authoritative server for the local zone, with PowerDNS-Admin and MariaDB behind it.
- [raw-bind-mount](raw-bind-mount/) — Bind-mounting an arbitrary host path into an Incus container via `x-incus.raw.lxc`, for when a regular Compose `volumes:` bind mount isn't expressive enough.
- [wikijs](https://docs.incus-compose.org/examples/wikijs/) — Wiki.js, a modern wiki app, backed by Postgres.
