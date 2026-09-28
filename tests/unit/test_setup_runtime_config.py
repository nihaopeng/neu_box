"""The combined RPM initializes runtime.env through neuboxctl setup."""

import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest

from neu_box import ctl
from neu_box.maintenance import pause, runtime_config, setup


def _executable(path: Path) -> Path:
    path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    path.chmod(0o755)
    return path


def _runtime_paths(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> tuple[Path, Path]:
    wrapper = _executable(tmp_path / "neu-box-runtime")
    hook = _executable(tmp_path / "neu-box-hook")
    monkeypatch.setattr(setup, "_RUNTIME_WRAPPER", wrapper)
    monkeypatch.setattr(setup, "RUNTIME_CONFIG_PATH", tmp_path / "runtime.env")
    monkeypatch.setattr(runtime_config, "_DEFAULT_HOOK", str(hook))
    monkeypatch.setattr(runtime_config, "_RUNC_PATHS", ())
    monkeypatch.setattr(runtime_config, "_DOCKER_PATHS", ())
    return wrapper, hook


def _which(**paths: Path | None):
    return lambda name: str(paths[name]) if paths.get(name) else None


def test_fresh_setup_discovers_runc_and_writes_runtime_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _, hook = _runtime_paths(monkeypatch, tmp_path)
    real_runc = _executable(tmp_path / "runc")
    docker = _executable(tmp_path / "docker")
    monkeypatch.setattr(runtime_config.shutil, "which", _which(runc=real_runc, docker=docker))

    result = setup.initialize_runtime_config(59123)

    content = (tmp_path / "runtime.env").read_text(encoding="utf-8")
    assert result.worker_url == "http://127.0.0.1:59123"
    assert result.real_runc == str(real_runc)
    assert f"NEU_BOX_HOOK={hook}\n" in content
    assert "NEU_BOX_CONFIG_VERSION=1\n" in content
    assert "NEU_BOX_HOOK_PHASE=createRuntime\n" in content
    assert "NEU_BOX_CAP_GUARD=drop\n" in content
    assert (tmp_path / "runtime.env").stat().st_mode & 0o777 == 0o640


def test_setup_finds_runc_outside_sudo_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    real_runc = _executable(tmp_path / "runc")
    docker = _executable(tmp_path / "docker")
    monkeypatch.setattr(runtime_config.shutil, "which", _which())
    monkeypatch.setattr(runtime_config, "_RUNC_PATHS", (str(real_runc),))
    monkeypatch.setattr(runtime_config, "_DOCKER_PATHS", (str(docker),))

    result = setup.initialize_runtime_config(59075)

    assert result.real_runc == str(real_runc)


def test_existing_custom_values_and_comments_survive_worker_url_sync(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    config = tmp_path / "runtime.env"
    config.write_text(
        "# operator note\n"
        "NEU_BOX_CONFIG_VERSION=1\n"
        "NEU_BOX_WORKER_URL=http://127.0.0.1:59075 # local worker\n"
        "NEU_BOX_HOOK=/opt/site/hook\n"
        "NEU_BOX_HOOK_PHASE=prestart\n"
        "NEU_BOX_REAL_RUNC=/opt/site/runc\n"
        "NEU_BOX_CAP_GUARD=deny\n"
        "SITE_SETTING=keep-this\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(runtime_config.shutil, "which", _which())

    result = setup.initialize_runtime_config(59123)

    content = config.read_text(encoding="utf-8")
    assert "# operator note\n" in content
    assert "NEU_BOX_WORKER_URL=http://127.0.0.1:59123 # local worker\n" in content
    assert "NEU_BOX_HOOK=/opt/site/hook\n" in content
    assert "NEU_BOX_HOOK_PHASE=prestart\n" in content
    assert "NEU_BOX_REAL_RUNC=/opt/site/runc\n" in content
    assert "NEU_BOX_CAP_GUARD=deny\n" in content
    assert "SITE_SETTING=keep-this\n" in content
    assert result.real_runc == "/opt/site/runc"
    assert config.stat().st_mode & 0o777 == 0o640
    before = config.stat().st_mtime_ns
    setup.initialize_runtime_config(59123)
    assert config.stat().st_mtime_ns == before


def test_existing_valid_custom_runtime_survives_discovery_on_docker_node(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    custom = _executable(tmp_path / "site-runc")
    discovered = _executable(tmp_path / "runc")
    docker = _executable(tmp_path / "docker")
    config = tmp_path / "runtime.env"
    config.write_text(f"NEU_BOX_REAL_RUNC={custom}\n", encoding="utf-8")
    monkeypatch.setattr(runtime_config.shutil, "which", _which(runc=discovered, docker=docker))

    result = setup.initialize_runtime_config(59075)

    assert result.real_runc == str(custom)
    assert f"NEU_BOX_REAL_RUNC={custom}\n" in config.read_text(encoding="utf-8")


def test_version_zero_and_duplicate_keys_follow_go_last_value_rule(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    config = tmp_path / "runtime.env"
    config.write_text(
        "# old config\n"
        "NEU_BOX_CONFIG_VERSION=0\n"
        "NEU_BOX_WORKER_URL=http://first:1\n"
        "NEU_BOX_WORKER_URL=http://last:2 # keep\n"
        "LOCAL_SETTING=unchanged\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(runtime_config.shutil, "which", _which())

    setup.initialize_runtime_config(59123)

    content = config.read_text(encoding="utf-8")
    assert "NEU_BOX_CONFIG_VERSION=1\n" in content
    assert content.count("NEU_BOX_WORKER_URL=http://127.0.0.1:59123") == 2
    assert "NEU_BOX_WORKER_URL=http://127.0.0.1:59123 # keep\n" in content
    assert "LOCAL_SETTING=unchanged\n" in content


def test_explicit_real_runc_overrides_existing_custom_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    config = tmp_path / "runtime.env"
    config.write_text("NEU_BOX_REAL_RUNC=/opt/site/old-runc # custom\n", encoding="utf-8")
    alternate = _executable(tmp_path / "alternate-runc")
    monkeypatch.setattr(runtime_config.shutil, "which", _which())

    result = setup.initialize_runtime_config(59075, str(alternate))

    assert result.real_runc == str(alternate)
    assert f"NEU_BOX_REAL_RUNC={alternate} # custom\n" in config.read_text(encoding="utf-8")
    assert ctl._parser().parse_args([
        "setup", "--real-runc", str(alternate),
    ]).real_runc == str(alternate)


def test_cli_passes_explicit_runc_to_setup(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(ctl, "load_role_environment", lambda _role, _config: None)
    monkeypatch.setattr(ctl, "env_int", lambda _name, _default: 59123)
    calls: list[tuple[int | None, int, Path | None, str | None, bool | None]] = []
    monkeypatch.setattr(
        ctl, "setup",
        lambda port, timeout, config, *, real_runc, restart_docker: (
            calls.append((port, timeout, config, real_runc, restart_docker)) or True
        ),
    )

    assert ctl.main(["setup", "--real-runc", "/opt/oci/runc"]) == 0
    assert calls == [(None, 60, None, "/opt/oci/runc", None)]


def test_missing_runc_on_docker_node_fails_before_writing_config(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    docker = _executable(tmp_path / "docker")
    monkeypatch.setattr(runtime_config.shutil, "which", _which(docker=docker))

    with pytest.raises(RuntimeError, match="找不到 runc"):
        setup.initialize_runtime_config(59075)

    assert not (tmp_path / "runtime.env").exists()


def test_existing_invalid_runtime_path_on_docker_node_fails_before_writing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    config = tmp_path / "runtime.env"
    original = "NEU_BOX_REAL_RUNC=/missing/runc\nNEU_BOX_WORKER_URL=http://127.0.0.1:59075\n"
    config.write_text(original, encoding="utf-8")
    docker = _executable(tmp_path / "docker")
    monkeypatch.setattr(runtime_config.shutil, "which", _which(docker=docker))

    with pytest.raises(RuntimeError, match="真实 runc 不存在或不可执行"):
        setup.initialize_runtime_config(59123)

    assert config.read_text(encoding="utf-8") == original


def test_host_only_setup_does_not_require_runc_or_hook(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    _runtime_paths(monkeypatch, tmp_path)
    monkeypatch.setattr(runtime_config.shutil, "which", _which())

    result = setup.initialize_runtime_config(59075)

    assert result.real_runc == "/usr/local/bin/runc"
    assert (tmp_path / "runtime.env").is_file()


def test_runc_cannot_resolve_to_neubox_wrapper(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path,
) -> None:
    wrapper, _ = _runtime_paths(monkeypatch, tmp_path)
    runc_symlink = tmp_path / "runc"
    runc_symlink.symlink_to(wrapper)
    monkeypatch.setattr(runtime_config.shutil, "which", _which(runc=runc_symlink))

    with pytest.raises(RuntimeError, match="递归调用"):
        setup.initialize_runtime_config(59075)


def test_runtime_config_failure_stops_setup_before_database_and_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(setup, "require_root", lambda: None)
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda command, **_kwargs: subprocess.CompletedProcess(command, 3),
    )
    monkeypatch.setattr(setup, "migrate_config", lambda _config: None)
    monkeypatch.setattr(setup, "env_int", lambda _name, _default: 59075)
    monkeypatch.setattr(
        setup, "initialize_runtime_config",
        lambda _port, _runc: (_ for _ in ()).throw(RuntimeError("runtime init failed")),
    )
    for name in ("migrate_database", "check_database", "mark_paused"):
        monkeypatch.setattr(
            setup, name,
            lambda *_args, **_kwargs: pytest.fail("database/service work must not start"),
        )

    with pytest.raises(RuntimeError, match="runtime init failed"):
        setup.setup(None, 60)


def test_setup_reports_effective_runtime_config(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(setup, "require_root", lambda: None)
    monkeypatch.setattr(
        setup.subprocess, "run",
        lambda command, **_kwargs: subprocess.CompletedProcess(
            command, 3 if command[:3] == ["systemctl", "is-active", "--quiet"] else 0,
        ),
    )
    monkeypatch.setattr(setup, "migrate_config", lambda _config: None)
    monkeypatch.setattr(setup, "env_int", lambda _name, _default: 59075)
    monkeypatch.setattr(
        setup, "initialize_runtime_config",
        lambda _port, _runc: runtime_config.RuntimeConfig(
            tmp_path / "runtime.env", "http://127.0.0.1:59075", "/opt/hook",
            "createRuntime", "/opt/runc", "drop",
        ),
    )
    monkeypatch.setattr(setup, "database_path", lambda: tmp_path / "worker.db")
    monkeypatch.setattr(setup, "migrate_database", lambda *_args: SimpleNamespace(current=1))
    monkeypatch.setattr(setup, "check_database", lambda *_args: SimpleNamespace(current=1))
    monkeypatch.setattr(setup, "mark_paused", lambda: None)
    monkeypatch.setattr(setup, "clear_pause_marker", lambda: None)
    monkeypatch.setattr(setup, "control_worker", lambda *_args: None)
    monkeypatch.setattr(setup, "prepare_docker_config", lambda: None)
    monkeypatch.setattr(
        setup, "request_worker",
        lambda *_args, **_kwargs: {
            "status": "ok", "role": "worker", "version": setup.__version__,
            "schema_version": 1,
        },
    )

    setup.setup(None, 5)

    output = capsys.readouterr().out
    assert f"Runtime 配置: {tmp_path / 'runtime.env'}" in output
    assert "Worker URL: http://127.0.0.1:59075" in output
    assert "OCI hook:   /opt/hook" in output
    assert "real runc:  /opt/runc" in output
    assert "cap guard:  drop" in output


def test_pause_backs_up_runtime_config(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runtime_file = tmp_path / "runtime.env"
    runtime_file.write_text("NEU_BOX_WORKER_URL=http://127.0.0.1:59075\n", encoding="utf-8")
    backup = tmp_path / "worker.sqlite"
    monkeypatch.setattr(pause, "RUNTIME_CONFIG_PATH", runtime_file)
    monkeypatch.setattr(pause, "require_root", lambda: None)
    monkeypatch.setattr(pause, "mark_paused", lambda: None)
    monkeypatch.setattr(pause, "control_worker", lambda _command, _port: {"quiet": True})
    monkeypatch.setattr(pause, "env_text", lambda _name: str(tmp_path))
    monkeypatch.setattr(pause, "database_path", lambda: tmp_path / "worker.db")
    monkeypatch.setattr(pause, "backup_database", lambda *_args: backup)
    monkeypatch.setattr(pause, "sandbox_executable_path", lambda: tmp_path / "sandbox")
    monkeypatch.setattr(pause.subprocess, "run", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(pause, "stop_worker_after_cleanup", lambda: None)

    pause.pause(59075, 0, None)

    assert backup.with_suffix(".runtime.env").read_bytes() == runtime_file.read_bytes()
