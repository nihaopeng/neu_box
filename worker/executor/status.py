"""Worker 节点状态上报 — 查询本机 CPU/内存/GPU/NPU 资源及活跃沙盒。
设备信息（GPU/NPU）由独立的 shell 脚本采集并输出 JSON，实现解耦。
"""

import json
import os
import subprocess

import psutil
from flask import Blueprint

status_bp = Blueprint('status', __name__)

class Node_Manager:
    """Worker 本地节点状态管理器（单例），提供系统资源查询接口。"""

    _instance = None

    def __init__(self):
        self._cached_total_cpu: int = psutil.cpu_count(logical=True) or 0

    @classmethod
    def get_instance(cls) -> 'Node_Manager':
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    # ── CPU ───────────────────────────────────────────────────

    def cpu_info(self) -> tuple[int, float]:
        """返回 (total_cores, idle_percent)。"""
        psutil.cpu_percent(interval=0.05)            # 第一次预热
        usage = psutil.cpu_percent(interval=0.05)
        idle = max(0.0, 100.0 - usage)
        return self._cached_total_cpu, idle

    # ── 内存 ──────────────────────────────────────────────────

    def mem_info(self) -> tuple[int, int]:
        """返回 (total_bytes, available_bytes)。"""
        mem = psutil.virtual_memory()
        return mem.total, mem.available

    # ── 设备信息（由独立脚本采集） ─────────────────────────────

    def _run_device_script(self,path) -> dict:
        """执行 scripts/<name>.sh，解析其 JSON 输出。失败返回全 0。"""
        try:
            out = subprocess.check_output(
                [path],
                timeout=10,
                stderr=subprocess.DEVNULL,
            )
            return json.loads(out.decode())
        except (FileNotFoundError, subprocess.CalledProcessError,
                subprocess.TimeoutExpired, json.JSONDecodeError):
            return {'total': 0, 'idle': 0}

    def gpu_info(self) -> dict:
        return self._run_device_script(os.getenv('gpu_info_script_path'))

    def npu_info(self) -> dict:
        return self._run_device_script(os.getenv('npu_info_script_path'))

    # ── 活跃沙盒 ──────────────────────────────────────────────

    def active_sandbox_count(self) -> int:
        """统计 cgroup 中实际存在的沙盒数量（通过 sandbox.sh list）。"""
        try:
            from executor.sbx_manager import SbxManager
            return len(SbxManager.get_instance().list_sandboxes_via_script())
        except Exception:
            return 0

    # ── 汇总 ──────────────────────────────────────────────────

    def collect_status(self) -> dict:
        """汇总本节点完整资源状态。"""
        total_cpu, idle_cpu = self.cpu_info()
        total_mem, idle_mem = self.mem_info()
        gpu = self.gpu_info()
        npu = self.npu_info()
        sandboxes = self.active_sandbox_count()

        return {
            'status': 'online',
            'total_cpu': total_cpu,
            'idle_cpu': round(idle_cpu, 1),
            'total_mem': total_mem,
            'idle_mem': idle_mem,
            'total_devices': gpu['total'] + npu['total'],
            'idle_devices': gpu['idle'] + npu['idle'],
            'active_sandboxes': sandboxes,
        }


# ── 路由 ──────────────────────────────────────────────────────

@status_bp.route('/status', methods=['GET'])
def node_status():
    """返回本节点当前资源状态，供 master 查询。"""
    return Node_Manager.get_instance().collect_status(), 200
