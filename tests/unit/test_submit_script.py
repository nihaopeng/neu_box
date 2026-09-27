"""Submitted scripts keep their bytes and their sandbox lifetime."""

import asyncio
import json
import os
import sqlite3
import sys
import threading
from types import SimpleNamespace

from flask import Flask

from neu_box.api import tasks as task_api
from neu_box.execution import logs
from neu_box.execution import host as host_execution
from neu_box.execution.host import _execute_in_sandbox, HostCommandExecutor
from neu_box.migrations.engine import migrate_database
from neu_box.runtime.sandbox import SbxManager
from neu_box.scheduling import tasks as task_shape
from neu_box.scheduling.queue import TaskQueue
from neu_box.storage import (
    Database, MIGRATIONS_PACKAGE, REQUIRED_COLUMNS, REQUIRED_INDEXES,
)


def test_submit_script_api_preserves_original_text(monkeypatch):
    captured = {}

    class Queue:
        def submit(self, *args, **kwargs):
            captured['args'] = args
            captured['kwargs'] = kwargs
            return 'task-1'

        def position(self, task_id):
            return 1

    class Db:
        def get_task(self, task_id):
            return {'priority': 0}

    monkeypatch.setattr(task_api.TaskQueue, 'get_instance', lambda: Queue())
    monkeypatch.setattr(task_api.Database, 'get_instance', lambda: Db())
    monkeypatch.setattr(task_api.SbxManager, 'get_instance', lambda: object())
    monkeypatch.setattr(task_api.devices, 'discover_nodes', lambda: [])
    script = "set -e\ncat <<'SH'\n  exact indentation\nSH\n\n"
    application = Flask(__name__)
    application.register_blueprint(task_api.command_bp, url_prefix='/tasks')

    response = application.test_client().post(
        '/tasks', json={'user_id': _current_user(),
                        'script': script},
    )

    assert response.status_code == 202
    assert captured['args'][1] == script
    assert captured['kwargs']['command_mode'] == 'script'
    assert captured['kwargs']['command_argv'] is None
    duplicate = application.test_client().post(
        '/tasks', json={'user_id': _current_user(), 'script': script,
                        'command': 'echo duplicate'},
    )
    assert duplicate.status_code == 400
    malformed_shape = application.test_client().post(
        '/tasks', json=['script'],
    )
    assert malformed_shape.status_code == 400

    argv = ['neubox', 'docker', 'run', '--rm', 'training:latest',
            'python', 'train.py']
    response = application.test_client().post(
        '/tasks', json={'user_id': _current_user(), 'command_argv': argv},
    )
    assert response.status_code == 202
    assert captured['kwargs']['command_mode'] == 'argv'
    assert captured['kwargs']['command_argv'] == argv


def _current_user():
    import pwd
    return pwd.getpwuid(os.getuid()).pw_name


def test_task_database_recovers_script_and_argv_modes(tmp_path):
    path = tmp_path / 'worker.db'
    migrate_database(path, MIGRATIONS_PACKAGE, REQUIRED_COLUMNS,
                     REQUIRED_INDEXES)
    db = Database(str(path))
    script = "echo first\ncat <<'SH'\nsecond\nSH\n"
    argv = ['python', '-c', 'print("a b")', 'literal $HOME']
    db.insert_task('script-task', _current_user(), script,
                   command_mode='script')
    db.insert_task('argv-task', _current_user(), 'display only',
                   command_mode='argv', command_argv=argv)

    rows = {task['task_id']: task for task in db.get_queue_tasks()}
    assert rows['script-task']['command'] == script
    assert rows['script-task']['command_mode'] == 'script'
    assert rows['argv-task']['command_argv'] == argv
    assert rows['argv-task']['command_mode'] == 'argv'
    assert task_shape.public(rows['script-task'])['script'] == script
    assert task_shape.public(rows['argv-task'])['command_argv'] == argv


