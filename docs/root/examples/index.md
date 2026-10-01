---
date: 2026-08-28T05:01:42Z
dateCreated: 2026-07-05T04:30:14.405Z
description: Ready-to-run compose.yaml examples for Incus - Caddy, Gitea, Immich, Hugo, Kimai, PowerDNS, wikis, DNS resolvers and registry caches.
editor: markdown
tags: []
title: Examples
leafwiki_id: 7c5CgufvR
leafwiki_title: Examples
leafwiki_created_at: "2026-07-05T04:30:14.40523729Z"
leafwiki_updated_at: "2026-08-28T05:01:42Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Examples

- [caddy](caddy/) - Caddy as a reverse-proxy front door, split in two: a
  host-facing instance and an internal one, sharing one certificate store.
- [gitea](gitea/) - Gitea, a lightweight self-hosted Git service, backed by
  Postgres.
- [hugo](hugo/) - Hugo is one of the most popular open-source static site
  generators: fast builds, no runtime dependencies.
- [immich](immich/) - Immich, a self-hosted photo and video backup solution.
- [kimai](kimai/) - Kimai, an open-source time-tracking application, backed by
  MariaDB.
- [leafwiki](leafwiki/) - LeafWiki, a self-hosted wiki as a single Go binary,
  Markdown + SQLite on disk, no external database.
- [oci-registry-cache](oci-registry-cache/) - One `ociregistry` instance as a
  pull-through cache for every upstream registry at once, with the reverse proxy
  naming the upstream per vhost.
- [pdns](pdns/) - A split-horizon home resolver: `dnscrypt-proxy` as the
  client-facing resolver, PowerDNS as the authoritative server for the local
  zone, with PowerDNS-Admin and MariaDB behind it.
- [wikijs](wikijs/) - Wiki.js, a modern wiki app, backed by Postgres.
