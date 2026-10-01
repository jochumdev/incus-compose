---
date: 2026-08-08T02:12:01Z
dateCreated: 2026-07-12T02:07:30.949Z
description: Run Immich, self-hosted photo and video backup, on Incus - five services following Immich's own Compose layout.
editor: markdown
tags: []
title: Immich
leafwiki_id: 7BiJw5Yvg
leafwiki_title: Immich
leafwiki_created_at: "2026-07-12T02:07:30Z"
leafwiki_updated_at: "2026-08-08T02:12:01Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Immich

[Immich](https://immich.app/), a self-hosted photo and video backup solution.

The files for this example are on
[Github](https://github.com/lxc/incus-compose/tree/main/examples/immich).

## The example

Five services, following
[Immich's official Compose layout](https://docs.immich.app/install/docker-compose):
`server`, `machine-learning`, `microservices` (background workers, split from
`server` via `IMMICH_WORKERS_INCLUDE`/`EXCLUDE`), `redis`, and `database` (a
Postgres fork with vector search support). Version, secrets, and storage paths
come from `.env`.

Arrows are `depends_on: service_healthy`; `server` and `microservices` share the
`library` volume:

```mermaid
flowchart LR
    SRV["server<br/>published on 2283"]
    MS["microservices<br/>background workers"]
    ML["machine-learning<br/>model-cache volume"]
    R[(redis)]
    DB[("database<br/>Postgres with vector search")]
    LIB["library volume<br/>pool from UPLOAD_POOL"]

    SRV --> R
    SRV --> DB
    SRV --> ML
    SRV --> MS
    MS --> R
    MS --> DB
    SRV --- LIB
    MS --- LIB
```

## Usage

```bash
incus-compose up
```

Open http://10.131.32.17:2283/

## Notes

- `library`'s storage pool comes from `UPLOAD_POOL` in `.env` via
  `x-incus-compose.pool`; set it before first run if you need another pool than
  the default.
