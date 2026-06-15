import os
import subprocess
import time

from flask import Blueprint
from executor.port_pool import Port_Pool_Manager  # 引入刚才写好的池子

terminal_bp = Blueprint('terminal', __name__)

@terminal_bp.route('/create', methods=['POST'])
def create():
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
            # preexec_fn=os.setpgrp # ⚠️ 加上这行，让 ttyd 脱离 Python 的管辖，死后由操作系统自动回收
        )
        
        # 3. 稍微等待，确保没秒退
        time.sleep(0.1)
        if process.poll() is not None:
            # 如果启动失败，立刻归还端口
            Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
            return {'error': '终端启动失败', 'details': 'ttyd 异常退出'}, 500

        # 4. 启动成功，更新池子里记录的真实 PID

        # 自动获取本机 IP 地址（适配多网卡/容器环境）,使用ip addr show 命令获取非回环地址
        ip = os.getenv('HOST_IP')  # 优先使用环境变量指定的 IP

        return {
            'message': '终端创建成功',
            'sandbox_id': process.pid,
            'assigned_port': port,
            'terminal_url': f'http://{ip}:{port}'
        }, 201

    except Exception as e:
        # 万一代码抛出未知异常，确保端口一定会被释放（防死锁/防堆积）
        Port_Pool_Manager.get_Port_Pool_Manager().release_port(port)
        return {'error': '系统内部错误', 'details': str(e)}, 500