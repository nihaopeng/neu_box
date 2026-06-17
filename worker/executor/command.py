"""命令执行模块 — 在沙盒中安全执行用户提交的命令。

架构:
  TaskQueue (FIFO)  →  后台线程逐任务消费  →  execute_in_sandbox (SIGSTOP 方案)

API:
  POST /command/run           提交命令（异步），返回 task_id
  GET  /command/queue         查看所有任务队列（公开元数据，不含日志）
  GET  /command/result/<id>   查看自己的任务结果（含日志，需 user_id 匹配）
"""

import os
import pwd
import signal
import subprocess
import threading
import time
import uuid
from collections import OrderedDict

from flask import Blueprint, request

from executor.sbx_manager import SbxManager

command_bp = Blueprint('command', __name__)

DEFAULT_TIMEOUT = int(os.getenv('command_timeout', '300'))
# 已完成任务保留数量上限（FIFO 淘汰）
MAX_COMPLETED_TASKS = int(os.getenv('command_max_completed', '200'))
# 队列视图中展示的最近完成任务数
QUEUE_RECENT_LIMIT = int(os.getenv('command_queue_recent', '30'))


# ==================================================================
# 安全执行 — preexec_fn 写 cgroup.procs + setuid 切用户
# ==================================================================
#
# 流程:
#   建沙盒 → Popen(preexec_fn: 写 cgroup.procs → setuid → exec bash)
#   → 更新 DB → communicate(等待完成) → 销毁沙盒
#
# preexec_fn 运行在 fork 之后、exec 之前的子进程中：
#   1. 此时仍是 root，可以写 cgroup.procs
#   2. setgid/setuid 切到目标用户
#   3. chdir 到用户 HOME
#   4. 退出 preexec_fn，子进程 exec bash -c <命令>
#
# 为什么不用 SIGSTOP:
#   之前的 SIGSTOP 方案是 preexec_fn + os.kill(getpid(), SIGSTOP)，
#   在 ARM64 Python 上子进程直接退出。write() + setuid() 不涉及
#   信号操作，不受该问题影响。
# ==================================================================


def _cgroup_procs_path(sandbox_name: str) -> str:
    return f"/sys/fs/cgroup/sandbox_{sandbox_name}/cgroup.procs"


