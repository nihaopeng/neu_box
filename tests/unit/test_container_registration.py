"""容器归属登记：登记事务本身、OCI runtime hook 端点，以及 start 借条。

端点行为按 ``docs/container-registration.md`` 校验：状态码、错误码、幂等，
以及「读沙盒状态」和「写登记」在同一个 ``lifecycle_lock`` 临界区里。
"""

import os
import threading
import time

import pytest
from flask import Flask

from neu_box.api.containers import container_bp
from neu_box.runtime import containers
from neu_box.runtime.container_intents import START_INTENT_TTL, StartIntentStore
from neu_box.runtime.containers import ContainerIdentity, DockerExecutorError
from neu_box.runtime.sandbox import SbxManager

SANDBOX = 'sbx_user_task-1.slice'
CONTAINER_ID = 'a' * 64


def _identity(namespace=42, container_id='cid', host_pid=123, cgroup='/docker/cid'):
    return ContainerIdentity(
        container_ref=container_id,
        container_id=container_id,
        init_host_pid=host_pid,
        init_start_time=1,
        mount_namespace=namespace,
        container_cgroup=cgroup,
        started_at='',
    )


class _DB:
    """最小 DB 替身：只实现登记路径读的几个方法。"""

    def __init__(self, sandboxes=None, containers=None):
        self.sandboxes = {
            name: dict(record) for name, record in (sandboxes or {}).items()
        }
        self.containers = dict(containers or {})
        self.activations = []

    def get_sandbox(self, name):
        return self.sandboxes.get(name)

    def get_container(self, mount_namespace):
        return self.containers.get(int(mount_namespace))

    def list_containers(self):
        return list(self.containers.values())

    def delete_container(self, mount_namespace):
        self.containers.pop(int(mount_namespace), None)

    def activate_sandbox(self, name, pids):
        """对齐 ``storage.activate_sandbox``：只有 CREATING 那一行能改。"""
        self.activations.append((name, list(pids)))
        record = self.sandboxes.get(name)
        if record is None or record.get('state') != 'CREATING':
            return False
        record['state'] = 'ACTIVE'
        record['pids'] = list(pids)
        return True


def _manager(sandboxes=None, containers=None):
    """真 ``register_runtime_container`` + 假 DB，只把 native 写入换掉。"""
    manager = SbxManager.__new__(SbxManager)
    manager.db = _DB(sandboxes=sandboxes, containers=containers)
    manager.lock = threading.RLock()
    manager._container_registration_lock = threading.RLock()
    manager.unbound = []
    manager.unbind_container = lambda namespace: manager.unbound.append(namespace)
    manager._close_container_handles = lambda _namespace: None
    manager._container_alive = lambda _record: False
    manager._container_fds = {}

    def register_container(sandbox_name, identity):
        record = {
            'sandbox_name': sandbox_name,
            'container_ref': identity.container_ref,
            'container_id': identity.container_id,
            'init_host_pid': identity.init_host_pid,
            'init_start_time': identity.init_start_time,
            'mount_namespace': identity.mount_namespace,
            'state': 'ACTIVE',
        }
        manager.db.containers[int(identity.mount_namespace)] = record
        return record

    manager.register_container = register_container
    return manager


def _active_sandbox(name=SANDBOX):
    return {name: {'name': name, 'state': 'ACTIVE'}}


def _creating_sandbox(name=SANDBOX, pids=()):
    """Docker 命令任务的常态：容器不 join，沙盒一直停在 CREATING。"""
    return {name: {'name': name, 'state': 'CREATING', 'pids': list(pids)}}


def _exploding_identity(*_args, **_kwargs):
    raise AssertionError('this request must be rejected before resolving identity')


def _never_allocated_pid() -> int:
    """永远不可能存在的 PID：内核不会分配超过 pid_max 的号。"""
    with open('/proc/sys/kernel/pid_max', encoding='utf-8') as stream:
        return int(stream.read().strip()) + 1


@pytest.fixture
def runtime_http():
    """真 Flask 路由；管理器与身份由各测试自己注入。"""
    app = Flask(__name__)
    app.register_blueprint(container_bp, url_prefix='/container')
    return app.test_client()


def _install(monkeypatch, manager, identity=None):
    """注入管理器；``identity`` 可以是身份对象，也可以是替身函数。"""
    monkeypatch.setattr(
        SbxManager, 'get_instance', classmethod(lambda cls: manager))
    if identity is not None:
        factory = identity if callable(identity) else (lambda *a, **k: identity)
        monkeypatch.setattr(
            'neu_box.api.containers.runtime_container_identity', factory)


