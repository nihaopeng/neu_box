"""容器归属登记端点。

路径按**资源**命名（``/container/register``），不按调用方 —— 调它的是节点上的
OCI runtime hook，但这里管的是容器，不是 runtime。

The hook calls it from Docker's create path.  It must never query Docker: doing
so from that path would re-enter Docker authorization plugins.  The hook
supplies the trusted container PID/ID and the sandbox name carried by the
``sandbox_cgroup`` annotation; Worker derives the mount namespace and cgroup
values directly from ``/proc``.

The endpoint is the Worker half of the runtime contract: the wire format is
frozen in ``docs/worker-api.md``, the Worker-side flow is in
``docs/container-registration.md``, and the runtime half (wrapper, hook) lives
in ``native/runtime``.
"""

from __future__ import annotations

import logging
import os
import re

from flask import Blueprint, request

# pid → 沙盒、pid 属主校验和沙盒属主解析都只有一份实现，就在 sandboxes.py：
# acquire / join / status 用的也是它们，借条接口必须和它们口径一致。
from neu_box.api.sandboxes import (
    _find_sandbox_for_pid,
    _sandbox_owner,
    _verify_pid_owner,
)
from neu_box.runtime.container_intents import START_INTENT_TTL, StartIntentStore
from neu_box.runtime.containers import (
    DockerExecutorError,
    mount_namespace_of,
    process_start_time,
    runtime_container_identity,
)
from neu_box.runtime.sandbox import SbxManager
from neu_box.storage import CONTAINER_ACTIVE

logger = logging.getLogger(__name__)

container_bp = Blueprint("container", __name__)

# 借条按容器 ID 记（Docker 的 64 位十六进制 ID）：hook 报上来的、docker inspect
# 拿到的都是它，容器名会变、ID 不会。
_CONTAINER_ID = re.compile(r"[0-9a-f]{64}")


def _sandbox_name_from_annotation(value: str) -> str | None:
    """Resolve the ``sandbox_cgroup`` annotation to a stored sandbox name.

    The annotation carries the sandbox name verbatim, so only an exact match
    against the database key is accepted.  cgroup paths and bare basenames are
    rejected on purpose: those are recycled, and after a sandbox is destroyed
    and recreated the stale annotation would point at a live sandbox that
    belongs to somebody else.
    """
    name = str(value or "").strip()
    if not name:
        return None
    if not SbxManager.get_instance().db.get_sandbox(name):
        return None
    return name


def _runtime_error_response(exc: DockerExecutorError):
    """Map a registration failure to the contract's status codes."""
    if exc.code == "sandbox_not_found":
        return {"error": str(exc), "code": exc.code}, 404
    return {"error": str(exc), "code": exc.code}, 409


def _live_registration(manager: SbxManager, record: dict) -> bool:
    """Distinguish a dead init process from an unreadable one after restart.

    A retained pidfd gives a definitive answer.  After Worker restart, the
    manager falls back to /proc; its boolean helper treats read errors as
    dead, so verify a negative result before lending the container again.
    """
    if manager._container_alive(record):
        return True
    namespace = int(record['mount_namespace'])
    if namespace in manager._container_fds:
        return False
    pid = int(record['init_host_pid'])
    try:
        start_time = process_start_time(pid)
        if start_time != int(record['init_start_time']):
            return False
        current_namespace = mount_namespace_of(pid)
    except DockerExecutorError:
        try:
            os.stat(f'/proc/{pid}')
        except FileNotFoundError:
            return False
        raise
    return current_namespace == namespace


