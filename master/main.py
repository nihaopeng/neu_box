import json
import flask

class Config:
    def __init__(self):
        self.config = json.load(open('config.json'))

    def get_instance(self):
        if not hasattr(self, '_instance'):
            self._instance = Config()
        return self._instance

from entry_point.terminal import terminal_bp
from entry_point.nodes import nodes_bp
from src_manager.nodes_pool import Nodes_Pool

app = flask.Flask(__name__, static_folder='static', static_url_path='/static')

app.register_blueprint(terminal_bp, url_prefix='/terminal')
app.register_blueprint(nodes_bp, url_prefix='/nodes')


@app.route('/')
def home():
    return flask.send_from_directory('static', 'index.html')


if __name__ == '__main__':
    ip = Config().config.get('master', {}).get('ip', '0.0.0.0')
    port = int(Config().config.get('master', {}).get('port', 25565))

    # 启动后台轮询，定期查询所有 worker 节点状态
    Nodes_Pool.get_nodes_pool().start_polling(interval=15)

    app.run(host=ip, port=port)