def test_upgrade_from_previous_schema_preserves_old_command_tasks(tmp_path):
    path = tmp_path / 'worker.db'
    migrate_database(path, MIGRATIONS_PACKAGE, REQUIRED_COLUMNS,
                     REQUIRED_INDEXES)
    # Restore the exact task-table shape and history from the previous
    # release, then upgrade a queued row through the public migrator.
    with sqlite3.connect(path) as connection:
        connection.execute('ALTER TABLE tasks DROP COLUMN command_argv')
        connection.execute('ALTER TABLE tasks DROP COLUMN command_mode')
        connection.execute('DELETE FROM schema_migrations WHERE version=8')
        connection.execute(
            'INSERT INTO tasks (task_id, user_id, command) VALUES (?, ?, ?)',
            ('legacy-task', _current_user(), 'echo old'),
        )
    migrate_database(path, MIGRATIONS_PACKAGE, REQUIRED_COLUMNS,
                     REQUIRED_INDEXES)

    legacy = Database(str(path)).get_task('legacy-task')
    assert legacy['command'] == 'echo old'
    assert legacy['command_mode'] == 'command'
    assert legacy['command_argv'] is None


def _run_host(monkeypatch, tmp_path, command, *, mode, argv=None):
    class Sandbox:
        joined = False

        def join_sandbox(self, name, pid):
            self.joined = True
            return True

    sandbox = Sandbox()
    monkeypatch.setattr(SbxManager, 'get_instance', lambda: sandbox)
    monkeypatch.setattr(logs, 'log_path', lambda task_id: str(tmp_path / 'task.log'))
    monkeypatch.setattr(host_execution.pwd, 'getpwnam', lambda username:
                        SimpleNamespace(pw_uid=os.geteuid(),
                                        pw_gid=os.getegid(),
                                        pw_dir=str(tmp_path)))
    bin_dir = tmp_path / 'bin'
    bin_dir.mkdir()
    wrapper = bin_dir / 'neubox'
    wrapper.write_text('#!/bin/sh\nprintf "wrapped:%s\\n" "$*"\n')
    wrapper.chmod(0o700)
    (tmp_path / '.bashrc').write_text(
        'export SCRIPT_INIT=ready\nexport PATH="$HOME/bin:$PATH"\n'
    )
    outcome = asyncio.run(_execute_in_sandbox(
        command, 'sbx_test.slice', timeout=10,
        username=_current_user(),
        target={'type': 'host', 'workdir': str(tmp_path)},
        command_mode=mode, command_argv=argv,
    ))
    assert sandbox.joined
    return outcome


def test_script_executes_heredoc_in_user_bash_environment(monkeypatch, tmp_path):
    script = (
        "set -e\n"
        'cd "$HOME"\n'
        "cat <<'DATA' > output.txt\n"
        "first line\n"
        "  indented line\n"
        "DATA\n"
        'printf "%s\\n" "$SCRIPT_INIT"\n'
        'neubox docker run --rm training:latest true\n'
    )

    outcome = _run_host(monkeypatch, tmp_path, script, mode='script')

    assert outcome['returncode'] == 0, outcome
    assert 'ready' in outcome['stdout']
    assert 'wrapped:docker run --rm training:latest true' in outcome['stdout']
    assert (tmp_path / 'output.txt').read_text() == (
        'first line\n  indented line\n'
    )


def test_argv_exec_does_not_reparse_shell_metacharacters(monkeypatch, tmp_path):
    argv = [
        sys.executable, '-c',
        'import json,sys; print(json.dumps(sys.argv[1:]))',
        'a b', '$(touch escaped)',
    ]

    outcome = _run_host(monkeypatch, tmp_path, '', mode='argv', argv=argv)

    assert outcome['returncode'] == 0, outcome
    assert json.dumps(['a b', '$(touch escaped)']) in outcome['stdout']
    assert not (tmp_path / 'escaped').exists()


