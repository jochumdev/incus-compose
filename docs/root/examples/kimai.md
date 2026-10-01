---
date: 2026-08-08T02:12:01Z
dateCreated: 2026-07-12T02:07:55.448Z
description: Run Kimai, an open-source time-tracking application, on Incus with a MariaDB database and credentials kept in .env.
editor: markdown
tags: []
title: Kimai
leafwiki_id: 81lJw5LDg
leafwiki_title: Kimai
leafwiki_created_at: "2026-07-12T02:07:55Z"
leafwiki_updated_at: "2026-08-08T02:12:01Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Kimai

[Kimai](https://www.kimai.org/en/), an open-source time-tracking application,
backed by MariaDB.

The files for this example are on
[Github](https://github.com/lxc/incus-compose/tree/main/examples/kimai).

## The example

Two services: `mariadb` and `kimai`. Database credentials are passed as Compose
`secrets` sourced from environment variables (`db-password`, `db-root-password`,
`db-my-cnf`) rather than plain `environment:` entries, keeping them out of the
rendered container config. All values come from `.env`.

## Usage

```bash
incus-compose up
```

Open http://10.137.32.17:8001/
