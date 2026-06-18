"""沙盒服务 API — 允许用户将当前终端进程加入独占设备的 cgroup 沙盒。

内网用户可通过 curl 或 neu-sbox 脚本调用：
  POST /sandbox/acquire   → 创建沙盒，把调用者的 shell PID 加入
  POST /sandbox/release   → 销毁沙盒，释放设备
  GET  /sandbox/list      → 列出自己的沙盒
"""

import logging
import os
import pwd

from flask import Blueprint, request

from executor.sbx_manager import SbxManager

logger = logging.getLogger(__name__)
sandbox_bp = Blueprint('sandbox', __name__)


def _verify_pid_owner(pid: int, username: str) -> bool:
    """校验 PID 是否属于 username（通过 /proc/<pid>/status 的 Uid 字段）。"""
    try:
        pw = pwd.getpwnam(username)
        with open(f'/proc/{pid}/status') as f:
            for line in f:
                if line.startswith('Uid:'):
                    real_uid = int(line.split()[1])
                    return real_uid == pw.pw_uid
    except Exception:
        pass
    return False


@sandbox_bp.route('/acquire', methods=['POST'])
def acquire():
    """创建沙盒并加入指定 PID。

    请求体:
        { "username": "pengyt", "pid": 12345, "device_num": 1,
          "cpu": 0, "memory": 0, "mem_unit": "GB" }

    响应: { "sandbox_name": "user_pengyt_12345", "devices": ["235:0"], "message": "..." }
    """
    body = request.get_json(silent=True) or {}

    username = (body.get('username') or '').strip()
    pid = body.get('pid', 0)
    device_num = body.get('device_num', 0)
    cpu = body.get('cpu', 0)
    mem_val = body.get('memory', 0)
    mem_unit = body.get('mem_unit', 'GB')

    if not username or not pid:
        return {'error': 'username 和 pid 为必填参数'}, 400
    if not isinstance(pid, int) or pid <= 0:
        return {'error': 'pid 必须为正整数'}, 400

    # 校验 PID 归属
    if not _verify_pid_owner(pid, username):
        return {'error': f'PID {pid} 不属于用户 {username}，或进程不存在'}, 403

    # 转换内存格式
    if mem_val == 0:
        sandbox_mem = '0'
    elif mem_unit == 'GB':
        sandbox_mem = f'{mem_val}G'
    else:
        sandbox_mem = f'{mem_val}M'

    # 创建沙盒并分配设备
    sbx = SbxManager.get_instance()
    terminal_id = f"{username}_{pid}"
    result = sbx.allocate_for_terminal(
        terminal_id,
        cpu=cpu,
        mem=sandbox_mem,
        device_num=device_num,
    )
    if result is None:
        return {'error': '沙盒创建失败，设备可能不足'}, 503

    sandbox_name = result['sandbox_name']
    devices = result['devices']

    # 把用户进程加入沙盒
    if not sbx.join_sandbox(sandbox_name, pid):
        sbx.destroy_sandbox(sandbox_name)
        return {'error': '加入沙盒失败'}, 500

    logger.warning("用户 %s PID %s 已加入沙盒 '%s'，独占设备 %s",
                   username, pid, sandbox_name, devices)

    return {
        'sandbox_name': sandbox_name,
        'devices': devices,
        'message': f'PID {pid} 已加入沙盒 {sandbox_name}，独占设备 {devices}',
    }, 201


@sandbox_bp.route('/release', methods=['POST'])
def release():
    """销毁沙盒，释放设备。

    请求体: { "sandbox_name": "user_pengyt_12345" }
    """
    body = request.get_json(silent=True) or {}
    sandbox_name = (body.get('sandbox_name') or '').strip()

    if not sandbox_name:
        return {'error': 'sandbox_name 为必填参数'}, 400

    sbx = SbxManager.get_instance()
    ok = sbx.destroy_sandbox(sandbox_name)
    if ok:
        logger.warning("沙盒 '%s' 已手动释放", sandbox_name)
        return {'message': f'沙盒 {sandbox_name} 已销毁', 'sandbox_name': sandbox_name}, 200
    else:
        return {'message': f'沙盒 {sandbox_name} 不存在或已销毁', 'sandbox_name': sandbox_name}, 200


@sandbox_bp.route('/list', methods=['GET'])
def list_sandboxes():
    """列出沙盒。

    Query: ?username=pengyt  可选，过滤指定用户
    """
    username = (request.args.get('username') or '').strip()
    sbx = SbxManager.get_instance()
    all_names = sbx.list_sandboxes()

    if username:
        prefix = f"term_{username}_"
        filtered = [n for n in all_names if n.startswith(prefix)]
    else:
        filtered = all_names

    return {'sandboxes': filtered}, 200