def _register(client, **overrides):
    body = {
        'container_id': 'cid',
        'host_pid': 123,
        'sandbox_cgroup': SANDBOX,
    }
    body.update(overrides)
    return client.post('/container/register', json=body)


@pytest.fixture
def intents(monkeypatch):
    """每个用例一份干净的借条簿（进程内单例，不隔离会串）。"""
    store = StartIntentStore(ttl_seconds=START_INTENT_TTL)
    monkeypatch.setattr(
        StartIntentStore, 'get_instance', classmethod(lambda cls: store))
    return store


def _lend_request(client, **overrides):
    body = {
        'username': 'user',
        'container_id': CONTAINER_ID,
        'pid': 4242,
    }
    body.update(overrides)
    return client.post('/container/intent', json=body)


def _patch_caller(monkeypatch, *, sandbox=SANDBOX, owner_ok=True):
    """假掉"这个 pid 属于谁、在哪个沙盒里" —— 真实现读 /proc。"""
    monkeypatch.setattr(
        'neu_box.api.containers._verify_pid_owner', lambda pid, user: owner_ok)
    monkeypatch.setattr(
        'neu_box.api.containers._find_sandbox_for_pid', lambda pid: sandbox)


# ── 登记事务（SbxManager 层） ───────────────────────────────────


def test_register_container_if_free_serializes_check_and_insert():
    class DB:
        def __init__(self):
            self.record = None

        def get_container(self, _namespace):
            return self.record

        def get_sandbox(self, _name):
            return {'state': 'ACTIVE'}

    manager = SbxManager.__new__(SbxManager)
    manager.db = DB()
    manager.lock = threading.RLock()
    manager._container_registration_lock = threading.RLock()
    entered = threading.Barrier(2)
    results = []

    def register(name, identity):
        record = {'sandbox_name': name, 'mount_namespace': identity.mount_namespace}
        manager.db.record = record
        return record

    manager.register_container = register

    def worker():
        entered.wait()
        try:
            results.append(manager.register_container_if_free('sbx_u_1.slice', _identity()))
        except DockerExecutorError as exc:
            results.append(exc.code)

    # Start both callers together; only one may pass the check-and-insert
    # transaction while the other observes the first row.
    first = threading.Thread(target=worker)
    second = threading.Thread(target=worker)
    first.start()
    second.start()
    first.join()
    second.join()

    assert len([result for result in results if isinstance(result, dict)]) == 1
    assert 'docker_container_already_registered' in results


def test_release_container_expected_identity_does_not_delete_replacement():
    class DB:
        def get_container(self, _namespace):
            return {
                'mount_namespace': 42,
                'sandbox_name': 'sbx_other_2.slice',
                'container_id': 'new-container',
            }

    manager = SbxManager.__new__(SbxManager)
    manager.db = DB()
    manager.lock = threading.RLock()
    manager._container_registration_lock = threading.RLock()
    manager.unbind_container = lambda _namespace: pytest.fail('unexpected unbind')
    manager._close_container_handles = lambda _namespace: pytest.fail('unexpected close')

    manager.release_container(
        42,
        expected_sandbox_name='sbx_user_1.slice',
        expected_container_id='old-container',
    )


def test_release_container_expected_pid_does_not_unbind_a_newer_run():
    class DB:
        def get_container(self, _namespace):
            return {
                'mount_namespace': 42,
                'sandbox_name': SANDBOX,
                'container_id': 'cid',
                'init_host_pid': 777,
                'init_start_time': 1,
            }

    manager = SbxManager.__new__(SbxManager)
    manager.db = DB()
    manager.lock = threading.RLock()
    manager._container_registration_lock = threading.RLock()
    manager.unbind_container = lambda _namespace: pytest.fail('unexpected unbind')
    manager._close_container_handles = lambda _namespace: pytest.fail('unexpected close')

    manager.release_container(
        42,
        expected_sandbox_name=SANDBOX,
        expected_container_id='cid',
        expected_init_host_pid=123,
        expected_init_start_time=1,
    )


def test_register_container_if_free_rejects_destroying_sandbox():
    class DB:
        def get_sandbox(self, _name):
            return {'state': 'DESTROYING'}

        def get_container(self, _namespace):
            return None

    manager = SbxManager.__new__(SbxManager)
    manager.db = DB()
    manager.lock = threading.RLock()
    manager._container_registration_lock = threading.RLock()
    manager.register_container = lambda *_args: pytest.fail('must not register')

    with pytest.raises(DockerExecutorError) as error:
        manager.register_container_if_free('sbx_user_1.slice', _identity())
    assert error.value.code == 'sandbox_not_active'