def _intent_request_fields(body: dict):
    """借条接口共用的三个字段校验；不合法时返回 (None, 错误响应)。"""
    username = str(body.get("username") or "").strip()
    container_id = str(body.get("container_id") or "").strip().lower()
    if not username or not container_id:
        return None, ({"error": "username 和 container_id 为必填参数"}, 400)
    if not _CONTAINER_ID.fullmatch(container_id):
        return None, ({"error": "container_id 必须是 64 位十六进制容器 ID"}, 400)
    try:
        pid = int(body.get("pid"))
    except (TypeError, ValueError):
        return None, ({"error": "pid 必须为正整数"}, 400)
    if pid <= 0:
        return None, ({"error": "pid 必须为正整数"}, 400)
    if not _verify_pid_owner(pid, username):
        return None, ({
            "error": f"PID {pid} 不属于用户 {username}，或进程不存在",
            "code": "pid_owner_mismatch",
        }, 409)
    return (username, container_id, pid), None


@container_bp.post("/intent")
def lend_sandbox_for_start():
    """存一张 start 借条：把 ``pid`` 所在的那个沙盒借给 ``container_id``。

    ``neubox docker start`` 在真 start 之前调它。借条不能凭空借出卡：pid 必须是
    调用方自己的（``/proc/<pid>/status`` 校属主），pid 必须真的在某个沙盒里，而
    且那个沙盒的属主就是调用方。谁能借、借给谁、认领不到怎么办，见
    ``docs/container-registration.md``。
    """
    body = request.get_json(silent=True) or {}
    fields, error = _intent_request_fields(body)
    if error is not None:
        return error
    username, container_id, pid = fields

    sandbox_name = _find_sandbox_for_pid(pid)
    if not sandbox_name:
        return {
            "error": f"PID {pid} 不在任何沙盒中；请先在本 shell 执行 neubox acquire",
            "code": "not_in_sandbox",
        }, 409
    if _sandbox_owner(sandbox_name) != username:
        return {
            "error": f"沙盒 {sandbox_name} 不属于用户 {username}",
            "code": "sandbox_owner_mismatch",
        }, 409
    manager = SbxManager.get_instance()
    intents = StartIntentStore.get_instance()
    # Same lock order as the hook: registration stripe → manager locks →
    # intent store.  Checking registrations and reserving a new intent in one
    # critical section prevents another hook from finishing between them.
    with intents.registration(container_id):
        with manager.lock, manager._container_registration_lock:
            sandbox = manager.db.get_sandbox(sandbox_name)
            if not sandbox or sandbox.get('state') == 'DESTROYING':
                return {
                    "error": f"沙盒 {sandbox_name} 当前不可用",
                    "code": "sandbox_not_active",
                }, 409
            try:
                for record in manager.db.list_containers():
                    if record.get('container_id') != container_id:
                        continue
                    if _live_registration(manager, record):
                        return {
                            "error": "容器仍在运行并持有现有沙盒授权；请先停止它",
                            "code": "container_still_bound",
                        }, 409
            except Exception:
                logger.exception('无法确认容器 %s 的旧登记是否仍存活', container_id)
                return {
                    "error": "无法确认容器旧授权状态，暂不发放启动借条",
                    "code": "container_binding_unknown",
                }, 409
            lent = intents.lend(
                container_id, username, sandbox_name, borrower_pid=pid,
            )
            if lent is None:
                return {
                    "error": "容器已有未完成或尚未确认的启动借条",
                    "code": "start_intent_busy",
                }, 409
    logger.warning(
        "已记录 start 借条：容器 %s（属主 %s）→ 沙盒 '%s'，%.0fs 内有效",
        container_id, username, sandbox_name, START_INTENT_TTL,
    )
    return {
        "container_id": container_id,
        "sandbox_name": sandbox_name,
        "owner": username,
        "state": "pending",
        "expires_in": START_INTENT_TTL,
    }, 200


