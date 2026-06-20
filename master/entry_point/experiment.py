"""实验管理 API — 块（block）式实验笔记。

实验由 blocks 数组组成，按顺序渲染:
  text block:  {"type":"text","content":"..."}
  task block:  {"type":"task","task_id":"...","node_id":"...",
                "command":"...","log":{...}}
"""

import logging

from flask import Blueprint, request

from src_manager.db import Database

experiment_bp = Blueprint('experiment', __name__)
db = Database.get_instance()
logger = logging.getLogger('master.experiment')


@experiment_bp.route('/', methods=['POST'])
def create_experiment():
    """创建实验。

    请求体:
      { "title": "...", "blocks": [...], "tags": [...], "created_by": "..." }
    """
    data = request.get_json(silent=True) or {}
    title = (data.get('title') or '').strip()
    if not title:
        return {'error': '实验标题不能为空'}, 400
    exp_id = db.create_experiment(
        title=title,
        blocks=data.get('blocks') or [],
        tags=data.get('tags') or [],
        created_by=(data.get('created_by') or '').strip(),
    )
    return {'id': exp_id, 'message': '实验记录已创建'}, 201


@experiment_bp.route('/', methods=['GET'])
def list_experiments():
    search = (request.args.get('search') or '').strip()
    tag = (request.args.get('tag') or '').strip()
    created_by = (request.args.get('created_by') or '').strip()
    limit = request.args.get('limit', 100, type=int)
    experiments = db.list_experiments(search=search, tag=tag,
                                      created_by=created_by, limit=limit)
    return {'experiments': experiments, 'total': len(experiments)}, 200


@experiment_bp.route('/<exp_id>', methods=['GET'])
def get_experiment(exp_id: str):
    exp = db.get_experiment(exp_id)
    if not exp:
        return {'error': '实验记录不存在'}, 404
    return exp, 200


@experiment_bp.route('/<exp_id>', methods=['PUT'])
def update_experiment(exp_id: str):
    """全量更新实验（含 blocks）。"""
    exp = db.get_experiment(exp_id)
    if not exp:
        return {'error': '实验记录不存在'}, 404
    data = request.get_json(silent=True) or {}
    db.update_experiment(
        exp_id,
        title=data.get('title'),
        blocks=data.get('blocks'),
        tags=data.get('tags'),
    )
    return {'message': '已保存'}, 200


@experiment_bp.route('/<exp_id>', methods=['DELETE'])
def delete_experiment(exp_id: str):
    if not db.delete_experiment(exp_id):
        return {'error': '实验记录不存在'}, 404
    return {'message': '已删除'}, 200