def execute_in_sandbox(
    command: str,
    sandbox_name: str,
    cpu: int = 0,
    mem: str = "0",
    devices: list = None,
    timeout: int = DEFAULT_TIMEOUT,
    username: str = '',
) -> dict:
    """在沙盒中以指定用户身份安全执行一条命令。

    流程:
      1. 建沙盒（空壳）
      2. Popen(preexec_fn: 写 PID 到 cgroup.procs → setuid 切用户 → 返回)
      3. 更新 DB 记录（join_sandbox 做幂等确认）
      4. communicate(timeout) 收集 stdout/stderr
      5. 清理沙盒，返回结果
    """
    sbx = SbxManager.get_instance()

    # 1. 建沙盒（空壳，还没有进程）
    ok = sbx.create_sandbox(sandbox_name, cpu=cpu, mem=mem,
                            devices=devices if devices else None)
    if not ok:
        print(f"[execute_in_sandbox] 沙盒 '{sandbox_name}' 创建失败")
        return {
            'returncode': -1, 'stdout': '', 'stderr': f'Failed to create sandbox "{sandbox_name}"',
            'timed_out': False, 'error': 'sandbox_create_failed',
        }

    proc = None
    cg_procs = _cgroup_procs_path(sandbox_name)

    # 2. 构建 preexec_fn：写 cgroup.procs + 切用户
    if username:
        try:
            pw = pwd.getpwnam(username)
            target_uid = pw.pw_uid
            target_gid = pw.pw_gid
            target_dir = pw.pw_dir
        except KeyError:
            return {
                'returncode': -1, 'stdout': '', 'stderr': f'Unknown user: {username}',
                'timed_out': False, 'error': 'unknown_user',
            }

        def preexec():
            # 1) 写 PID 到 cgroup（仍是 root）
            with open(cg_procs, 'w') as f:
                f.write(str(os.getpid()))
            # 2) 切到目标用户
            os.setgid(target_gid)
            os.setuid(target_uid)
            os.chdir(target_dir)
            os.environ['HOME'] = target_dir
    else:
        # 不切用户，仅写 cgroup
        def preexec():
            with open(cg_procs, 'w') as f:
                f.write(str(os.getpid()))

    try:
        print(f"[execute_in_sandbox] 启动进程, cgroup={cg_procs}, user={username or '(root)'}")
        proc = subprocess.Popen(
            ['bash', '-c', command],
            preexec_fn=preexec,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        print(f"[execute_in_sandbox] 子进程 PID={proc.pid} 已启动")

        # 3. 更新 DB 记录（join_sandbox 也会写 cgroup.procs，幂等）
        time.sleep(0.05)
        try:
            sbx.join_sandbox(sandbox_name, proc.pid)
        except Exception as e:
            print(f"[execute_in_sandbox] join_sandbox 跳过 (进程可能已退出): {e}")

        # 4. 等待命令执行完成
        print(f"[execute_in_sandbox] 等待 PID={proc.pid} 完成 (timeout={timeout}s)")
        try:
            stdout, stderr = proc.communicate(timeout=timeout)
            timed_out = False
        except subprocess.TimeoutExpired:
            timed_out = True
            print(f"[execute_in_sandbox] PID={proc.pid} 超时，正在终止...")
            try:
                os.killpg(proc.pid, signal.SIGTERM)
                proc.wait(timeout=5)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                try:
                    proc.kill()
                    proc.wait(timeout=5)
                except Exception:
                    pass
            stdout, stderr = proc.communicate()

        print(f"[execute_in_sandbox] PID={proc.pid} 完成, rc={proc.returncode}")
        return {
            'returncode': proc.returncode if not timed_out else -1,
            'stdout': stdout.decode('utf-8', errors='replace'),
            'stderr': stderr.decode('utf-8', errors='replace'),
            'timed_out': timed_out,
            'error': None,
        }

    except Exception as e:
        print(f"[execute_in_sandbox] 异常: {type(e).__name__}: {e}")
        import traceback
        traceback.print_exc()
        if proc is not None:
            try:
                proc.kill()
                proc.wait(timeout=5)
            except Exception:
                pass
        return {
            'returncode': -1, 'stdout': '', 'stderr': f'Execution error: {e}',
            'timed_out': False, 'error': 'exception',
        }
    finally:
        print(f"[execute_in_sandbox] 清理沙盒 '{sandbox_name}'")
        try:
            sbx.destroy_sandbox(sandbox_name)
        except Exception as e:
            print(f"[execute_in_sandbox] 清理沙盒异常: {e}")


# ==================================================================
# 任务队列（单例，FIFO）
# ==================================================================

from executor.db import Database


class TaskQueue:
    """FIFO 任务队列管理器（单例）。

    每个 Worker 有自己的队列，后台线程逐任务消费。
    任务数据持久化到 SQLite，进程重启后可恢复历史记录。
    """

    _instance = None

    def __init__(self):
        self._lock = threading.Lock()
        self._cv = threading.Condition(self._lock)

        # 等待队列: 有序字典保持 FIFO，key=task_id（内存，驱动消费线程）
        self._pending: OrderedDict[str, dict] = OrderedDict()
        # 当前正在执行的任务
        self._running: dict | None = None

        self._running_flag = False
        self._worker_thread: threading.Thread | None = None

        # 数据库实例
        self._db = Database.get_instance()

        # 启动时恢复：标记上次异常退出的 running 任务为 failed
        self._recover_orphaned()

    @classmethod
    def get_instance(cls) -> 'TaskQueue':
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    # ── 启动/停止 ─────────────────────────────────────────────

    def start(self):
        if self._running_flag:
            return
        self._running_flag = True
        self._worker_thread = threading.Thread(
            target=self._consume_loop, daemon=True, name='task-queue-consumer')
        self._worker_thread.start()
        print('[TaskQueue] 后台消费线程已启动')

    def _recover_orphaned(self):
        """启动恢复：将上次异常退出的任务复原。"""
        all_active = self._db.get_queue_tasks()
        for task in all_active:
            if task['status'] == 'running':
                print(f'[TaskQueue] 恢复: 标记孤儿任务 {task["task_id"]} 为 failed')
                self._db.update_task_result(
                    task['task_id'], 'failed', -1, '', '',
                    error='Worker 可能在执行过程中重启')
            elif task['status'] == 'queued':
                # 重新加入内存队列（按原 position 排序）
                print(f'[TaskQueue] 恢复: 重新入队 {task["task_id"]} '
                      f'(原 position={task.get("position")})')
                self._pending[task['task_id']] = {
                    'task_id': task['task_id'],
                    'user_id': task['user_id'],
                    'command': task['command'],
                    'cpu': task.get('cpu', 0),
                    'mem': task.get('mem', '0'),
                    'devices': task.get('devices', []),
                    'status': 'queued',
                    'position': 0,
                    'created_at': task.get('created_at', time.time()),
                    'started_at': None,
                    'finished_at': None,
                    'result': None,
                }
        # 重新排位
        if self._pending:
            self._reindex()
            print(f'[TaskQueue] 恢复完成: {len(self._pending)} 个任务重新入队')

    # ── 提交 ──────────────────────────────────────────────────

    def submit(self, user_id: str, command: str,
               cpu: int = 0, mem: str = "0",
               devices: list = None, password: str = '') -> str:
        """提交任务到队列，返回 task_id。user_id 即系统用户名。"""
        task_id = uuid.uuid4().hex[:12]
        devices_list = devices or []

        # 持久化到 DB
        self._db.insert_task(
            task_id=task_id, user_id=user_id, command=command,
            cpu=cpu, mem=mem, devices=devices_list, password=password)

        task = {
            'task_id': task_id,
            'user_id': user_id,
            'command': command,
            'cpu': cpu,
            'mem': mem,
            'devices': devices_list,
            'status': 'queued',
            'position': 0,
            'created_at': time.time(),
            'started_at': None,
            'finished_at': None,
            'result': None,
        }

        with self._lock:
            self._pending[task_id] = task
            self._reindex()
            self._cv.notify()

        print(f'[TaskQueue] 任务入队: {task_id} user={user_id} cmd={command[:60]}...')
        return task_id

    # ── 查询 ──────────────────────────────────────────────────

    def get_queue(self) -> list[dict]:
        """返回队列视图：运行中 → 排队中 → 最近完成/失败的任务（不含日志）。
        从 DB 读取，保证进程重启后数据不丢。
        """
        active = self._db.get_queue_tasks()              # queued + running
        recent = self._db.get_recent_tasks(limit=QUEUE_RECENT_LIMIT)  # completed + failed
        # 去重：active 中的 task_id 优先（理论上状态不同不会重复）
        active_ids = {t['task_id'] for t in active}
        all_tasks = active + [t for t in recent if t['task_id'] not in active_ids]
        return [self._format_public(t) for t in all_tasks]

    def get_result(self, task_id: str, user_id: str, password: str = '') -> dict | None:
        """获取任务结果。需 user_id 匹配 + password 正确才返回日志。

        Returns:
            None 表示任务不存在
            dict: 含 status、user_id 等公开字段。
                  'result' 仅当 user_id 匹配且密码正确时包含。
                  'permission_denied'=True 表示身份匹配但密码错误。
        """
        task = self._db.get_task(task_id)
        if task is None:
            return None

        public = self._format_public(task)
        if task['user_id'] != user_id:
            return public  # 别人的任务，不返回日志

        # 验证密码
        if not self._db.verify_task_password(task_id, password):
            public['permission_denied'] = True
            return public

        public['result'] = {
            'stdout': task.get('stdout') or '',
            'stderr': task.get('stderr') or '',
            'returncode': task.get('returncode'),
            'timed_out': bool(task.get('timed_out')),
        }
        return public

    # ── 内部 ──────────────────────────────────────────────────

    def _reindex(self):
        """更新所有 pending 任务的 position 字段（FIFO 顺序）。"""
        task_ids = list(self._pending.keys())
        for i, tid in enumerate(task_ids):
            self._pending[tid]['position'] = i + 1
        # 同步到 DB
        if task_ids:
            self._db.update_position_batch(task_ids)

    @staticmethod
    def _format_public(task: dict) -> dict:
        """返回任务的公开视图（不含日志）。"""
        return {
            'task_id': task['task_id'],
            'user_id': task['user_id'],
            'command': task['command'],
            'status': task['status'],
            'position': task.get('position', 0),
            'cpu': task.get('cpu', 0),
            'mem': task.get('mem', '0'),
            'devices': task.get('devices', []),
            'created_at': task.get('created_at'),
            'started_at': task.get('started_at'),
            'finished_at': task.get('finished_at'),
        }

    # ── 消费循环 ──────────────────────────────────────────────

    def _consume_loop(self):
        """后台线程：从队列中取任务，逐个执行。"""
        while self._running_flag:
            task = None
            with self._lock:
                if not self._pending:
                    self._cv.wait(timeout=30)
                    continue

                _, task = self._pending.popitem(last=False)
                task['status'] = 'running'
                task['started_at'] = time.time()
                self._running = task
                self._reindex()

            # 更新 DB 状态为 running
            self._db.update_task_status(
                task['task_id'], 'running', started_at=task['started_at'])

            # 在锁外执行
            print(f'[TaskQueue] 开始执行: {task["task_id"]} '
                  f'user={task["user_id"]} cmd={task["command"][:60]}')

            sandbox_name = f"cmd_{task['task_id']}"
            result = execute_in_sandbox(
                command=task['command'],
                sandbox_name=sandbox_name,
                cpu=task.get('cpu', 0),
                mem=task.get('mem', '0'),
                devices=task.get('devices') if task.get('devices') else None,
                username=task['user_id'],
            )

            # 持久化结果到 DB
            finished_at = time.time()
            status = ('completed' if result.get('returncode') == 0
                      and not result.get('timed_out') else 'failed')
            self._db.update_task_result(
                task_id=task['task_id'],
                status=status,
                returncode=result.get('returncode', -1),
                stdout=result.get('stdout', ''),
                stderr=result.get('stderr', ''),
                timed_out=result.get('timed_out', False),
                error=result.get('error'),
                finished_at=finished_at,
            )

            # 淘汰过旧记录
            self._db.cleanup_old_tasks(keep=MAX_COMPLETED_TASKS)

            # 更新内存状态
            with self._lock:
                task['result'] = result
                task['finished_at'] = finished_at
                task['status'] = status
                self._running = None

            print(f'[TaskQueue] 完成: {task["task_id"]} status={status} '
                  f'rc={result.get("returncode")}')


# ==================================================================
# Flask 路由
# ==================================================================

@command_bp.route('/run', methods=['POST'])
def run_command():
    """提交命令到任务队列（异步）。

    请求体 (JSON):
        { "command": "...", "user_id": "...", "cpu": 2, "memory": 4,
          "mem_unit": "GB", "device_num": 1 }

    响应: { "task_id": "abc123", "position": 3, "message": "..." }
    """
    body = request.get_json(silent=True) or {}

    command = (body.get('command') or '').strip()
    if not command:
        return {'error': '命令不能为空'}, 400

    user_id = (body.get('user_id') or '').strip()
    if not user_id:
        return {'error': 'user_id 不能为空'}, 400

    password = body.get('password') or ''
    if not password:
        return {'error': '密码不能为空'}, 400

    cpu = body.get('cpu', 0)
    if not isinstance(cpu, int) or cpu < 0:
        cpu = 0

    mem_val = body.get('memory', 0)
    mem_unit = body.get('mem_unit', 'GB')
    if not isinstance(mem_val, int) or mem_val < 0:
        mem_val = 0
    if mem_val == 0:
        sandbox_mem = '0'
    elif mem_unit == 'GB':
        sandbox_mem = f'{mem_val}G'
    else:
        sandbox_mem = f'{mem_val}M'

    device_num = body.get('device_num', 0)
    if not isinstance(device_num, int) or device_num < 0:
        device_num = 0

    # 分配设备
    devices = []
    if device_num > 0:
        sbx = SbxManager.get_instance()
        free = sbx._get_free_devices()
        if len(free) < device_num:
            return {'error': f'设备不足: 需要 {device_num} 个, 可用 {len(free)} 个'}, 503
        devices = free[:device_num]

    tq = TaskQueue.get_instance()
    task_id = tq.submit(
        user_id=user_id,
        command=command,
        cpu=cpu,
        mem=sandbox_mem,
        devices=devices if devices else None,
        password=password,
    )

    # 从 DB 获取当前队列位置
    db = Database.get_instance()
    task = db.get_task(task_id)
    position = task['position'] if task else 0

    return {
        'task_id': task_id,
        'position': position,
        'message': f'任务已提交，队列位置 #{position}',
    }, 202


@command_bp.route('/queue', methods=['GET'])
def get_queue():
    """查看当前任务队列（所有用户的待执行 + 正在执行的任务，不含日志）。

    响应: { "queue": [...], "running": {...} }
    """
    tq = TaskQueue.get_instance()
    return {
        'queue': tq.get_queue(),
        'total_pending': len(tq._pending),
    }, 200


@command_bp.route('/result/<task_id>', methods=['GET'])
def get_result(task_id: str):
    """查看某个任务的完整结果。需要 user_id 参数做权限校验。

    Query: ?user_id=xxx
    只有本人能看到日志输出。

    响应: 任务完整信息（含 stdout/stderr 当 user_id 匹配时）
    """
    user_id = (request.args.get('user_id') or '').strip()
    password = request.args.get('password') or ''
    if not user_id:
        return {'error': 'user_id 参数必填'}, 400

    tq = TaskQueue.get_instance()
    result = tq.get_result(task_id, user_id, password)

    if result is None:
        return {'error': '任务不存在'}, 404

    return result, 200


# ── 启动 TaskQueue（在 import 时自动启动） ──
TaskQueue.get_instance().start()
