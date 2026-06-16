import os
import subprocess
import time

from flask import Blueprint, request
from executor.port_pool import Port_Pool_Manager
from executor.sbx_manager import SbxManager

terminal_bp = Blueprint('terminal', __name__)


@terminal_bp.route('/create', methods=['POST'])
def create():
    # 解析请求中的设备/资源需求
    body = request.get_json(silent=True) or {}
    device_num = body.get('device_num', 0)
    sandbox_cpu = body.get('cpu', 0)
    sandbox_mem = body.get('mem', '0')

    # 1. 临时占住一个端口，传入一个虚拟的 PID (比如 -1)
    # 因为此时进程还没启动，还没拿到真实 PID
    port = Port_Pool_Manager.get_Port_Pool_Manager().acquire_port()
    if not port:
        return {'error': '服务器繁忙', 'details': '防火墙端口池已耗尽，请稍后再试'}, 503

    try:
        # 2. 启动 ttyd，绑定拿到的黄金端口
        process = subprocess.Popen(
            ['ttyd', '-p', str(port), '-W', '-q', 'bash'],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # 3. 稍微等待，确保没秒退
        time.sleep(0.1)
        if process.poll() is not None:
            # 如果启动失败，立刻归还端口
            Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
            return {'error': '终端启动失败', 'details': 'ttyd 异常退出'}, 500

        # 4. 分配沙盒并加入 ttyd 进程
        sbx = SbxManager.get_instance()
        result = sbx.allocate_for_terminal(
            str(process.pid),
            cpu=sandbox_cpu,
            mem=sandbox_mem,
            device_num=device_num,
        )
        if result is None:
            # 沙盒创建失败 → 清理 ttyd 并归还端口
            try:
                process.terminate()
                process.wait(timeout=5)
            except Exception:
                process.kill()
            Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
            return {'error': '沙盒分配失败', 'details': '无法创建 cgroup 沙盒，请检查系统配置'}, 500

        sandbox_name = result['sandbox_name']
        allocated_devices = result['devices']

        # 将 ttyd 进程加入沙盒
        if not sbx.join_sandbox(sandbox_name, process.pid):
            # 加入失败 → 清理沙盒、ttyd、端口
            sbx.destroy_sandbox(sandbox_name)
            try:
                process.terminate()
                process.wait(timeout=5)
            except Exception:
                process.kill()
            Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
            return {'error': '沙盒加入失败', 'details': '无法将进程加入沙盒'}, 500

        # 5. 启动成功，更新池子里记录的真实 PID

        # 自动获取本机 IP 地址（适配多网卡/容器环境）,使用ip addr show 命令获取非回环地址
        ip = os.getenv('HOST_IP')  # 优先使用环境变量指定的 IP

        return {
            'message': '终端创建成功',
            'sandbox_id': process.pid,
            'sandbox_name': sandbox_name,
            'allocated_devices': allocated_devices,
            'assigned_port': port,
            'terminal_url': f'http://{ip}:{port}'
        }, 201

    except Exception as e:
        # 万一代码抛出未知异常，确保端口一定会被释放（防死锁/防堆积）
        Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
        return {'error': '系统内部错误', 'details': str(e)}, 500
