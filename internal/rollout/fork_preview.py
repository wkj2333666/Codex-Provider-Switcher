"""Embedded, bounded SQLite metadata compatibility repair; no history writes."""
import json
import os
from pathlib import Path
import sqlite3
import stat
import sys
import tomllib
import uuid


def valid_id(value):
    if not isinstance(value, str) or str(uuid.UUID(value)) != value:
        raise ValueError("invalid thread id")
    return value


def regular(path):
    # Do not follow DB/rollout symlinks or their directory components.
    if path.resolve() != path.absolute() or not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError("unsafe storage path")
    return path


def database(path, mode):
    regular(path)
    for suffix in ("-wal", "-shm"):
        sibling = Path(str(path) + suffix)
        if sibling.exists() or sibling.is_symlink():
            regular(sibling)
    conn = sqlite3.connect(path.as_uri() + "?mode=" + mode, uri=True, timeout=0.25)
    conn.row_factory = sqlite3.Row
    return conn


def metadata(home, row):
    path = Path(row["rollout_path"])
    if not any(path.is_relative_to(home / name) for name in ("sessions", "archived_sessions")):
        raise ValueError("rollout outside history directories")
    regular(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError("invalid rollout")
        line = stream.readline(1024 * 1024 + 1)
    if len(line) > 1024 * 1024:
        raise ValueError("oversized metadata")
    record = json.loads(line)
    meta = record["payload"]
    if record["type"] != "session_meta" or meta["id"] != row["id"]:
        raise ValueError("wrong rollout identity")
    return meta


def thread_row(db, thread_id):
    # Other metadata columns may contain very large initial prompts. The repair
    # needs only a bounded preview and the fields used for identity checks.
    return db.execute(
        "SELECT id,substr(preview,1,8192) AS preview,archived,history_mode,"
        "thread_source,source,rollout_path FROM threads WHERE id=?", (thread_id,)
    ).fetchone()


def repair(home, thread_id):
    valid_id(thread_id)
    config_path = home / "config.toml"
    settings = {}
    if config_path.exists():
        if regular(config_path).stat().st_size > 1024 * 1024:
            raise ValueError("oversized config")
        settings = tomllib.loads(config_path.read_text())
    configured = settings.get("sqlite_home")
    environment = os.environ.get("CODEX_SQLITE_HOME", "").strip()
    sqlite_home = Path(configured or environment or home).expanduser()
    if not sqlite_home.is_absolute():
        sqlite_home = (home if configured else Path.cwd()) / sqlite_home
    db = database(sqlite_home / "state_5.sqlite", "rw")
    history = None
    try:
        db.execute("BEGIN IMMEDIATE")
        row = thread_row(db, thread_id)
        if row is None or row["archived"] or row["history_mode"] != "paginated":
            return ""
        if row["thread_source"] != "user" and not (
            row["thread_source"] is None and row["source"] in ("cli", "vscode", "appServer", "unknown")
        ):
            return ""
        if row["preview"]:
            return row["preview"][:8192]
        meta = metadata(home, row)
        base = meta.get("history_base")
        if not base:
            return ""
        valid_id(meta["forked_from_id"])

        # A before-first-message fork is intentionally empty. Check inherited
        # user items against each lineage cutoff before borrowing any preview.
        # A fork of an untouched fork may have user items only in an ancestor.
        seen = {thread_id}
        segments = []
        for _ in range(32):
            source_id = valid_id(base["thread_id"])
            end = base["end_ordinal_exclusive"]
            if source_id in seen or type(end) is not int or end < 0:
                raise ValueError("invalid fork lineage")
            seen.add(source_id)
            source = thread_row(db, source_id)
            if source is None:
                raise ValueError("missing fork ancestor")
            segments.append((source_id, end, (source["preview"] or "")[:8192]))
            base = metadata(home, source).get("history_base")
            if not base:
                break
        else:
            raise ValueError("fork lineage too deep")

        history = database(sqlite_home / "thread_history_1.sqlite", "ro")
        for source_id, end, preview in reversed(segments):
            if not preview.strip():
                continue
            exists = history.execute(
                "SELECT 1 FROM thread_items WHERE thread_id=? AND item_type='userMessage' "
                "AND rollout_ordinal<? LIMIT 1", (source_id, end)
            ).fetchone()
            if exists:
                db.execute("UPDATE threads SET preview=? WHERE id=? AND (preview='' OR preview IS NULL)",
                           (preview, thread_id))
                db.commit()
                return preview
        return ""
    finally:
        if history is not None:
            history.close()
        db.close()


if __name__ == "__main__":
    try:
        print(json.dumps(repair(Path(sys.argv[1]), sys.argv[2]), ensure_ascii=False))
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error):
        # Never expose prompts, config values or SQL contents in proxy errors.
        sys.exit(1)