def test_register_runtime_container_is_idempotent_per_identity():
    manager = _manager(sandboxes=_active_sandbox())

    first, created = manager.register_runtime_container(SANDBOX, _identity())
    second, created_again = manager.register_runtime_container(SANDBOX, _identity())

    assert created is True
    assert created_again is False
    assert first == second == manager.db.containers[42]


def test_register_runtime_container_promotes_a_creating_sandbox():
    """登记即 join：容器不进 cgroup，登记就是它加入沙盒的那一步。"""
    manager = _manager(sandboxes=_creating_sandbox())

    record, created = manager.register_runtime_container(SANDBOX, _identity())

    assert created is True
    assert record == manager.db.containers[42]
    assert manager.db.get_sandbox(SANDBOX)['state'] == 'ACTIVE'
    assert manager.db.activations == [(SANDBOX, [])]


def test_register_runtime_container_keeps_the_container_pid_out_of_the_pids():
    """pids 是"谁在沙盒 cgroup 里"的记录，容器不在里面。"""
    manager = _manager(sandboxes=_creating_sandbox(pids=[4242]))

    manager.register_runtime_container(SANDBOX, _identity(host_pid=9999))

    assert manager.db.get_sandbox(SANDBOX)['pids'] == [4242]


def test_register_runtime_container_leaves_an_active_sandbox_alone():
    manager = _manager(sandboxes=_active_sandbox())

    record, created = manager.register_runtime_container(SANDBOX, _identity())

    assert created is True
    assert manager.db.get_sandbox(SANDBOX)['state'] == 'ACTIVE'
    assert manager.db.activations == []


def test_register_runtime_container_activates_only_once():
    """幂等重试不该再推一次状态（第二发看到的已经是 ACTIVE）。"""
    manager = _manager(sandboxes=_creating_sandbox())

    manager.register_runtime_container(SANDBOX, _identity())
    _, created = manager.register_runtime_container(SANDBOX, _identity())

    assert created is False
    assert manager.db.activations == [(SANDBOX, [])]


def test_register_runtime_container_rejects_sandbox_being_destroyed():
    manager = _manager(
        sandboxes={SANDBOX: {'name': SANDBOX, 'state': 'DESTROYING'}})

    with pytest.raises(DockerExecutorError) as error:
        manager.register_runtime_container(SANDBOX, _identity())
    assert error.value.code == 'sandbox_not_active'
    assert manager.db.containers == {}
    assert manager.db.activations == []


def test_register_runtime_container_rejects_namespace_of_another_container():
    other = 'sbx_other_task-9.slice'
    manager = _manager(
        sandboxes={**_active_sandbox(), other: {'name': other, 'state': 'ACTIVE'}},
        containers={42: {'sandbox_name': other, 'container_id': 'other-cid'}},
    )

    with pytest.raises(DockerExecutorError) as error:
        manager.register_runtime_container(SANDBOX, _identity())
    assert error.value.code == 'docker_container_registered_elsewhere'


def test_register_runtime_container_keeps_an_existing_binding():
    """同一个容器重复登记：以既有绑定为准，不跟着这次报上来的沙盒改。

    hook 重试可能带着建容器时那行 annotation 再登记一次，而按借条改绑过的
    容器可能已经绑定别的沙盒；重复登记不能搬走授权。
    """
    other = 'sbx_other_task-9.slice'
    manager = _manager(
        sandboxes={**_active_sandbox(),
                   other: {'name': other, 'state': 'ACTIVE'}},
        containers={42: {'sandbox_name': other, 'container_id': 'cid'}},
    )

    record, created = manager.register_runtime_container(SANDBOX, _identity())

    assert created is False
    assert record['sandbox_name'] == other
    assert manager.db.get_container(42)['sandbox_name'] == other


# ── 端点 ────────────────────────────────────────────────────────


def test_endpoint_registers_then_answers_already_registered(runtime_http, monkeypatch):
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager, _identity(namespace=9001, cgroup='/docker/abc'))

    first = _register(runtime_http, container_id='cid', host_pid=4242)
    assert first.status_code == 201
    assert first.get_json() == {
        'sandbox_name': SANDBOX,
        'container_id': 'cid',
        'mount_namespace': 9001,
        'container_cgroup': '/docker/abc',
        'status': 'registered',
    }

    again = _register(runtime_http, container_id='cid', host_pid=4242)
    assert again.status_code == 200
    assert again.get_json()['status'] == 'already_registered'