@container_bp.get("/intent")
def start_intent_state():
    """查一张借条有没有被认领：``neubox docker start`` 用它确认卡真的借出去了。

    ``neubox`` 启动容器后轮询这里；完整登记成功才返回 ``consumed``。
    没有借条时返回 ``state: null``（不是 404：这里只回答"有没有"）。
    """
    body = {
        "username": request.args.get("username"),
        "container_id": request.args.get("container_id"),
        "pid": request.args.get("pid"),
    }
    fields, error = _intent_request_fields(body)
    if error is not None:
        return error
    username, container_id, pid = fields

    # Only the CLI process that borrowed this intent acknowledges a consumed
    # result.  A second CLI's query must not unlock a replacement borrow.
    entry = StartIntentStore.get_instance().acknowledge(container_id, username, pid)
    return {
        "container_id": container_id,
        "sandbox_name": entry["sandbox_name"] if entry else None,
        "owner": username,
        "state": ("consumed" if entry["consumed_at"] is not None else "pending")
        if entry else None,
        "expires_in": START_INTENT_TTL,
    }, 200


@container_bp.post("/register")
def register_runtime_container():
    """Register a container reported by an OCI runtime hook.

    Required JSON fields are ``container_id``, ``host_pid`` and
    ``sandbox_cgroup`` (the sandbox name).  ``container_cgroup`` and
    ``mount_namespace`` are optional observations from the hook and are
    cross-checked when supplied; the Worker never trusts them as identity.

    The state check and the registration itself happen inside one
    ``SbxManager.lifecycle_lock()`` critical section: see
    ``SbxManager.register_runtime_container``.
    """
    body = request.get_json(silent=True) or {}
    container_id = str(body.get("container_id") or "").strip()
    sandbox_value = str(body.get("sandbox_cgroup") or "").strip()
    if not container_id or not sandbox_value:
        return {"error": "container_id 和 sandbox_cgroup 为必填参数"}, 400
    try:
        host_pid = int(body.get("host_pid"))
    except (TypeError, ValueError):
        return {"error": "host_pid 必须为正整数"}, 400
    if host_pid <= 0:
        return {"error": "host_pid 必须为正整数"}, 400

    # 同一容器的另一次 hook 不得在本次 complete/rollback 之前把暂时写入的
    # 登记当作稳定授权。不同容器仍可并行登记。
    intents = StartIntentStore.get_instance()
    with intents.registration(container_id):
        return _register_with_intent(body, container_id, host_pid,
                                     sandbox_value, intents)


def _same_registration(record: dict | None, identity,
                       sandbox_name: str) -> bool:
    """Only the exact runtime identity may be reused or rolled back."""
    if not record:
        return False
    try:
        return (
            record['sandbox_name'] == sandbox_name
            and record['container_id'] == identity.container_id
            and int(record['mount_namespace']) == identity.mount_namespace
            and int(record['init_host_pid']) == identity.init_host_pid
            and int(record['init_start_time']) == identity.init_start_time
        )
    except (KeyError, TypeError, ValueError):
        return False


def _registration_response(record: dict, identity, created: bool):
    return {
        "sandbox_name": record["sandbox_name"],
        "container_id": record["container_id"],
        "mount_namespace": record["mount_namespace"],
        "container_cgroup": identity.container_cgroup,
        "status": "registered" if created else "already_registered",
    }, 201 if created else 200


