"""Legacy worker.env values remain usable after setup migrates the file."""

import os
from pathlib import Path

import pytest

from neu_box.maintenance.worker_config import migrate_config


def test_migrate_legacy_worker_settings_once(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = tmp_path / "worker.env"
    config.write_text(
        "# Keep this operator note\n"
        "port=59123\n"
        "db_dir=/var/lib/neu-box/old\n"
        "NEU_BOX_DEVICE_INFO_SCRIPT=/opt/neu-box/current/share/neu-box/info/npu_info.sh\n"
        "SITE_SETTING=kept\n",
        encoding="utf-8",
    )
    monkeypatch.delenv("NEU_BOX_PORT", raising=False)
    monkeypatch.delenv("NEU_BOX_DB_PATH", raising=False)

    migrate_config(config)

    migrated = config.read_text(encoding="utf-8")
    assert "# Keep this operator note\n" in migrated
    assert "NEU_BOX_PORT=59123\n" in migrated
    assert "NEU_BOX_DB_PATH=/var/lib/neu-box/old/neu_box.db\n" in migrated
    assert "NEU_BOX_DEVICE_INFO_SCRIPT=/usr/share/neu-box/info/npu_info.sh\n" in migrated
    assert "SITE_SETTING=kept\n" in migrated
    assert os.environ["NEU_BOX_PORT"] == "59123"
    assert os.environ["NEU_BOX_DB_PATH"] == "/var/lib/neu-box/old/neu_box.db"

    migrate_config(config)
    assert config.read_text(encoding="utf-8") == migrated


def test_canonical_file_and_explicit_environment_values_win(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = tmp_path / "worker.env"
    config.write_text(
        "port=59123\n"
        "NEU_BOX_PORT=59075\n"
        "db_dir=/var/lib/neu-box/old\n"
        "SITE_SETTING=kept\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("NEU_BOX_PORT", "59075")
    monkeypatch.setenv("NEU_BOX_DB_PATH", "/site/database.db")

    migrate_config(config)

    content = config.read_text(encoding="utf-8")
    assert "port=59123\n" in content
    assert content.count("NEU_BOX_PORT=") == 1
    assert "NEU_BOX_PORT=59075\n" in content
    assert "NEU_BOX_DB_PATH=/var/lib/neu-box/old/neu_box.db\n" in content
    assert "SITE_SETTING=kept\n" in content
    assert os.environ["NEU_BOX_PORT"] == "59075"
    assert os.environ["NEU_BOX_DB_PATH"] == "/site/database.db"