def test_endpoint_reports_unknown_sandbox(runtime_http, monkeypatch):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()), _exploding_identity)

    response = _register(runtime_http, sandbox_cgroup='sbx_someone_else_1.slice')
    assert response.status_code == 404
    assert response.get_json()['code'] == 'sandbox_not_found'


def test_endpoint_promotes_a_creating_sandbox(runtime_http, monkeypatch):
    """docker 命令任务的沙盒没人 join，端点这一层必须也能把它推成 ACTIVE。"""
    manager = _manager(sandboxes=_creating_sandbox())
    _install(monkeypatch, manager, _identity(namespace=9001, cgroup='/docker/abc'))

    response = _register(runtime_http, container_id='cid', host_pid=4242)

    assert response.status_code == 201
    assert response.get_json()['status'] == 'registered'
    assert manager.db.get_sandbox(SANDBOX)['state'] == 'ACTIVE'


def test_endpoint_reports_inactive_sandbox(runtime_http, monkeypatch):
    manager = _manager(
        sandboxes={SANDBOX: {'name': SANDBOX, 'state': 'DESTROYING'}})
    _install(monkeypatch, manager, _identity())

    response = _register(runtime_http)
    assert response.status_code == 409
    assert response.get_json()['code'] == 'sandbox_not_active'
    assert manager.db.containers == {}
    assert manager.db.activations == []


def test_endpoint_reports_namespace_registered_elsewhere(runtime_http, monkeypatch):
    manager = _manager(
        sandboxes=_active_sandbox(),
        containers={42: {'sandbox_name': 'sbx_other_task-9.slice',
                         'container_id': 'other-cid'}},
    )
    _install(monkeypatch, manager, _identity(namespace=42))

    response = _register(runtime_http)
    assert response.status_code == 409
    assert response.get_json()['code'] == 'docker_container_registered_elsewhere'


@pytest.mark.parametrize('body,expected', [
    ({}, 400),
    ({'container_id': 'cid'}, 400),
    ({'container_id': 'cid', 'sandbox_cgroup': SANDBOX}, 400),
    ({'container_id': '', 'host_pid': 1, 'sandbox_cgroup': SANDBOX}, 400),
    ({'container_id': 'cid', 'host_pid': 'abc', 'sandbox_cgroup': SANDBOX}, 400),
    ({'container_id': 'cid', 'host_pid': 0, 'sandbox_cgroup': SANDBOX}, 400),
    ({'container_id': 'cid', 'host_pid': -3, 'sandbox_cgroup': SANDBOX}, 400),
    # 参数就不合法时连身份都不该去解析（_exploding_identity 会炸）。
    ({'container_id': 'cid', 'host_pid': 1,
      'sandbox_cgroup': 'sbx_unknown_1.slice'}, 404),
])
def test_endpoint_rejects_bad_requests_before_resolving_identity(
        runtime_http, monkeypatch, body, expected):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()), _exploding_identity)

    response = runtime_http.post('/container/register', json=body)
    assert response.status_code == expected


@pytest.mark.parametrize('overrides,code', [
    ({'container_cgroup': '/docker/other'}, 'runtime_identity_changed'),
    ({'mount_namespace': 12345}, 'runtime_identity_changed'),
    ({'mount_namespace': 'not-an-inum'}, 'runtime_identity_changed'),
])
def test_endpoint_rejects_inconsistent_optional_observations(
        runtime_http, monkeypatch, overrides, code):
    _install(
        monkeypatch,
        _manager(sandboxes=_active_sandbox()),
        _identity(namespace=9001, cgroup='/docker/abc'),
    )

    response = _register(runtime_http, **overrides)
    assert response.status_code == 409
    assert response.get_json()['code'] == code


def test_endpoint_accepts_matching_optional_observations(runtime_http, monkeypatch):
    _install(
        monkeypatch,
        _manager(sandboxes=_active_sandbox()),
        _identity(namespace=9001, cgroup='/docker/abc'),
    )

    response = _register(
        runtime_http, container_cgroup='/docker/abc', mount_namespace=9001)
    assert response.status_code == 201
    assert response.get_json()['status'] == 'registered'


# ── 用真的 runtime_container_identity（只读 /proc） ──────────────


def test_endpoint_rejects_pid_sharing_the_host_mount_namespace(runtime_http, monkeypatch):
    """宿主机自己绝不能登记成受托方 —— 这条用真身份解析验证。"""
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))

    response = _register(runtime_http, container_id='host', host_pid=os.getpid())
    assert response.status_code == 409
    assert response.get_json()['code'] == 'docker_container_same_mount_namespace'


