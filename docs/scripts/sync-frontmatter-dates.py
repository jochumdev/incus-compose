#!/usr/bin/env python3
"""Sync the frontmatter date fields under root/ with what actually changed.

    ./scripts/sync-frontmatter-dates.py            # dry run, prints what would change
    ./scripts/sync-frontmatter-dates.py --apply

**Git decides which files are stamped, not mtime.** A file is restamped only
when its content differs from HEAD, ignoring the date keys themselves. mtime
cannot answer that question: a checkout, a clone, a `git checkout --` restore or
an editor rewrite all bump it without the content changing, so taking it as the
source meant editing two files restamped every file in the tree.

For a file that did change, the *updated* fields (`date`,
`leafwiki_updated_at`) come from its mtime - the edit is happening now, so the
filesystem is right about when. A clean file is left alone entirely, unless it
is missing one of the four keys, which is backfilled from its last commit.

Created fields are never taken from the filesystem: birth time only records when
a checkout last rewrote the file, so an existing `dateCreated` /
`leafwiki_created_at` is left byte-for-byte alone. A missing one is filled from
its counterpart, or from the first commit that added the file.

Files are rewritten with their mtime restored, so the values stay true and a
second run is a no-op - including before the stamp itself is committed. Only the
four date keys are touched; nothing else in the frontmatter is reformatted or
reordered.
"""

import argparse
import os
import re
import subprocess
from datetime import datetime, timezone
from pathlib import Path

DOCS = Path(__file__).resolve().parent.parent

UPDATED = ("date", "leafwiki_updated_at")
CREATED = ("dateCreated", "leafwiki_created_at")
# Insertion order for keys a file is missing entirely.
ORDER = ("date", "dateCreated", "leafwiki_created_at", "leafwiki_updated_at")
DATE_LINE = re.compile(r"^[+-](%s):" % "|".join(ORDER))


def fmt(dt, key):
    """Render dt in the precision and quoting each key already uses.

    The sub-second field is always zero: whole seconds is all the wiki needs,
    and it keeps the value stable across filesystems that differ in timestamp
    resolution.
    """
    stamp = dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
    return "%sZ" % stamp


def parse(value):
    v = value.strip().strip('"').strip("'")
    v = re.sub(r"(\.\d{6})\d+Z$", r"\1Z", v)  # datetime tops out at microseconds
    try:
        return datetime.fromisoformat(v.replace("Z", "+00:00"))
    except ValueError:
        return None


def git(*args):
    return subprocess.run(
        ["git", *args],
        cwd=DOCS,
        capture_output=True,
        text=True,
    ).stdout.strip()


def first_commit(path):
    out = git(
        "log", "--diff-filter=A", "--follow", "--format=%aI", "-1", "--", str(path)
    )
    return datetime.fromisoformat(out) if out else None


def last_commit(path):
    out = git("log", "--format=%aI", "-1", "--", str(path))
    return datetime.fromisoformat(out) if out else None


def changed_paths():
    """Paths under root/ whose content differs from HEAD, or None if unknown.

    A diff confined to the date keys does not count, so re-running before the
    stamp is committed does not stamp it again with a later time.
    """
    if (
        subprocess.run(
            ["git", "rev-parse", "--verify", "HEAD"], cwd=DOCS, capture_output=True
        ).returncode
        != 0
    ):
        return None

    paths = {
        DOCS / p
        for p in git(
            "ls-files", "--others", "--exclude-standard", "--", "root"
        ).splitlines()
    }

    for rel in git("diff", "HEAD", "--name-only", "--", "root").splitlines():
        lines = [
            line
            for line in git("diff", "HEAD", "-U0", "--", rel).splitlines()
            if line[:1] in "+-" and not line.startswith(("+++", "---"))
        ]
        if any(not DATE_LINE.match(line) for line in lines):
            paths.add(DOCS / rel)

    return paths


def split_frontmatter(text):
    """Return (lines, body) for a leading --- block, or (None, text)."""
    if not text.startswith("---\n"):
        return None, text
    end = text.find("\n---\n", 3)
    if end == -1:
        return None, text
    return text[4 : end + 1].splitlines(), text[end + 5 :]


def sync(path, apply, changed):
    st = path.stat()
    mtime = datetime.fromtimestamp(st.st_mtime_ns // 1000000000, timezone.utc)

    fm, body = split_frontmatter(path.read_text())
    is_new = fm is None
    if is_new:
        fm = []

    present = {}
    for i, line in enumerate(fm):
        m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*):(.*)$", line)
        if m and m.group(1) in ORDER:
            present[m.group(1)] = (i, m.group(2))

    if not changed and all(k in present for k in ORDER):
        return is_new, []

    if changed:
        edits = {k: fmt(mtime, k) for k in UPDATED}
    else:
        # Backfill only: mtime says nothing about a file nobody edited.
        stamp = last_commit(path) or mtime
        edits = {k: fmt(stamp, k) for k in UPDATED if k not in present}

    if not all(k in present for k in CREATED):
        known = [parse(v) for k, (_, v) in present.items() if k in CREATED]
        known = [d for d in known if d]
        fallback = next(iter(known), None) or first_commit(path) or mtime
        for key in CREATED:
            if key not in present:
                edits[key] = fmt(fallback, key)

    out, touched = list(fm), []
    for key in ORDER:
        if key not in edits:
            continue
        new = "%s: %s" % (key, edits[key])
        if key in present:
            i, old = present[key]
            if out[i] != new:
                touched.append("%s %s ->%s" % (key, old.strip(), edits[key]))
                out[i] = new
        else:
            touched.append("+%s" % key)
            before = [k for k in ORDER[: ORDER.index(key)] if k in present]
            at = present[before[-1]][0] + 1 if before else 0
            out.insert(at, new)
            present = {k: (i + 1 if i >= at else i, v) for k, (i, v) in present.items()}
            present[key] = (at, edits[key])

    if touched and apply:
        path.write_text(
            "---\n" + "\n".join(out) + "\n---\n" + ("\n" if is_new else "") + body
        )
        # Restore in ns: the float form rounds, and the frontmatter carries ns.
        os.utime(path, ns=(st.st_atime_ns, st.st_mtime_ns))

    return is_new, touched


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument(
        "--apply", action="store_true", help="write the files (default: dry run)"
    )
    args = ap.parse_args()

    changed = changed_paths()

    count = 0
    for path in sorted((DOCS / "root").rglob("*.md")):
        is_new, touched = sync(path, args.apply, changed is None or path in changed)
        if not touched:
            continue
        count += 1
        print("%s%s" % (path.relative_to(DOCS), "  NEW frontmatter" if is_new else ""))
        for t in touched:
            print("    %s" % t)

    print(
        "\n%d file(s) %s"
        % (count, "updated" if args.apply else "would change (dry run)")
    )


if __name__ == "__main__":
    main()
