"""提交任务的执行上下文在入队前校验，并在 Host 执行时使用。"""

import asyncio
import os
import pwd

import pytest

from neu_box.execution.host import _execute_in_sandbox
from neu_box.execution.target import TargetValidationError, normalize_execution_target


def test_host_target_accepts_workdir_and_explicit_environment(tmp_path):
    target = normalize_execution_target({
        'type': 'host', 'workdir': str(tmp_path), 'env': {'EXPERIMENT': 'one'},
    })

    assert target == {
        'type': 'host', 'workdir': str(tmp_path), 'env': {'EXPERIMENT': 'one'},
    }


def test_docker_mounts_require_existing_sources_and_unique_destinations(tmp_path):
    source = tmp_path / 'project'
    source.mkdir()
    target = normalize_execution_target({
        'type': 'docker', 'image': 'training:latest',
        'mounts': [{'source': str(source), 'target': '/workspace'}],
    })
    assert target['mounts'] == [
        {'source': str(source), 'target': '/workspace', 'read_only': True},
    ]

    with pytest.raises(TargetValidationError, match='重复的容器挂载路径'):
        normalize_execution_target({
            'type': 'docker', 'image': 'training:latest',
            'mounts': [
                {'source': str(source), 'target': '/workspace'},
                {'source': str(source), 'target': '/workspace'},
            ],
        })
    with pytest.raises(TargetValidationError, match='挂载源不存在'):
        normalize_execution_target({
            'type': 'docker', 'image': 'training:latest',
            'mounts': [{'source': str(tmp_path / 'missing'), 'target': '/workspace'}],
        })


def test_host_executor_uses_submitted_workdir_and_environment(monkeypatch, tmp_path):
    captured = {}

    async def stop_before_spawn(*args, **kwargs):
        captured.update(kwargs)
        raise RuntimeError('captured')

    monkeypatch.setattr(asyncio, 'create_subprocess_exec', stop_before_spawn)
    user = pwd.getpwuid(os.getuid()).pw_name
    outcome = asyncio.run(_execute_in_sandbox(
        'python train.py', 'sbx_test.slice', username=user,
        target={'type': 'host', 'workdir': str(tmp_path),
                'env': {'EXPERIMENT': 'submitted'}},
    ))

    assert outcome['error'] == 'exception'
    assert captured['cwd'] == str(tmp_path)
    assert captured['env']['EXPERIMENT'] == 'submitted'