def test_endpoint_reports_dead_host_pid(runtime_http, monkeypatch):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))

    response = _register(
        runtime_http, container_id='gone', host_pid=_never_allocated_pid())
    assert response.status_code == 409
    assert response.get_json()['code'] == 'docker_container_pid_invalid'


def test_endpoint_resolves_identity_without_docker(runtime_http, monkeypatch):
    """契约第 1 条：只读 /proc，不查 Docker（这里把 docker_client 换成炸弹）。"""
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    monkeypatch.setattr(
        containers, 'docker_client',
        lambda *args, **kwargs: pytest.fail('registration must not query Docker'))
    # 假 /proc：真 runtime_container_identity 跑完整条路径。
    monkeypatch.setattr(
        containers, 'namespace_inode',
        lambda pid, namespace: 0x1000 + int(pid))
    monkeypatch.setattr(
        containers, 'read_unified_cgroup', lambda pid: f'/docker/{int(pid)}')
    monkeypatch.setattr(containers, 'process_start_time', lambda pid: 7)

    response = _register(runtime_http, container_id='oci-1', host_pid=os.getppid())
    assert response.status_code == 201
    body = response.get_json()
    assert body['container_id'] == 'oci-1'
    assert body['mount_namespace'] == 0x1000 + os.getppid()
    assert body['container_cgroup'] == f'/docker/{os.getppid()}'
    assert body['status'] == 'registered'


# ── 契约第 3 条：状态检查与写登记在同一个临界区里 ────────────────


def test_endpoint_registers_inside_the_lifecycle_lock(runtime_http, monkeypatch):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()), _identity())
    results = []
    thread = threading.Thread(target=lambda: results.append(_register(runtime_http)))

    with SbxManager.lifecycle_lock():
        thread.start()
        # 端点只做内存操作：没被锁挡住的话这点时间早就返回了。
        time.sleep(0.2)
        blocked = results == []
    thread.join(timeout=5)

    assert blocked, 'registration did not wait for the lifecycle lock'
    assert results[0].status_code == 201


# ── start 借条：`neubox docker start` 的两段式 ──────────────────


def test_intent_endpoint_records_the_callers_sandbox(
        runtime_http, monkeypatch, intents):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http)

    assert response.status_code == 200
    body = response.get_json()
    assert body['sandbox_name'] == SANDBOX
    assert body['state'] == 'pending'
    assert intents.peek(CONTAINER_ID, 'user')['sandbox_name'] == SANDBOX


def test_two_cli_processes_cannot_replace_a_pending_start(
        runtime_http, monkeypatch, intents):
    other = 'sbx_user_task-2.slice'
    manager = _manager(sandboxes={**_active_sandbox(),
                                  other: {'name': other, 'state': 'ACTIVE'}})
    _install(monkeypatch, manager)
    monkeypatch.setattr('neu_box.api.containers._verify_pid_owner',
                        lambda pid, user: True)
    monkeypatch.setattr('neu_box.api.containers._find_sandbox_for_pid',
                        lambda pid: SANDBOX if pid == 4242 else other)

    first = _lend_request(runtime_http, pid=4242)
    second = _lend_request(runtime_http, pid=4343)

    assert first.status_code == 200
    assert second.status_code == 409
    assert second.get_json()['code'] == 'start_intent_busy'
    assert intents.peek(CONTAINER_ID, 'user')['sandbox_name'] == SANDBOX
    assert intents.peek(CONTAINER_ID, 'user')['borrower_pid'] == 4242


def test_acknowledged_short_run_allows_next_lend_after_unregistration(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager)
    _patch_caller(monkeypatch)
    query = {'container_id': CONTAINER_ID, 'username': 'user', 'pid': 4242}

    assert _lend_request(runtime_http).status_code == 200
    claim = intents.claim(CONTAINER_ID, 'user')
    assert intents.complete(CONTAINER_ID, 'user', claim['sequence'])
    assert _lend_request(runtime_http, pid=4343).get_json()['code'] == 'start_intent_busy'

    # A second CLI's GET cannot acknowledge the first CLI's consumed result.
    other_query = dict(query, pid=4343)
    assert runtime_http.get('/container/intent', query_string=other_query).get_json()['state'] == 'consumed'
    assert intents.peek(CONTAINER_ID, 'user')['acknowledged_at'] is None
    assert runtime_http.get('/container/intent', query_string=query).get_json()['state'] == 'consumed'
    assert intents.peek(CONTAINER_ID, 'user')['acknowledged_at'] is not None

    manager.db.containers[42] = {
        'container_id': CONTAINER_ID,
        'mount_namespace': 42,
        'init_host_pid': 123,
    }
    manager._container_alive = lambda _record: True
    still_running = _lend_request(runtime_http, pid=4343)
    assert still_running.status_code == 409
    assert still_running.get_json()['code'] == 'container_still_bound'

    manager.db.delete_container(42)
    next_run = _lend_request(runtime_http, pid=4343)
    assert next_run.status_code == 200
    assert intents.peek(CONTAINER_ID, 'user')['borrower_pid'] == 4343


