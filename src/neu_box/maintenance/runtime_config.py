"""Initialize the bundled OCI runtime's env file during Worker setup."""

from __future__ import annotations

import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path

from neu_box.maintenance.paths import CTL_BIN, HOOK_BIN, RUNTIME_BIN

_VERSION = "1"
_DEFAULT_HOOK = str(HOOK_BIN)
_FORMER_PACKAGED_HOOK = "/usr/local/bin/neu-box-hook"
_DEFAULT_PHASE = "createRuntime"
_DEFAULT_RUNC = "/usr/local/bin/runc"
_RUNC_PATHS = (
    "/usr/local/bin/runc", "/usr/bin/runc", "/bin/runc",
    "/usr/local/sbin/runc", "/usr/sbin/runc",
)
_DOCKER_PATHS = (
    "/usr/local/bin/docker", "/usr/bin/docker", "/bin/docker",
    "/usr/local/sbin/docker", "/usr/sbin/docker",
)
_KEYS = (
    "NEU_BOX_CONFIG_VERSION",
    "NEU_BOX_WORKER_URL",
    "NEU_BOX_HOOK",
    "NEU_BOX_HOOK_PHASE",
    "NEU_BOX_REAL_RUNC",
    "NEU_BOX_CAP_GUARD",
)
_HEADER = (
    "# Neu Box Runtime configuration (managed by neuboxctl setup)\n"
    "# Custom values and comments are preserved when setup runs again.\n"
)


@dataclass(frozen=True)
class RuntimeConfig:
    path: Path
    worker_url: str
    hook: str
    hook_phase: str
    real_runc: str
    cap_guard: str


def _assignment(line: str) -> tuple[str, str] | None:
    """Read an env assignment with the same value rules as the Go runtime."""
    stripped = line.strip()
    if not stripped or stripped.startswith("#") or "=" not in stripped:
        return None
    key, value = stripped.split("=", 1)
    key = key.strip().removeprefix("export ").strip()
    if not key:
        return None
    value = value.strip()
    if value.startswith(('"', "'")):
        end = value.find(value[0], 1)
        value = value[1:end] if end >= 0 else value[1:]
    else:
        for index in range(1, len(value)):
            if value[index] == "#" and value[index - 1].isspace():
                value = value[:index].strip()
                break
    return key, value


def _comment(value: str) -> str:
    """Keep an existing inline comment when replacing a managed value."""
    body = value.rstrip("\r\n")
    stripped = body.lstrip()
    if stripped.startswith(('"', "'")):
        end = stripped.find(stripped[0], 1)
        return stripped[end + 1:] if end >= 0 else ""
    for index in range(1, len(body)):
        if body[index] == "#" and body[index - 1].isspace():
            start = index - 1
            while start > 0 and body[start - 1].isspace():
                start -= 1
            return body[start:]
    return ""


def _binary(path: str, label: str, wrapper: Path | None = None) -> str:
    candidate = Path(path).expanduser()
    if not candidate.is_absolute():
        raise RuntimeError(f"{label} 必须是绝对路径: {path}")
    resolved = candidate.resolve()
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise RuntimeError(f"{label} 不存在或不可执行: {resolved}")
    if wrapper is not None and resolved == wrapper.resolve():
        raise RuntimeError(f"真实 runc 指向 Neu Box wrapper，配置后会递归调用: {resolved}")
    return str(resolved)


def _discover_binary(name: str, locations: tuple[str, ...]) -> str | None:
    """Look beyond sudo's secure_path for host-installed Docker binaries."""
    paths = (shutil.which(name), *locations)
    for path in paths:
        if path and Path(path).is_file() and os.access(path, os.X_OK):
            return str(Path(path).resolve())
    return None


def _encode_value(key: str, value: str) -> str:
    """Write a value that the Go runtime's dotenv reader can parse exactly."""
    if "\n" in value or "\r" in value:
        raise RuntimeError(f"{key} 不能包含换行符")
    needs_quotes = value.startswith(('"', "'")) or any(
        char == "#" and value[index - 1].isspace()
        for index, char in enumerate(value) if index > 0
    )
    if not needs_quotes:
        return value
    for quote in ("'", '"'):
        if quote not in value:
            return f"{quote}{value}{quote}"
    raise RuntimeError(f"{key} 含有无法写入 runtime.env 的引号和注释符")


