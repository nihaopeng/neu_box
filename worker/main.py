import flask
import os
from dotenv import load_dotenv

load_dotenv()

from executor.terminal import terminal_bp
from executor.status import status_bp

app = flask.Flask(__name__)

app.register_blueprint(terminal_bp, url_prefix='/terminal')
# /status 直接挂载到根路径，方便 master 查询
app.register_blueprint(status_bp)


@app.route('/')
def home():
    return flask.send_from_directory('static', 'index.html')


if __name__ == '__main__':
    app.run(host=os.getenv('listen'), port=int(os.getenv('port')))
