import flask
from src_manager.nodes_pool import Nodes_Pool

nodes_bp = flask.Blueprint('nodes', __name__)


@nodes_bp.route('/get_all_nodes', methods=['POST'])
def get_all_nodes():
    """返回所有已注册节点的列表及当前状态，供前端选择器使用。"""
    pool = Nodes_Pool.get_nodes_pool()
    nodes_info = pool.get_all_nodes()
    return {'nodes': nodes_info}, 200


@nodes_bp.route('/<node_id>/status', methods=['GET'])
def query_node_status(node_id: str):
    """Master 主动查询某个 worker 的实时资源状态。"""
    pool = Nodes_Pool.get_nodes_pool()
    try:
        result = pool.query_node_status(node_id)
        return result, 200
    except ValueError as e:
        return {'error': str(e)}, 404
