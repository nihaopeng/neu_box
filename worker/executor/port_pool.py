import logging
import os
import socket
import threading
from typing import Optional
import psutil

logger = logging.getLogger(__name__)


class Port_Pool_Manager:
    def __init__(self):
        self.start_port = os.getenv('port_pool_start', '59081')
        self.end_port = os.getenv('port_pool_end', '59100')
        self.start_port = int(self.start_port)
        self.end_port = int(self.end_port)

        # 记录需要管理的所有合法端口集合
        self.all_managed_ports = set(range(self.start_port, self.end_port + 1))
        # 当前真正可用于分配的端口集合
        self.available_ports = set(range(self.start_port, self.end_port + 1))

        # 线程锁，保证分配时的线程安全
        self._lock = threading.Lock()
        # 后台收尸线程是否已启动
        self._reaper_started = False
        self._reaper_lock = threading.Lock()

    @classmethod
    def get_Port_Pool_Manager(cls) -> 'Port_Pool_Manager':
        """单例模式，确保全局只有一个端口池实例"""
        if not hasattr(cls, '_instance'):
            cls._instance = cls()
        return cls._instance

    def _is_port_actually_free(self, port: int) -> bool:
        """物理检查：尝试绑定，看端口是否真的空闲"""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            try:
                s.bind(('127.0.0.1', port))
                return True
            except OSError:
                return False

    def _reap_zombie_children(self):
        """
        直接扫描当前进程的所有子进程，找出僵尸并收尸。
        不依赖 TCP 连接扫描 —— 僵尸进程的 socket 早已关闭，
        所以 psutil.net_connections() 根本找不到它们。
        """
        try:
            for child in psutil.Process().children(recursive=True):
                try:
                    if child.status() == psutil.STATUS_ZOMBIE:
                        os.waitpid(child.pid, os.WNOHANG)
                        logger.debug("已收尸僵尸子进程 PID=%s", child.pid)
                except (psutil.NoSuchProcess, ProcessLookupError, OSError):
                    pass
        except Exception:
            pass

    def acquire_port(self) -> Optional[int]:
        """申请一个可用端口。如果满了，内部会自动触发收尸和全盘校准"""
        with self._lock:
            current_available = list(self.available_ports)

            for port in current_available:
                self.available_ports.remove(port)
                if self._is_port_actually_free(port):
                    return port
                else:
                    continue

            # 池子空了，先尝试收尸再重新校准
            logger.warning("端口池告急！触发【收尸 + 端口同步】...")
            self._reap_zombie_children()
            self._sync_reclaim_and_reap_zombies_with_lock()

            if self.available_ports:
                return self.available_ports.pop()

            return None  # 真的满了

    def release_port(self, port: int):
        """手动释放/归还端口"""
        with self._lock:
            if self.start_port <= port <= self.end_port:
                self.available_ports.add(port)

    def _sync_reclaim_and_reap_zombies_with_lock(self):
        """
        核心实现：在端口池内部，通过对系统级网络流和进程状态的综合审计，
        找出可回收的端口并清理僵尸进程。
        """
        logger.debug("开始执行系统级网络与进程交叉审计...")

        active_system_ports = set()

        # 1. 扫描系统当前的动态 TCP 连接流
        try:
            for conn in psutil.net_connections(kind='tcp'):
                if not (conn.laddr and conn.laddr.port):
                    continue

                port = conn.laddr.port
                if port not in self.all_managed_ports:
                    continue

                if conn.pid:
                    try:
                        proc = psutil.Process(conn.pid)

                        if proc.status() == psutil.STATUS_ZOMBIE:
                            logger.debug("发现端口 %s 被僵尸进程(PID: %s)霸占", port, conn.pid)
                            try:
                                proc.wait(timeout=0.1)
                                logger.debug("僵尸进程 %s 已成功火化", conn.pid)
                            except (psutil.NoSuchProcess, psutil.TimeoutExpired, OSError):
                                pass
                            continue

                    except (psutil.NoSuchProcess, psutil.AccessDenied,
                            psutil.TimeoutExpired, OSError):
                        continue

                    # 进程还活着，才认为这个端口被真正占用了
                    active_system_ports.add(port)
                # else: conn.pid 为 None（如 TIME_WAIT），不贸然标记为活跃，
                # 交给下面的 _is_port_actually_free() 物理检测来裁决

        except Exception as e:
            logger.error("审计系统连接失败: %s", e)
            return

        # 2. 重新对齐、重建可用端口池
        new_available = set()
        for port in self.all_managed_ports:
            if port not in active_system_ports:
                if self._is_port_actually_free(port):
                    new_available.add(port)
                else:
                    logger.debug("端口 %s 网络未登记，但物理绑定失败（可能正处于 TIME_WAIT 释放中）", port)

        self.available_ports = new_available
        logger.info("智能审计完成！当前真正空闲可分配的端口数: %s", len(self.available_ports))

    def force_sync_and_reclaim(self):
        """外部公开接口：允许手动或定时器调用"""
        with self._lock:
            self._reap_zombie_children()
            self._sync_reclaim_and_reap_zombies_with_lock()