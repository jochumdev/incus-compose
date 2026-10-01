---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
tags: []
leafwiki_id: bk8AJnwvg
leafwiki_title: Backup
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# backup

Copy the project's named volumes into a separate Incus project,
`<project>-backup`, and keep per-run restore points on them. `down`,
`down --volumes` and `down --project` never touch that project, which is the
point of it.

Demo: https://asciinema.org/a/1263992

```
incus-compose backup <subcommand>
```

| Subcommand                         | Description                                          |
| ---------------------------------- | ---------------------------------------------------- |
| `create [SERVICE...]`              | Copy the volumes and take a restore point            |
| `list`                             | The runs recorded so far                             |
| `verify [TIMESTAMP]`               | Check a run's restore points are all still there     |
| `restore [TIMESTAMP] [SERVICE...]` | Put a run's contents back into the project's volumes |
| `delete [TIMESTAMP]`               | Drop a run, or prune with `--keep-last`              |

Every subcommand takes `--pool`, which overrides
[`x-incus-compose.backup.pool`](/extras#backup). Bind mounts are not backed up -
use a named volume for anything worth keeping.

Each run copies the volume with a refresh, so only what changed moves, and then
snapshots the copy. The copies themselves are never deleted: they are the base
the next refresh sends a delta against.

## backup create

```
incus-compose backup create [SERVICE...]
```

| Option   | Description                                                              |
| -------- | ------------------------------------------------------------------------ |
| `--name` | Name for this run, shown by `list`                                       |
| `--live` | Copy while the services run, which is crash-consistent rather than clean |
| `--pool` | Storage pool for the backup volumes                                      |

Without `--live` the services in scope are stopped for the copy and started
again afterwards.

## backup list

```
incus-compose backup list
```

| Option     | Description                 |
| ---------- | --------------------------- |
| `--format` | table (default), yaml, json |
| `--pool`   | Storage pool to look in     |

`SIZE` is what the run's backup volumes occupy. Incus reports usage per volume
and not per restore point, so runs sharing a volume report the same figure, and
pools that do not track per-volume usage - `dir` among them - report `0B`.

## backup verify

```
incus-compose backup verify [TIMESTAMP]
```

| Option     | Description                 |
| ---------- | --------------------------- |
| `--format` | table (default), yaml, json |
| `--pool`   | Storage pool to look in     |

Checks the newest run, or the one the timestamp names. Each volume reports `ok`,
`backup volume missing` or `restore point missing`, and the project is compared
against the run: a volume added since reads `not in this backup`, one removed
reads `no longer in the project`. Exits non-zero if any row is not `ok`, so it
works from a cron.

## backup restore

```
incus-compose backup restore [TIMESTAMP] [SERVICE...]
```

| Option      | Description                                         |
| ----------- | --------------------------------------------------- |
| `--volume`  | Restore only this volume (repeatable)               |
| `--dry-run` | Print what would be restored and stop               |
| `--yes`     | Restore without asking; required without a terminal |
| `--pool`    | Storage pool to restore from                        |

Restores the newest run unless a timestamp is given. This overwrites live data,
so it refuses while any of the services holding those volumes is running - stop
them first with `incus-compose stop`.

```bash
incus-compose stop
incus-compose backup restore --dry-run
incus-compose backup restore --yes
incus-compose start
```

## backup delete

```
incus-compose backup delete [TIMESTAMP]
```

| Option        | Description                       |
| ------------- | --------------------------------- |
| `--keep-last` | Delete every run but the newest N |
| `--pool`      | Storage pool to delete from       |

Takes a timestamp or `--keep-last`, not both and not neither. It removes the
restore points and the run's manifest; the backup volumes stay, so the next
`create` is still a delta. Deleting the newest run therefore costs a full copy
next time, and says so.

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command          | Variable                                | Flag          | Description                          |
| ---------------- | --------------------------------------- | ------------- | ------------------------------------ |
| all              | `INCUS_COMPOSE_BACKUP_POOL`             | `--pool`      | Storage pool for backup volumes      |
| `backup list`    | `INCUS_COMPOSE_BACKUP_LIST_FORMAT`      | `--format`    | Output format                        |
| `backup verify`  | `INCUS_COMPOSE_BACKUP_VERIFY_FORMAT`    | `--format`    | Output format                        |
| `backup restore` | `INCUS_COMPOSE_BACKUP_RESTORE_VOLUME`   | `--volume`    | Restore only these volumes           |
| `backup delete`  | `INCUS_COMPOSE_BACKUP_DELETE_KEEP_LAST` | `--keep-last` | Delete every backup but the newest N |

`backup restore --yes` and `backup restore --dry-run` have no variable - see the
exceptions table in
[Environment Variables](/environment-variables#cli-configuration).