def test_lend_retires_dead_registration_before_issuing_intent(
        runtime_http, monkeypatch, intents):
    record = {
        'container_id': CONTAINER_ID,
        'mount_namespace': 42,
        'init_host_pid': 123,
        'init_start_time': 1,
    }
    manager = _manager(sandboxes=_active_sandbox(), containers={42: record})
    manager._container_fds[42] = (0, 0)  # pidfd exists; dead result is definitive.
    _install(monkeypatch, manager)
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http)

    assert response.status_code == 200
    assert manager.unbound == [42]
    assert manager.db.get_container(42) is None
    assert intents.peek(CONTAINER_ID, 'user')['sandbox_name'] == SANDBOX


def test_lend_refuses_intent_if_old_authorization_cannot_be_revoked(
        runtime_http, monkeypatch, intents):
    record = {
        'container_id': CONTAINER_ID,
        'mount_namespace': 42,
        'init_host_pid': 123,
        'init_start_time': 1,
    }
    manager = _manager(sandboxes=_active_sandbox(), containers={42: record})
    manager._container_fds[42] = (0, 0)

    def failed_unbind(_namespace):
        raise OSError('BPF map unavailable')

    manager.unbind_container = failed_unbind
    _install(monkeypatch, manager)
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'container_binding_unknown'
    assert manager.db.get_container(42) is not None
    assert intents.peek(CONTAINER_ID, 'user') is None


def test_lend_fails_closed_when_old_registration_liveness_is_unknown(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox(), containers={
        42: {'container_id': CONTAINER_ID, 'mount_namespace': 42},
    })

    def unknown(_record):
        raise OSError('pidfd unavailable')

    manager._container_alive = unknown
    _install(monkeypatch, manager)
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'container_binding_unknown'
    assert intents.peek(CONTAINER_ID, 'user') is None


def test_lend_rejects_unreadable_live_pid_after_worker_restart(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox(), containers={
        42: {'container_id': CONTAINER_ID, 'mount_namespace': 42,
             'init_host_pid': os.getpid(), 'init_start_time': 1},
    })
    _install(monkeypatch, manager)
    _patch_caller(monkeypatch)

    def cannot_read(_pid):
        raise DockerExecutorError('proc permission denied', 'docker_container_pid_invalid')

    monkeypatch.setattr('neu_box.api.containers.process_start_time', cannot_read)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'container_binding_unknown'
    assert intents.peek(CONTAINER_ID, 'user') is None


def test_intent_beats_a_stale_annotation(runtime_http, monkeypatch, intents):
    """借条指到的沙盒才是这次 start 该用的那个 —— annotation 已经指向死沙盒。"""
    other = 'sbx_lent_task-7.slice'
    manager = _manager(sandboxes={**_active_sandbox(other)})
    _install(monkeypatch, manager, _identity(namespace=9001, cgroup='/docker/abc'))
    _patch_caller(monkeypatch, sandbox=other)
    intents.lend('cid', 'user', other)

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 201
    assert response.get_json()['sandbox_name'] == other
    assert manager.db.get_container(9001)['sandbox_name'] == other
    assert intents.peek('cid', 'user')['consumed_at'] is not None

    # 同一 OCI 运行实例重试，已经消费的借条只能幂等确认原登记。
    again = _register(runtime_http, container_id='cid')
    assert again.status_code == 200
    assert again.get_json()['status'] == 'already_registered'


def test_intent_only_lends_to_the_containers_own_owner(
        runtime_http, monkeypatch, intents):
    """跨属主借条必须阻断旧 annotation，不能回退错绑。"""
    other = 'sbx_lent_task-7.slice'
    manager = _manager(sandboxes={**_active_sandbox(), **_active_sandbox(other)})
    _install(monkeypatch, manager, _exploding_identity)
    intents.lend('cid', 'someone-else', other)

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'start_intent_owner_mismatch'
    assert manager.db.containers == {}
    assert intents.peek('cid', 'someone-else')['consumed_at'] is None


