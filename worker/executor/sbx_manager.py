"""沙盒资源分配管理 — 调用 sandbox.sh 实现 cgroup v2 + eBPF 设备隔离。
沙盒状态持久化到 SQLite，防止掉线后重复开沙盒出问题。

设备分配模型:
  - 从 .env 读取单一的 device_major（如 235 即 NPU，195 即 GPU）
  - 扫描 /dev 发现该 major 下的所有字符设备节点（如 235:0, 235:1, ...）
  - 通过 DB 追踪每个沙盒已占用的设备，空闲设备 = 全部 - 已分配
  - 终端申请时按 device_num 从空闲池中分配
  - 通过 Node_Manager (status.py) 校验设备空闲数量
"""

import json
import logging
import os
import signal
import stat
import subprocess
import threading
import time
from typing import Optional, List

import psutil

from executor.db import Database

logger = logging.getLogger(__name__)


# ==================================================================
# SbxManager — 沙盒生命周期管理（单例）
# ==================================================================

class SbxManager:
    """Worker 本地沙盒管理器（单例）。

    封装 sandbox.sh 的 create / join / destroy / status 调用，
    并在本地 DB 中记录每个沙盒的状态，支持重启后恢复。
    """

    _instance = None

    def __init__(self):
        # 脚本路径
        default_script = os.path.join(
            os.path.dirname(__file__), '..', 'scripts', 'sanbox', 'v2', 'sandbox.sh'
        )
        self._script_path = os.getenv('sandbox_script_path', default_script)

        # 设备 major 号（单一值，从环境变量读取，默认 235 = NPU）
        self.device_major = int(os.getenv('device_major', '235'))

        # 本地 DB（统一 SQLite）
        self.db = Database.get_instance()

        # 线程安全
        self._lock = threading.Lock()

        # 启动时恢复
        self._recover_on_startup()

    @classmethod
    def get_instance(cls) -> 'SbxManager':
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    # ── 内部工具 ─────────────────────────────────────────────────

    def _run_script(self, *args) -> subprocess.CompletedProcess:
        """调用 sandbox.sh，返回 CompletedProcess。"""
        cmd = [self._script_path] + list(args)
        return subprocess.run(cmd, capture_output=True, text=True, timeout=30)

    @staticmethod
    def _cg_path(name: str) -> str:
        return f"/sys/fs/cgroup/sandbox_{name}"

    @staticmethod
    def _discover_device_nodes(major: int) -> List[str]:
        """扫描 /dev 目录，找出指定 major 号的所有字符设备节点。

        Returns:
            "major:minor" 字符串列表，按 minor 排序，如 ["235:0", "235:1", ...]
        """
        devices = []
        try:
            for entry in os.listdir('/dev'):
                path = os.path.join('/dev', entry)
                try:
                    s = os.stat(path)
                    if stat.S_ISCHR(s.st_mode) and os.major(s.st_rdev) == major:
                        devices.append(f"{major}:{os.minor(s.st_rdev)}")
                except OSError:
                    continue
        except OSError:
            pass
        devices.sort(key=lambda x: int(x.split(':')[1]))
        return devices

    def _get_allocated_devices(self) -> set:
        """扫描 DB 中所有活跃沙盒，汇总已分配的设备号集合。"""
        allocated = set()
        for rec in self.db.list_sandboxes():
            for dev in rec.get('devices', []):
                allocated.add(dev)
        return allocated

    def _list_sandbox_names(self) -> List[str]:
        """返回所有沙盒名称列表（兼容旧 SandboxDB.list_all 接口）。"""
        return [s['name'] for s in self.db.list_sandboxes()]

    def _get_free_devices(self) -> List[str]:
        """返回当前空闲的设备节点列表（全部 - 已分配），按 minor 排序。"""
        all_devices = set(self._discover_device_nodes(self.device_major))
        allocated = self._get_allocated_devices()
        free = sorted(all_devices - allocated, key=lambda x: int(x.split(':')[1]))
        return free

    # ── 启动恢复 ─────────────────────────────────────────────────

    def _recover_on_startup(self):
        """重启后核对 DB 与 cgroup 实际状态，清理已不存在的沙盒记录。"""
        for name in self._list_sandbox_names():
            if not os.path.isdir(self._cg_path(name)):
                logger.warning("恢复: 沙盒 '%s' 的 cgroup 已不存在，清理 DB 记录", name)
                self.db.delete_sandbox(name)
            else:
                logger.info("恢复: 沙盒 '%s' 仍存活", name)

    # ── 设备空闲校验 ─────────────────────────────────────────────

    def _validate_idle_count(self, needed: int) -> bool:
        """通过 Node_Manager (status.py) 校验设备空闲数量是否足够。

        根据 device_major 选择对应的信息源:
          235 → npu_info() (NPU / Davinci)
          195 → gpu_info() (NVIDIA GPU)
        """
        # 延迟导入，避免在 status.py 初始化前触发
        from executor.status import Node_Manager
        nm = Node_Manager.get_instance()

        if self.device_major == 235:
            info = nm.npu_info()
        elif self.device_major == 195:
            info = nm.gpu_info()
        else:
            # 未知 major，跳过系统级校验，仅依赖 DB
            return True

        idle = info.get('idle', 0)
        if idle < needed:
            logger.warning("设备不足: 需要 %s 个, 系统空闲 %s 个 (major=%s)",
                           needed, idle, self.device_major)
            return False

        logger.debug("设备校验通过: 需要 %s 个, 系统空闲 %s 个 (major=%s)",
                     needed, idle, self.device_major)
        return True

    # ── 核心操作 ─────────────────────────────────────────────────

    def create_sandbox(self, name: str, cpu: int = 0, mem: str = "0",
                       devices: Optional[List[str]] = None,
                       port: int = None) -> bool:
        """创建沙盒。

        Args:
            name:    沙盒名称
            cpu:     CPU 核数 (0=不限)
            mem:     内存限制 (如 "512M", "2G", "0"=不限)
            devices: 设备号列表 (如 ["235:0", "235:1"])。None 表示不预留任何设备。

        Returns:
            True 表示创建成功（或已存在且有效）。
        """
        with self._lock:
            # 已存在且在 cgroup 中有效 → 直接返回
            if os.path.isdir(self._cg_path(name)):
                existing = self.db.get_sandbox(name)
                if existing:
                    logger.info("沙盒 '%s' 已存在，跳过创建", name)
                    return True

            # 构建命令行
            args = ['create', name, str(cpu), mem]
            if devices:
                args.extend(devices)

            logger.info("创建沙盒 '%s' (cpu=%s, mem=%s, devices=%s)", name, cpu, mem, devices)
            result = self._run_script(*args)
            if result.returncode != 0:
                logger.error("创建沙盒 '%s' 失败: %s", name, result.stderr.strip())
                return False

            # 写入 DB
            self.db.insert_sandbox(
                name=name, cpu=cpu, mem=mem,
                devices=devices or [],
                cgroup_path=self._cg_path(name),
                pids=[], port=port)
            logger.info("✓ 沙盒 '%s' 创建成功", name)
            return True

    def join_sandbox(self, name: str, pid: int) -> bool:
        """将进程加入沙盒。

        Returns:
            True 表示加入成功。
        """
        with self._lock:
            record = self.db.get_sandbox(name)
            if not record:
                logger.error("加入失败: 沙盒 '%s' 不在 DB 中", name)
                return False

            result = self._run_script('join', name, str(pid))
            if result.returncode != 0:
                logger.error("加入 PID %s 到 '%s' 失败: %s", pid, name, result.stderr.strip())
                return False

            # 更新 DB
            pids = record.get('pids', [])
            if pid not in pids:
                pids.append(pid)
                record['pids'] = pids
                self.db.update_sandbox_pids(name, pids)

            logger.info("✓ PID %s 已加入沙盒 '%s'", pid, name)
            return True

    def destroy_sandbox(self, name: str) -> bool:
        """销毁沙盒，清理 cgroup 和 eBPF 预留，释放设备。

        Returns:
            True 表示销毁成功（或沙盒本来就不存在）。
        """
        with self._lock:
            if not os.path.isdir(self._cg_path(name)):
                self.db.delete_sandbox(name)
                return True

            result = self._run_script('destroy', name)
            if result.returncode != 0:
                logger.error("销毁沙盒 '%s' 失败: %s", name, result.stderr.strip())
                # 如果 cgroup 确实没了，至少清理 DB
                if not os.path.isdir(self._cg_path(name)):
                    self.db.delete_sandbox(name)
                return False

            self.db.delete_sandbox(name)
            logger.info("✓ 沙盒 '%s' 已销毁", name)
            return True

    def sandbox_status(self, name: str) -> Optional[dict]:
        """查询沙盒状态（调用 sandbox.sh status）。"""
        if not os.path.isdir(self._cg_path(name)):
            return None
        result = self._run_script('status', name)
        if result.returncode != 0:
            return None
        return {'name': name, 'output': result.stdout}

    def list_sandboxes(self) -> List[str]:
        """列出 DB 中所有沙盒名称。"""
        return self._list_sandbox_names()

    # ── 终端专用 ─────────────────────────────────────────────────

    def allocate_for_terminal(self, terminal_id: str,
                              cpu: int = 0, mem: str = "0",
                              device_num: int = 0,
                              port: int = None) -> Optional[dict]:
        """为终端会话分配沙盒。

        根据 device_num 从空闲设备池中分配指定数量的设备节点，
        沙盒命名为 term_<terminal_id>。

        Args:
            terminal_id: 终端唯一标识（如 ttyd 的 PID）
            cpu:         CPU 核数 (0=不限)
            mem:         内存限制 (如 "512M", "2G", "0"=不限)
            device_num:  要分配的设备数量 (0=不分配设备)

        Returns:
            成功返回 {'sandbox_name': str, 'devices': [str]}，失败返回 None。
        """
        sandbox_name = f"term_{terminal_id}"

        devices = []
        if device_num > 0:
            # 1. 通过 Node_Manager 校验系统空闲设备数
            if not self._validate_idle_count(device_num):
                return None

            # 2. 从 DB 计算实际空闲设备
            free = self._get_free_devices()
            if len(free) < device_num:
                logger.warning("设备不足: 需要 %s 个, DB 空闲 %s 个 (free=%s)",
                               device_num, len(free), free)
                return None

            devices = free[:device_num]
            logger.info("分配设备: %s (从空闲池 %s 中选取 %s 个)", devices, free, device_num)

        success = self.create_sandbox(
            sandbox_name,
            cpu=cpu,
            mem=mem,
            devices=devices if devices else None,
            port=port,
        )
        if not success:
            return None

        return {'sandbox_name': sandbox_name, 'devices': devices}

    # ── 孤儿清理 ─────────────────────────────────────────────────

    @staticmethod
    def _get_active_ports() -> set:
        """扫描系统所有 ESTABLISHED 状态的 TCP 端口，返回活跃端口集合。

        一次扫描，供后续多个沙盒复用，避免重复调用 psutil.net_connections()。
        """
        active = set()
        try:
            for conn in psutil.net_connections(kind='tcp'):
                if conn.laddr and conn.status == 'ESTABLISHED':
                    active.add(conn.laddr.port)
        except Exception:
            pass
        return active

    def cleanup_orphaned(self) -> int:
        """清理所有进程已退出的沙盒，释放设备资源。
        对于 term_* 沙盒，增加终端超时未连接检查：若进程存活但端口无
        ESTABLISHED 连接且超过 terminal_idle_timeout 秒，则强制杀进程并清理。

        Returns:
            清理的沙盒数量。
        """
        from executor.port_pool import Port_Pool_Manager

        logger.debug("正在扫描活跃端口...")
        active_ports = self._get_active_ports()  # 一次扫描，所有 term_* 沙盒复用
        logger.debug("扫描完成，活跃端口: %s", active_ports)
        cleaned = 0
        sandbox_names = self._list_sandbox_names()
        logger.debug("共 %s 个沙盒待检查", len(sandbox_names))
        for name in sandbox_names:
            record = self.db.get_sandbox(name)
            if not record:
                continue

            pids = record.get('pids', [])
            if not pids:
                # 没有进程记录的沙盒：检查 cgroup.procs 是否为空
                procs_file = os.path.join(self._cg_path(name), 'cgroup.procs')
                try:
                    with open(procs_file) as f:
                        content = f.read().strip()
                    if not content:
                        logger.info("清理空沙盒 '%s' (无进程)", name)
                        port = record.get('port')
                        if port:
                            Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
                        self.destroy_sandbox(name)
                        cleaned += 1
                except (OSError, IOError):
                    # cgroup 目录可能已不存在
                    port = record.get('port')
                    if port:
                        Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
                    self.db.delete_sandbox(name)
                    cleaned += 1
                continue

            # 检查记录的 PID 是否还活着
            all_dead = True
            for pid in pids:
                try:
                    os.kill(pid, 0)
                    all_dead = False
                    break
                except OSError:
                    pass

            if all_dead:
                logger.info("清理孤儿沙盒 '%s' (所有 PID 已退出)", name)
                port = record.get('port')
                if port:
                    Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
                self.destroy_sandbox(name)
                cleaned += 1
            elif name.startswith('term_'):
                # 终端沙盒：进程还活着但端口无 ESTABLISHED 连接 → 无人使用，直接清理
                port = record.get('port')
                if port and port not in active_ports:
                    logger.info("终端沙盒 '%s' 端口 %s 无活跃连接，清理", name, port)
                    for pid in pids:
                        try:
                            os.kill(pid, signal.SIGTERM)
                        except OSError:
                            pass
                    # 等待进程优雅退出
                    time.sleep(0.3)
                    for pid in pids:
                        try:
                            os.kill(pid, signal.SIGKILL)
                        except OSError:
                            pass
                    Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
                    self.destroy_sandbox(name)
                    cleaned += 1

        return cleaned


    # ── 定时收尸（Reaper） ───────────────────────────────────────

    def _reaper_loop(self):
        """后台收尸线程主循环。每隔 sandbox_reaper_interval 秒执行一次收尸。"""
        interval = int(os.getenv('sandbox_reaper_interval', '30'))
        logger.info("定时收尸已启动 (间隔=%ss)", interval)

        while True:
            try:
                t0 = time.monotonic()
                logger.debug("开始收尸扫描...")
                cleaned = self.cleanup_orphaned()
                remaining = len(self._list_sandbox_names())
                if cleaned > 0:
                    logger.info("本轮收尸完成: 清理=%s, 剩余沙盒=%s", cleaned, remaining)
                else:
                    logger.debug("本轮收尸完成: 清理=%s, 剩余沙盒=%s", cleaned, remaining)
                # 用实际耗时修正 sleep，保证间隔稳定
                elapsed = time.monotonic() - t0
                sleep_time = max(0, interval - elapsed)
                time.sleep(sleep_time)
            except Exception as e:
                logger.error("收尸异常: %s", e, exc_info=True)
                time.sleep(interval)

    def start_reaper(self):
        """启动后台收尸线程（daemon 线程，随主进程退出）。"""
        t = threading.Thread(target=self._reaper_loop, daemon=True, name='sbx-reaper')
        t.start()
        return t
