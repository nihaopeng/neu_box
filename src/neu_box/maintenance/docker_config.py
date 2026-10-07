"""Install the bundled OCI runtime into Docker without discarding site settings."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from neu_box.maintenance.runtime_config import _discover_binary
from neu_box.maintenance.paths import CTL_BIN, RUNTIME_BIN


DAEMON_CONFIG = Path("/etc/docker/daemon.json")
RUNTIME_NAME = "neu-box-runtime"
RUNTIME_PATH = str(RUNTIME_BIN)
_DOCKER_PATHS = ("/usr/bin/docker", "/usr/local/bin/docker", "/bin/docker")
_DOCKERD_PATHS = ("/usr/bin/dockerd", "/usr/local/bin/dockerd", "/usr/sbin/dockerd")


@dataclass(frozen=True)
class DockerConfig:
    path: Path
    dockerd: str
    docker: str
    content: str | None


def _unique_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"重复的 JSON 键: {key}")
        result[key] = value
    return result


def _read_config(path: Path) -> dict[str, object]:
    if path.is_symlink():
        raise RuntimeError(f"Docker 配置是符号链接，请先检查: {path}")
    try:
        source = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return {}
    except (OSError, UnicodeError) as exc:
        raise RuntimeError(f"读取 Docker 配置失败: {path}: {exc}") from exc
    try:
        value = json.loads(source, object_pairs_hook=_unique_pairs)
    except ValueError as exc:
        raise RuntimeError(f"Docker 配置不是有效 JSON: {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise RuntimeError(f"Docker 配置顶层必须是 JSON 对象: {path}")
    return value


def prepare_docker_config(path: Path = DAEMON_CONFIG) -> DockerConfig | None:
    """Check the proposed config before changing the live daemon."""
    docker = _discover_binary("docker", _DOCKER_PATHS)
    dockerd = _discover_binary("dockerd", _DOCKERD_PATHS)
    if docker is None and dockerd is None:
        return None
    if docker is None or dockerd is None:
        raise RuntimeError("Docker 安装不完整：需要 docker 和 dockerd 才能配置容器运行时")
    current = _read_config(path)
    default = current.get("default-runtime")
    if default not in (None, "runc", RUNTIME_NAME):
        raise RuntimeError(
            f"Docker 当前配置了其他默认运行时 {default!r}；"
            "请先确认它与 neu-box-runtime 的调用链，再手动调整配置"
        )
    runtimes = current.get("runtimes", {})
    if not isinstance(runtimes, dict):
        raise RuntimeError("Docker 配置 runtimes 必须是 JSON 对象")
    existing = runtimes.get(RUNTIME_NAME, {})
    if not isinstance(existing, dict):
        raise RuntimeError(f"Docker 配置 runtimes.{RUNTIME_NAME} 必须是 JSON 对象")
    if default == RUNTIME_NAME and existing.get("path") == RUNTIME_PATH:
        return DockerConfig(path, dockerd, docker, None)
    desired = dict(current)
    desired_runtimes = dict(runtimes)
    desired_runtimes[RUNTIME_NAME] = {**existing, "path": RUNTIME_PATH}
    desired["runtimes"] = desired_runtimes
    desired["default-runtime"] = RUNTIME_NAME
    return DockerConfig(path, dockerd, docker, json.dumps(desired, ensure_ascii=False, indent=2) + "\n")


def _write_validated(plan: DockerConfig) -> None:
    if plan.content is None:
        return
    path = plan.path
    path.parent.mkdir(parents=True, mode=0o755, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", dir=path.parent,
            prefix=f".{path.name}.", delete=False,
        ) as stream:
            temporary = Path(stream.name)
            stream.write(plan.content)
            stream.flush()
            os.fchmod(stream.fileno(), path.stat().st_mode & 0o777 if path.exists() else 0o644)
            os.fsync(stream.fileno())
        checked = subprocess.run(
            [plan.dockerd, "--validate", "--config-file", str(temporary)],
            capture_output=True, text=True, timeout=20,
        )
        if checked.returncode:
            raise RuntimeError(f"Docker 拒绝新配置: {(checked.stderr or checked.stdout).strip()}")
        if path.exists():
            with tempfile.NamedTemporaryFile(
                dir=path.parent, prefix=f"{path.name}.neubox.bak.", delete=False,
            ) as backup:
                with path.open("rb") as source:
                    shutil.copyfileobj(source, backup)
                os.fchmod(backup.fileno(), path.stat().st_mode & 0o777)
                os.fsync(backup.fileno())
            print(f"Docker 配置备份: {backup.name}", flush=True)
        os.replace(temporary, path)
        temporary = None
        print(f"已更新 Docker 配置: {path}", flush=True)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def _live_default(plan: DockerConfig) -> str | None:
    result = subprocess.run(
        [plan.docker, "info", "--format", "{{.DefaultRuntime}}"],
        capture_output=True, text=True, timeout=15,
    )
    return result.stdout.strip() if result.returncode == 0 else None


def _runtime_active(plan: DockerConfig) -> bool:
    if _live_default(plan) != RUNTIME_NAME:
        return False
    result = subprocess.run(
        [plan.docker, "info", "--format", "{{json .Runtimes}}"],
        capture_output=True, text=True, timeout=15,
    )
    if result.returncode:
        return False
    try:
        runtimes = json.loads(result.stdout)
        return runtimes[RUNTIME_NAME]["path"] == RUNTIME_PATH
    except (KeyError, TypeError, ValueError):
        return False


def verify_docker_ready(path: Path = DAEMON_CONFIG) -> None:
    """Prevent an explicit resume while Docker still uses its old runtime."""
    if _read_config(path).get("default-runtime") != RUNTIME_NAME:
        return
    plan = prepare_docker_config(path)
    if plan is None or plan.content is not None or not _runtime_active(plan):
        raise RuntimeError(
            "Docker 尚未加载 neu-box-runtime；先执行 sudo systemctl restart docker，"
            "确认 docker info --format '{{.DefaultRuntime}}'，然后再执行 "
            f"sudo {CTL_BIN} resume"
        )


def activate_docker_config(plan: DockerConfig | None, *, restart: bool | None = None) -> bool:
    """Return False when the operator defers a required Docker restart."""
    if plan is None:
        return True
    _write_validated(plan)
    if plan.content is None and _runtime_active(plan):
        print("Docker 已使用 neu-box-runtime，无需重启。", flush=True)
        return True
    if restart is None:
        if sys.stdin.isatty():
            try:
                restart = input("现在重启 Docker？运行中的容器可能停止 [y/N]: ").strip().lower() in {"y", "yes"}
            except EOFError:
                restart = False
        else:
            restart = False
    if not restart:
        print(
            "Docker 尚未加载 neu-box-runtime；Worker 保持暂停。完成维护后执行：\n"
            "  sudo systemctl restart docker\n"
            "  docker info --format '{{.DefaultRuntime}}'\n"
            f"  sudo {CTL_BIN} resume",
            flush=True,
        )
        return False
    print("正在重启 Docker，等待命令返回。", flush=True)
    subprocess.run(["systemctl", "restart", "docker"], check=True)
    deadline = time.monotonic() + 60
    while not _runtime_active(plan):
        if time.monotonic() >= deadline:
            raise RuntimeError("Docker 重启后仍未使用 neu-box-runtime；Worker 保持暂停，请检查 docker.service 日志")
        time.sleep(1)
    return True
