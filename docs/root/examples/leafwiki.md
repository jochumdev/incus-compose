---
date: 2026-08-08T02:12:01Z
dateCreated: 2026-07-12T02:08:27.812Z
description: Run LeafWiki on Incus - a self-hosted wiki as a single Go binary, Markdown and SQLite on disk, with optional Git backup.
editor: markdown
tags: []
title: LeafWiki
leafwiki_id: Mn_bwcYvg
leafwiki_title: LeafWiki
leafwiki_created_at: "2026-07-12T02:08:27Z"
leafwiki_updated_at: "2026-08-08T02:12:01Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# LeafWiki

[LeafWiki](https://leafwiki.com/) - a self-hosted wiki as a single Go binary,
Markdown + SQLite on disk, no external database.

The page you see is a LeafWiki with a Git backup to
[jochumdev/incus-compose-docs](https://github.com/jochumdev/incus-compose-docs).

The files for this example are on
[Github](https://github.com/lxc/incus-compose/tree/main/examples/leafwiki).

## The example

A single `wiki` service. Demonstrates:

- a bind-mounted SSH deploy key (`ssh.key`, a placeholder here) for LeafWiki's
  optional Git backup (`LEAFWIKI_GIT_BACKUP*` env vars)

All config comes from `.env`.

## Usage

```bash
incus-compose up
```

Open http://10.135.32.17:8080/
