"""配置 OCI runtime 与 Docker，迁移数据库并启动 Worker。"""

from __future__ import annotations

import subprocess
import time
import urllib.error
from pathlib import Path

from neu_box import __version__
from neu_box.config import RUNTIME_CONFIG_PATH, env_int
from neu_box.migrations.engine import check_database, migrate_database
from neu_box.storage import (
    MIGRATIONS_PACKAGE,
    REQUIRED_COLUMNS,
    REQUIRED_INDEXES,
    database_path,
)
from neu_box.maintenance.pause import (
    SERVICE, control_worker, request_worker, require_root,
)
from neu_box.maintenance.markers import (
    clear_pause_marker, mark_paused,
)
from neu_box.maintenance.runtime_config import RuntimeConfig, ensure_runtime_config
from neu_box.maintenance.worker_config import migrate_config
from neu_box.maintenance.docker_config import prepare_docker_config, activate_docker_config
from neu_box.maintenance.paths import RUNTIME_BIN


_RUNTIME_WRAPPER = RUNTIME_BIN


def initialize_runtime_config(port: int, real_runc: str | None = None) -> RuntimeConfig:
    """Keep the bundled OCI hook's Worker URL in step with worker.env."""
    return ensure_runtime_config(
        RUNTIME_CONFIG_PATH, port, real_runc=real_runc,
        wrapper=_RUNTIME_WRAPPER,
    )


def setup(
    port: int | None,
    timeout: int,
    config: Path | None = None,
    real_runc: str | None = None,
    restart_docker: bool | None = None,
) -> bool:
    require_root()
    if subprocess.run(["systemctl", "is-active", "--quiet", SERVICE]).returncode == 0:
        raise RuntimeError("Worker 仍在运行，请先执行 neuboxctl pause")

    migrate_config(config)
    # Legacy env files used ``port``. Resolve the port after that key has
    # migrated, so the health check reaches the port the new service loads.
    # The environment is the sole source of the runtime port.
    port = env_int("NEU_BOX_PORT", 59075)
    if not 1 <= port <= 65535:
        raise RuntimeError(f"NEU_BOX_PORT 必须在 1-65535 之间，实际为 {port}")
    runtime = initialize_runtime_config(port, real_runc)
    docker = prepare_docker_config()
    print(
        f"Runtime 配置: {runtime.path}\n"
        f"  Worker URL: {runtime.worker_url}\n"
        f"  OCI hook:   {runtime.hook}\n"
        f"  real runc:  {runtime.real_runc}\n"
        f"  cap guard:  {runtime.cap_guard}",
        flush=True,
    )
    for operation in (migrate_database, check_database):
        status = operation(
            database_path(), MIGRATIONS_PACKAGE, REQUIRED_COLUMNS, REQUIRED_INDEXES,
        )
        print(f"{operation.__name__}: schema={status.current}", flush=True)

    # 在启动服务前设置，避免恢复的 pending 在健康检查前开始执行。
    # Keep the service paused while it is starting and during health checks.
    # The marker is consumed only after an explicit successful resume below.
    mark_paused()
    subprocess.run(["systemctl", "daemon-reload"], check=True)
    subprocess.run(["systemctl", "enable", "--now", SERVICE], check=True)
    print("正在等待 Worker 加载 BPF 并通过 /healthz 检查。", flush=True)
    expected_health = {
        'status': 'ok', 'role': 'worker',
        'version': __version__, 'schema_version': status.current,
    }
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(
                "Worker 健康检查超时，保持暂停；请查看 "
                "journalctl -u neuboxd.service"
            )
        try:
            health = request_worker(port, "/healthz", timeout=min(5, remaining))
        except (urllib.error.URLError, TimeoutError):
            time.sleep(min(1, max(0, deadline - time.monotonic())))
            continue
        if any(health.get(key) != value for key, value in expected_health.items()):
            raise RuntimeError(f"Worker 健康检查不通过，保持暂停: {health}")
        break
    if not activate_docker_config(docker, restart=restart_docker):
        return False
    control_worker("resume", port)
    clear_pause_marker()
    print("Worker 已就绪，已恢复任务调度。", flush=True)
    return True
