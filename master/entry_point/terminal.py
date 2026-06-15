from flask import Blueprint, request
from src_manager.nodes_pool import Nodes_Pool

terminal_bp = Blueprint('terminal', __name__)

@terminal_bp.route('/create', methods=['POST'])
def create():
    data = request.get_json()

    if not data:
        return {'error': '请求体不能为空'}, 400

    node_id = data.get('node_id')
    cpu = data.get('cpu', 1)
    memory = data.get('memory', 1)
    mem_unit = data.get('mem_unit', 'GB')
    gpu = data.get('gpu', 0)
    npu = data.get('npu', 0)

    # 参数校验
    errors = []
    if not isinstance(cpu, int) or cpu < 1:
        errors.append('CPU 至少为 1')
    if not isinstance(memory, int) or memory < 1:
        errors.append('内存至少为 1')
    if mem_unit not in ('MB', 'GB'):
        errors.append('内存单位只能是 MB 或 GB')
    if not isinstance(gpu, int) or gpu < 0:
        errors.append('GPU 不能为负数')
    if not isinstance(npu, int) or npu < 0:
        errors.append('NPU 不能为负数')

    if errors:
        return {'error': '; '.join(errors)}, 400

    req ={
        'cpu': cpu,
        'memory': memory,
        'mem_unit': mem_unit,
        'gpu': gpu,
        'npu': npu
    }

    response = Nodes_Pool.get_nodes_pool().req_node(node_id, req)

    # 暂返模拟响应
    return response.json(), response.status_code