def test_intent_endpoint_rejects_a_stale_lent_sandbox(
        runtime_http, monkeypatch, intents):
    """借条里的沙盒已经没了：不能静默绑定到旧 annotation。"""
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()), _identity())
    _patch_caller(monkeypatch, sandbox='sbx_lent_task-7.slice')
    intents.lend('cid', 'user', 'sbx_lent_task-7.slice')

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'sandbox_not_active'
    assert intents.peek('cid', 'user')['consumed_at'] is None


def test_intent_is_not_confirmed_when_registration_fails(
        runtime_http, monkeypatch, intents):
    """身份检查失败后可重试同一借条，不会误报 consumed。"""
    calls = 0

    def identity_after_retry(*_args, **_kwargs):
        nonlocal calls
        calls += 1
        if calls == 1:
            raise DockerExecutorError('容器身份无效', 'docker_container_pid_invalid')
        return _identity()

    _install(monkeypatch, _manager(sandboxes=_active_sandbox()), identity_after_retry)
    intents.lend('cid', 'user', SANDBOX)

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    entry = intents.peek('cid', 'user')
    assert entry['claimed_at'] is None
    assert entry['consumed_at'] is None
    assert _register(runtime_http, container_id='cid').status_code == 201
    assert intents.peek('cid', 'user')['consumed_at'] is not None


def test_expired_and_replaced_intent_rolls_back_only_this_new_registration(
        runtime_http, monkeypatch, intents):
    other = 'sbx_user_task-2.slice'
    manager = _manager(sandboxes={**_active_sandbox(),
                                  other: {'name': other, 'state': 'ACTIVE'}})
    _install(monkeypatch, manager, _identity())
    clock = [100.0]
    intents._clock = lambda: clock[0]
    intents._ttl = 1
    intents.lend('cid', 'user', SANDBOX)
    register = manager.register_container

    def register_then_replace(name, identity):
        record = register(name, identity)
        clock[0] += 2
        assert intents.lend('cid', 'user', other) is not None
        return record

    manager.register_container = register_then_replace

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'start_intent_changed'
    assert manager.db.get_container(42) is None
    assert manager.unbound == [42]
    assert intents.peek('cid', 'user')['sandbox_name'] == other
    assert intents.peek('cid', 'user')['claimed_at'] is None

    manager.register_container = register
    retry = _register(runtime_http, container_id='cid')
    assert retry.status_code == 201
    assert retry.get_json()['sandbox_name'] == other


def test_expired_intent_rolls_back_new_registration(
        runtime_http, monkeypatch):
    clock = [100.0]
    store = StartIntentStore(ttl_seconds=1, clock=lambda: clock[0])
    monkeypatch.setattr(StartIntentStore, 'get_instance',
                        classmethod(lambda cls: store))
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager, _identity())
    store.lend('cid', 'user', SANDBOX)
    register = manager.register_container

    def register_then_expire(name, identity):
        record = register(name, identity)
        clock[0] += 2
        return record

    manager.register_container = register_then_expire

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'start_intent_changed'
    assert manager.db.get_container(42) is None
    assert manager.unbound == [42]


def test_changed_intent_never_revokes_an_existing_registration(
        runtime_http, monkeypatch, intents):
    existing = {
        'sandbox_name': SANDBOX,
        'container_id': 'cid',
        'mount_namespace': 42,
        'init_host_pid': 123,
        'init_start_time': 1,
        'state': 'ACTIVE',
    }
    manager = _manager(sandboxes=_active_sandbox(), containers={42: existing})
    _install(monkeypatch, manager, _identity())
    clock = [100.0]
    intents._clock = lambda: clock[0]
    intents._ttl = 1
    intents.lend('cid', 'user', SANDBOX)
    register = manager.register_runtime_container

    def repeat_then_replace(name, identity):
        result = register(name, identity)
        clock[0] += 2
        assert intents.lend('cid', 'user', 'sbx_user_task-2.slice') is not None
        return result

    manager.register_runtime_container = repeat_then_replace

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'start_intent_changed'
    assert manager.db.get_container(42) is existing
    assert manager.unbound == []