def test_cancel_signals_script_process_group_before_sandbox_cleanup(monkeypatch):
    events = []

    class Sandbox:
        def destroy_sandbox(self, name):
            events.append(('cleanup', name))
            return True

    monkeypatch.setattr(SbxManager, 'get_instance', lambda: Sandbox())
    monkeypatch.setattr(host_execution.os, 'killpg', lambda pid, sig:
                        events.append(('killpg', pid, sig)))
    task = {
        'command': 'long_running_step\nnext_step\n',
        'command_mode': 'script', 'user_id': _current_user(),
    }
    executor = HostCommandExecutor(task=task, sandbox_name='sbx_test.slice')
    process = SimpleNamespace(pid=321, returncode=None)
    assert executor._on_spawn(process)

    executor.cancel()

    assert events == [
        ('killpg', 321, host_execution.signal.SIGKILL),
        ('cleanup', 'sbx_test.slice'),
    ]
    assert not executor._on_spawn(SimpleNamespace(pid=322, returncode=None))


def test_cancel_still_cleans_sandbox_when_process_signal_fails(monkeypatch):
    cleaned = []

    class Sandbox:
        def destroy_sandbox(self, name):
            cleaned.append(name)
            return True

    def denied_signal(_pid, _signal):
        raise PermissionError('signal denied')

    monkeypatch.setattr(SbxManager, 'get_instance', lambda: Sandbox())
    monkeypatch.setattr(host_execution.os, 'killpg', denied_signal)
    executor = HostCommandExecutor(task={}, sandbox_name='sbx_test.slice')
    assert executor._on_spawn(SimpleNamespace(pid=321, returncode=None))

    executor.cancel()

    assert cleaned == ['sbx_test.slice']


def test_script_entry_exit_cleans_sandbox_before_result_is_saved(monkeypatch):
    events = []

    class Executor:
        async def run(self, timeout):
            return {'returncode': 7, 'timed_out': False, 'error': None}

    class Sandbox:
        def destroy_sandbox(self, name):
            events.append(('cleanup', name))
            return True

    class Db:
        def update_task_result(self, task_id, status, returncode, *args):
            events.append(('result', status, returncode))

        def cleanup_old_tasks(self, keep):
            pass

        def cleanup_old_sessions(self, keep):
            pass

    monkeypatch.setattr(SbxManager, 'get_instance', lambda: Sandbox())
    queue = TaskQueue.__new__(TaskQueue)
    queue._lock = threading.RLock()
    queue._db = Db()
    queue._running = {}
    queue._maintenance_errors = {}
    task = {'task_id': 't1', 'user_id': 'alice', '_executor': Executor()}

    asyncio.run(queue._execute_one(task))

    assert events == [
        ('cleanup', 'sbx_alice_t1.slice'),
        ('result', 'failed', 7),
    ]


def test_cleanup_failure_keeps_business_exit_code(monkeypatch):
    saved = {}

    class Executor:
        async def run(self, timeout):
            return {'returncode': 0, 'timed_out': False, 'error': None}

    class Sandbox:
        def destroy_sandbox(self, name):
            return False

    class Db:
        def update_task_result(self, task_id, status, returncode, *args):
            saved.update(status=status, returncode=returncode,
                         error=args[3])

        def cleanup_old_tasks(self, keep):
            pass

        def cleanup_old_sessions(self, keep):
            pass

    monkeypatch.setattr(SbxManager, 'get_instance', lambda: Sandbox())
    queue = TaskQueue.__new__(TaskQueue)
    queue._lock = threading.RLock()
    queue._db = Db()
    queue._running = {}
    queue._maintenance_errors = {}
    task = {'task_id': 't1', 'user_id': 'alice', '_executor': Executor()}

    asyncio.run(queue._execute_one(task))

    assert saved == {
        'status': 'failed', 'returncode': 0,
        'error': 'sandbox_cleanup_failed',
    }

    task['_canceled'] = True
    asyncio.run(queue._execute_one(task))
    assert saved == {
        'status': 'cancelled', 'returncode': 0,
        'error': '用户手动取消; sandbox_cleanup_failed',
    }
