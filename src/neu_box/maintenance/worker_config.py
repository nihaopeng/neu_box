"""Migrate legacy Worker environment settings during setup."""

from __future__ import annotations

import os
from pathlib import Path


_LEGACY_CONFIG_KEYS = {
    "listen": "NEU_BOX_LISTEN",
    "port": "NEU_BOX_PORT",
    "device_filter": "NEU_BOX_DEVICE_FILTER",
    "sandbox_reaper_interval": "NEU_BOX_SANDBOX_REAPER_INTERVAL",
    "command_timeout": "NEU_BOX_COMMAND_TIMEOUT",
    "command_max_completed": "NEU_BOX_COMMAND_MAX_COMPLETED",
    "command_queue_recent": "NEU_BOX_COMMAND_QUEUE_RECENT",
}


def _unquote(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
        return value[1:-1]
    # Match systemd/dotenv's conventional inline comment form while keeping
    # literal '#' characters that are part of an unspaced value (for example
    # a device regex).
    if " #" in value:
        value = value.split(" #", 1)[0].rstrip()
    return value


def _legacy_value(key: str, value: str) -> tuple[str, str] | None:
    """Return the canonical key/value for one legacy setting.

    ``db_dir`` was the old directory setting (the database filename was
    always ``neu_box.db``), so it needs a value transformation rather than a
    simple rename.  Path settings are intentionally migrated only when they
    point at the old source/``/opt`` layout; an operator supplied custom path
    remains untouched.
    """
    value = _unquote(value)
    if not value:
        return None
    if key == "db_dir":
        return "NEU_BOX_DB_PATH", str(Path(value).expanduser() / "neu_box.db")
    canonical = _LEGACY_CONFIG_KEYS.get(key)
    if canonical:
        return canonical, value
    if key == "dev_info_script_path":
        return "NEU_BOX_DEVICE_INFO_SCRIPT", (
            "/usr/share/neu-box/info/" + Path(value).name
        )
    if key == "sandbox_script_path":
        return "NEU_BOX_SANDBOX_EXECUTABLE", (
            "/usr/libexec/neu-box/neu-box-sandbox"
        )
    if key == "NEU_BOX_SANDBOX_SCRIPT":
        return "NEU_BOX_SANDBOX_EXECUTABLE", (
            "/usr/libexec/neu-box/neu-box-sandbox"
        )
    if key == "NEU_BOX_SANDBOX_EXECUTABLE" and (
        value.endswith("sandbox.sh") or value.startswith("/opt/neu-box/current/")
    ):
        return key, "/usr/libexec/neu-box/neu-box-sandbox"
    if key == "NEU_BOX_DEVICE_INFO_SCRIPT" and (
        "/share/neu-box/info/" in value
        or value.startswith("/opt/neu-box/current/")
    ):
        return key, "/usr/share/neu-box/info/" + Path(value).name
    return None


def migrate_config(config: Path | None) -> None:
    """Convert keys from the pre-RPM env file in place, idempotently.

    RPM deliberately preserves ``%config(noreplace)`` files.  This small
    migration therefore belongs to setup, alongside the database migration,
    and only rewrites known legacy keys; unknown/local settings are retained.
    """
    if config is None or not config.is_file():
        return
    try:
        source = config.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeError(f"读取配置失败: {config}: {exc}") from exc
    lines = source.splitlines(keepends=True)
    updates: dict[str, str] = {}
    original_values: dict[str, str] = {}
    entries: list[tuple[int, str, str]] = []
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in stripped:
            continue
        key, value = stripped.split("=", 1)
        entries.append((index, key.strip(), value))
    canonical_present = {
        key for _index, key, value in entries
        if (key.startswith("NEU_BOX_") or key == "LOG_LEVEL")
        and _unquote(value)
    }
    # Rewrite recognized settings in place.  Appending a second key is unsafe:
    # dotenv implementations differ on whether the first or last duplicate
    # wins, and a stale value may already be present in os.environ.
    for index, key, value in entries:
        migrated = _legacy_value(key, value)
        if migrated is None:
            continue
        new_key, new_value = migrated
        # A canonical value already present in the operator's file wins over
        # a duplicate legacy alias.  This also avoids producing ambiguous
        # duplicate dotenv assignments.
        if key != new_key and new_key in canonical_present:
            continue
        updates.setdefault(new_key, new_value)
        original_values.setdefault(new_key, _unquote(value))
        # Preserve an explanatory comment only when the whole line is a plain
        # assignment; legacy files did not have a reliable inline-comment
        # grammar and values may legitimately contain '#'.
        lines[index] = f"{new_key}={new_value}\n"
    # Ensure a canonical setting is present when a legacy key was removed from
    # the line in favour of a newer duplicate already in the file.
    existing = {
        line.split("=", 1)[0].strip() for line in lines if "=" in line
    }
    if lines and not lines[-1].endswith(("\n", "\r")):
        lines[-1] += "\n"
    lines.extend(
        f"{key}={value}\n" for key, value in updates.items()
        if key not in existing
    )
    # Keep setup's current process in sync.  ``load_role_environment`` loads
    # before migration with override=False, so newly written canonical keys
    # would otherwise not affect database_path() or native path helpers until
    # the next invocation.  Explicit environment overrides always win.
    for key, value in updates.items():
        current = os.environ.get(key)
        old = original_values.get(key)
        if current is None or current == old:
            os.environ[key] = value
    if not updates:
        return
    temporary = config.with_name(f".{config.name}.migrate-{os.getpid()}")
    try:
        temporary.write_text("".join(lines), encoding="utf-8")
        temporary.chmod(config.stat().st_mode & 0o777)
        temporary.replace(config)
    except OSError as exc:
        temporary.unlink(missing_ok=True)
        raise RuntimeError(f"写入迁移后配置失败: {config}: {exc}") from exc
