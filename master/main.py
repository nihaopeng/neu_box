import json
import logging
import os
import time
from logging.handlers import RotatingFileHandler

import flask

# ── 集中日志配置 ─────────────────────────────────────────────
_log_dir = os.path.join(os.path.dirname(__file__), 'logs')
os.makedirs(_log_dir, exist_ok=True)

# 解析日志级别（兼容 .env 中带引号的写法）
_raw_level = os.getenv('LOG_LEVEL', 'INFO').strip().strip('"').strip("'").upper()
_log_level = getattr(logging, _raw_level, logging.INFO)

_log_fmt = logging.Formatter(
    '%(asctime)s [%(name)s] %(levelname)s %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S',
)

# 按启动时间命名日志文件，每次重启写新文件
_start_ts = time.strftime('%Y%m%d-%H%M%S')
_log_file = os.path.join(_log_dir, f'master-{_start_ts}.log')

_file_handler = RotatingFileHandler(
    _log_file, maxBytes=10 * 1024 * 1024, backupCount=5,
)
_file_handler.setFormatter(_log_fmt)

_console_handler = logging.StreamHandler()
_console_handler.setFormatter(_log_fmt)

# 文件、控制台、root logger 统一使用 LOG_LEVEL
_file_handler.setLevel(_log_level)
_console_handler.setLevel(_log_level)
_root_logger = logging.getLogger()
_root_logger.setLevel(_log_level)
_root_logger.addHandler(_file_handler)
_root_logger.addHandler(_console_handler)

logging.getLogger('master').info('Master 启动，日志级别=%s，日志文件=%s', _raw_level, _log_file)


class Config:
    def __init__(self):
        self.config = json.load(open('config.json'))

    def get_instance(self):
        if not hasattr(self, '_instance'):
            self._instance = Config()
        return self._instance

from entry_point.terminal import terminal_bp
from entry_point.command import command_bp
from entry_point.nodes import nodes_bp
from entry_point.experiment import experiment_bp
from src_manager.nodes_pool import Nodes_Pool

app = flask.Flask(__name__, static_folder='static', static_url_path='/static')

app.register_blueprint(terminal_bp, url_prefix='/terminal')
app.register_blueprint(command_bp, url_prefix='/command')
app.register_blueprint(nodes_bp, url_prefix='/nodes')
app.register_blueprint(experiment_bp, url_prefix='/experiments')


@app.route('/')
def home():
    return flask.send_from_directory('static', 'index.html')


if __name__ == '__main__':
    ip = Config().config.get('master', {}).get('ip', '0.0.0.0')
    port = int(Config().config.get('master', {}).get('port', 25565))

    # 启动后台轮询，定期查询所有 worker 节点状态
    Nodes_Pool.get_nodes_pool().start_polling(interval=15)

    app.run(host=ip, port=port)