def _register_with_intent(body: dict, container_id: str, host_pid: int,
                          sandbox_value: str, intents: StartIntentStore):
    # 借条优先于 annotation。认领只保留本次意图，完整登记成功后才确认，
    # 否则客户端可能把一次身份校验失败误判成已经获得设备授权。
    owner = _sandbox_owner(sandbox_value)
    intent_state, entry = intents.claim_state(container_id, owner)
    if intent_state == 'busy':
        return {
            "error": "该容器的启动借条正在被另一次登记处理",
            "code": "start_intent_busy",
        }, 409
    if intent_state == 'owner_mismatch':
        return {
            "error": "该容器存在其他属主的启动借条，不能使用旧 annotation 登记",
            "code": "start_intent_owner_mismatch",
        }, 409
    claim = entry if intent_state == 'claimed' else None
    completed = False
    try:
        if entry is not None:
            sandbox_name = _sandbox_name_from_annotation(entry['sandbox_name'])
        else:
            sandbox_name = _sandbox_name_from_annotation(sandbox_value)
        if sandbox_name is None and entry is not None:
            return {
                "error": f"start 借条里的沙盒 {entry['sandbox_name']} 已不可用",
                "code": "sandbox_not_active",
            }, 409
        if sandbox_name is None:
            return {
                "error": "sandbox_cgroup 未匹配到 Worker 沙盒",
                "code": "sandbox_not_found",
            }, 404
        if claim is not None:
            logger.warning(
                "容器 %s 按 start 借条绑到沙盒 '%s'（annotation 指向 '%s'）",
                container_id, sandbox_name, sandbox_value,
            )

        try:
            identity = runtime_container_identity(container_id, host_pid)
        except DockerExecutorError as exc:
            return _runtime_error_response(exc)

        supplied_cgroup = body.get("container_cgroup")
        supplied_mnt = body.get("mount_namespace")
        if supplied_cgroup is not None and str(supplied_cgroup) != identity.container_cgroup:
            return {
                "error": "container_cgroup 与实际值不一致",
                "code": "runtime_identity_changed",
            }, 409
        if supplied_mnt is not None:
            try:
                if int(supplied_mnt) != identity.mount_namespace:
                    raise ValueError
            except (TypeError, ValueError):
                return {
                    "error": "mount_namespace 与实际值不一致",
                    "code": "runtime_identity_changed",
                }, 409

        manager = SbxManager.get_instance()
        if intent_state == 'consumed':
            # A retry of the *same* hook may have lost the first HTTP reply.
            # Return idempotently only for the exact already-bound identity;
            # never create a second binding from a consumed intent.
            with manager.lock, manager._container_registration_lock:
                record = manager.db.get_container(identity.mount_namespace)
                sandbox = manager.db.get_sandbox(sandbox_name)
                if (record and record.get('state') == CONTAINER_ACTIVE
                        and sandbox and sandbox.get('state') != 'DESTROYING'
                        and _same_registration(record, identity, sandbox_name)):
                    return _registration_response(record, identity, False)
            return {
                "error": "启动借条已被其他容器运行消费",
                "code": "start_intent_changed",
            }, 409

        # ``register_container`` pins mnt ns and watches PID exit. A new
        # registration can be rolled back immediately if its claim changes.
        try:
            record, created = manager.register_runtime_container(
                sandbox_name, identity,
            )
        except DockerExecutorError as exc:
            return _runtime_error_response(exc)
        if (claim is not None and not created
                and (record.get('state') != CONTAINER_ACTIVE
                     or not _same_registration(record, identity, sandbox_name))):
            return {
                "error": "容器已登记到另一沙盒或另一运行实例",
                "code": "docker_container_registered_elsewhere",
            }, 409
        if claim is not None:
            if not intents.complete(container_id, owner, claim['sequence']):
                if created:
                    try:
                        manager.release_container(
                            identity.mount_namespace,
                            expected_sandbox_name=sandbox_name,
                            expected_container_id=identity.container_id,
                            expected_init_host_pid=identity.init_host_pid,
                            expected_init_start_time=identity.init_start_time,
                        )
                        remaining = manager.db.get_container(identity.mount_namespace)
                        if _same_registration(remaining, identity, sandbox_name):
                            raise RuntimeError('本次登记仍存在')
                    except Exception:
                        logger.exception('start 借条失效后回滚容器 %s 授权失败', container_id)
                        return {
                            "error": "启动借条失效，容器授权回滚失败",
                            "code": "start_intent_rollback_failed",
                        }, 500
                return {
                    "error": "登记期间启动借条已失效或被另一次启动替换",
                    "code": "start_intent_changed",
                }, 409
            completed = True
        return _registration_response(record, identity, created)
    finally:
        if claim is not None and not completed:
            intents.abort(container_id, owner, claim['sequence'])