def test_rollback_does_not_unbind_a_replacement_identity(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager, _identity())
    intents.lend('cid', 'user', SANDBOX)
    complete = intents.complete
    replacement = {
        'sandbox_name': SANDBOX,
        'container_id': 'different-container',
        'mount_namespace': 42,
        'init_host_pid': 777,
        'init_start_time': 999,
        'state': 'ACTIVE',
    }

    def replace_before_rollback(*args):
        manager.db.containers[42] = replacement
        return False

    monkeypatch.setattr(intents, 'complete', replace_before_rollback)

    response = _register(runtime_http, container_id='cid')

    assert response.status_code == 409
    assert response.get_json()['code'] == 'start_intent_changed'
    assert manager.db.get_container(42) is replacement
    assert manager.unbound == []
    monkeypatch.setattr(intents, 'complete', complete)


def test_busy_intent_blocks_annotation_fallback_then_allows_retry(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager, _identity())
    intents.lend('cid', 'user', SANDBOX)
    claim = intents.claim('cid', 'user')

    busy = _register(runtime_http, container_id='cid')

    assert busy.status_code == 409
    assert busy.get_json()['code'] == 'start_intent_busy'
    assert manager.db.containers == {}
    assert intents.abort('cid', 'user', claim['sequence'])
    assert _register(runtime_http, container_id='cid').status_code == 201


def test_consumed_intent_rejects_a_different_container_run(
        runtime_http, monkeypatch, intents):
    manager = _manager(sandboxes=_active_sandbox())
    _install(monkeypatch, manager, _identity())
    intents.lend('cid', 'user', SANDBOX)
    assert _register(runtime_http, container_id='cid').status_code == 201

    _install(monkeypatch, manager, _identity(host_pid=777))
    retry = _register(runtime_http, container_id='cid', host_pid=777)

    assert retry.status_code == 409
    assert retry.get_json()['code'] == 'start_intent_changed'
    assert manager.db.get_container(42)['init_host_pid'] == 123


def test_intent_state_endpoint_reports_consumption(
        runtime_http, monkeypatch, intents):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    _patch_caller(monkeypatch)
    query = {'container_id': CONTAINER_ID, 'username': 'user', 'pid': 4242}

    empty = runtime_http.get('/container/intent', query_string=query)
    assert empty.status_code == 200
    assert empty.get_json()['state'] is None

    _lend_request(runtime_http)
    pending = runtime_http.get('/container/intent', query_string=query)
    assert pending.get_json()['state'] == 'pending'

    claim = intents.claim(CONTAINER_ID, 'user')
    still_pending = runtime_http.get('/container/intent', query_string=query)
    assert still_pending.get_json()['state'] == 'pending'
    assert intents.complete(CONTAINER_ID, 'user', claim['sequence'])
    consumed = runtime_http.get('/container/intent', query_string=query)
    assert consumed.get_json()['state'] == 'consumed'
    assert consumed.get_json()['sandbox_name'] == SANDBOX


def test_intent_endpoint_rejects_a_pid_outside_a_sandbox(
        runtime_http, monkeypatch, intents):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    _patch_caller(monkeypatch, sandbox=None)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'not_in_sandbox'
    assert intents.peek(CONTAINER_ID, 'user') is None


def test_intent_endpoint_rejects_a_pid_of_another_user(
        runtime_http, monkeypatch, intents):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    _patch_caller(monkeypatch, owner_ok=False)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'pid_owner_mismatch'
    assert intents.peek(CONTAINER_ID, 'user') is None


def test_intent_endpoint_rejects_a_sandbox_of_another_user(
        runtime_http, monkeypatch, intents):
    other = 'sbx_other_task-7.slice'
    _install(monkeypatch, _manager(sandboxes={**_active_sandbox(other)}))
    _patch_caller(monkeypatch, sandbox=other)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'sandbox_owner_mismatch'


def test_intent_endpoint_rejects_an_unavailable_sandbox(
        runtime_http, monkeypatch, intents):
    _install(monkeypatch, _manager(
        sandboxes={SANDBOX: {'name': SANDBOX, 'state': 'DESTROYING'}}))
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http)

    assert response.status_code == 409
    assert response.get_json()['code'] == 'sandbox_not_active'


@pytest.mark.parametrize('overrides,expected', [
    ({'container_id': ''}, 400),
    ({'container_id': 'not-a-docker-id'}, 400),
    ({'username': ''}, 400),
    ({'pid': 'abc'}, 400),
    ({'pid': 0}, 400),
])
def test_intent_endpoint_rejects_bad_requests(
        runtime_http, monkeypatch, intents, overrides, expected):
    _install(monkeypatch, _manager(sandboxes=_active_sandbox()))
    _patch_caller(monkeypatch)

    response = _lend_request(runtime_http, **overrides)

    assert response.status_code == expected