def _write(path: Path, content: str) -> None:
    path.parent.mkdir(mode=0o750, parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", dir=path.parent,
            prefix=f".{path.name}.", delete=False,
        ) as stream:
            temporary = Path(stream.name)
            stream.write(content)
            stream.flush()
            os.fchmod(stream.fileno(), 0o640)
        os.replace(temporary, path)
    except OSError as exc:
        raise RuntimeError(f"写入 runtime 配置失败: {path}: {exc}") from exc
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def _render(lines: list[str], updates: dict[str, str]) -> str:
    """Replace owned keys in place; leave every other line and comment alone."""
    seen: set[str] = set()
    output: list[str] = []
    for line in lines:
        parsed = _assignment(line)
        key = parsed[0] if parsed else ""
        if key not in updates:
            output.append(line)
            continue
        seen.add(key)
        if parsed[1] == updates[key]:
            output.append(line)
            continue
        prefix, old_value = line.split("=", 1)
        ending = "\r\n" if line.endswith("\r\n") else "\n" if line.endswith("\n") else ""
        output.append(f"{prefix}={_encode_value(key, updates[key])}{_comment(old_value)}{ending}")
    if output and not output[-1].endswith(("\n", "\r")):
        output[-1] += "\n"
    output.extend(
        f"{key}={_encode_value(key, updates[key])}\n"
        for key in _KEYS if key not in seen
    )
    return "".join(output)


def ensure_runtime_config(
    path: Path,
    port: int,
    *,
    real_runc: str | None = None,
    wrapper: Path = RUNTIME_BIN,
) -> RuntimeConfig:
    """Create or update runtime.env without relying on a separate config CLI.

    The Worker port is authoritative. Existing custom runtime settings survive
    setup; an explicit runc override always wins over them. A default runc path
    is refreshed from PATH when possible.
    """
    try:
        with path.open("r", encoding="utf-8", newline="") as stream:
            source = stream.read()
    except FileNotFoundError:
        source = _HEADER
    except (OSError, UnicodeError) as exc:
        raise RuntimeError(f"读取 runtime 配置失败: {path}: {exc}") from exc
    lines = source.splitlines(keepends=True)
    values: dict[str, str] = {}
    for line in lines:
        parsed = _assignment(line)
        if parsed is not None:
            values[parsed[0]] = parsed[1]
    raw_version = values.get("NEU_BOX_CONFIG_VERSION") or "0"
    try:
        version = int(raw_version)
    except ValueError as exc:
        raise RuntimeError(f"runtime 配置版本不是整数: {raw_version!r}") from exc
    if version not in (0, 1):
        raise RuntimeError(f"不支持的 runtime 配置版本: {version}")

    docker_present = _discover_binary("docker", _DOCKER_PATHS) is not None
    discovered = _discover_binary("runc", _RUNC_PATHS)
    existing_runc = values.get("NEU_BOX_REAL_RUNC", "")
    if real_runc is not None:
        effective_runc = _binary(real_runc, "真实 runc", wrapper)
    elif existing_runc and existing_runc != _DEFAULT_RUNC:
        # A custom path may be another OCI runtime placed behind our wrapper.
        effective_runc = existing_runc
    elif existing_runc == _DEFAULT_RUNC and Path(existing_runc).is_file() and os.access(existing_runc, os.X_OK):
        effective_runc = existing_runc
    elif discovered:
        effective_runc = _binary(discovered, "真实 runc", wrapper)
    else:
        effective_runc = existing_runc or _DEFAULT_RUNC

    configured_hook = values.get("NEU_BOX_HOOK") or ""
    effective_hook = (
        _DEFAULT_HOOK if configured_hook in ("", _FORMER_PACKAGED_HOOK)
        else configured_hook
    )
    if docker_present:
        if not discovered and not existing_runc and real_runc is None:
            raise RuntimeError(
                "已安装 Docker，但找不到 runc；请用 "
                f"sudo {CTL_BIN} setup --real-runc /真实/runc/路径"
            )
        _binary(effective_runc, "真实 runc", wrapper)
        _binary(effective_hook, "OCI hook")

    cap_guard = values.get("NEU_BOX_CAP_GUARD") or "drop"
    if cap_guard.strip().lower() not in {"drop", "deny", "off"}:
        raise RuntimeError(f"无效的 NEU_BOX_CAP_GUARD: {cap_guard!r}")
    config = RuntimeConfig(
        path=path,
        worker_url=f"http://127.0.0.1:{port}",
        hook=effective_hook,
        hook_phase=values.get("NEU_BOX_HOOK_PHASE") or _DEFAULT_PHASE,
        real_runc=effective_runc,
        cap_guard=cap_guard,
    )
    updates = {
        "NEU_BOX_CONFIG_VERSION": _VERSION,
        "NEU_BOX_WORKER_URL": config.worker_url,
        "NEU_BOX_HOOK": config.hook,
        "NEU_BOX_HOOK_PHASE": config.hook_phase,
        "NEU_BOX_REAL_RUNC": config.real_runc,
        "NEU_BOX_CAP_GUARD": config.cap_guard,
    }
    rendered = _render(lines, updates)
    if rendered != source or not path.is_file() or path.stat().st_mode & 0o777 != 0o640:
        _write(path, rendered)
    return config
