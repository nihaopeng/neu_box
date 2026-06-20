#!/usr/bin/env python3
"""实验记录测试 — CRUD、搜索、批量操作。"""

import time
from common import (
    get, post, put, delete,
    assert_ok, assert_eq, assert_gt, assert_in, run_tests,
)


def test_create_read_update_delete():
    """创建 → 查询 → 更新 → 删除 全流程。"""
    # 创建
    ts = int(time.time())
    _, d = post("/experiments/", {
        "title": f"__test_crud_{ts}",
        "blocks": [
            {"type": "text", "content": "## 测试笔记\n自动测试内容。"},
            {"type": "task", "command": "nvidia-smi", "log": "GPU info..."},
        ],
        "tags": ["test", "auto"],
        "created_by": "auto_test",
    })
    assert_ok(_, f"创建失败: {d.get('error',_)}")
    exp_id = d.get("id") or d.get("exp_id")
    assert exp_id, f"未返回 ID: {d}"
    print(f"    创建: {exp_id}", flush=True)

    # 查询详情
    _, exp = get(f"/experiments/{exp_id}")
    assert_ok(_, f"查询失败: {exp.get('error',_)}")
    assert_eq(len(exp.get("blocks", [])), 2, "blocks 数量不对")
    assert_in("test", exp.get("tags", []), "tags 不包含 test")

    # 更新
    _, d = put(f"/experiments/{exp_id}", {
        "title": f"__test_crud_{ts}_updated",
        "blocks": [{"type": "text", "content": "更新后"}],
        "tags": ["test", "updated"],
    })
    assert_ok(_, f"更新失败: {d.get('error',_)}")

    # 删除
    _, d = delete(f"/experiments/{exp_id}")
    assert_ok(_, f"删除失败: {d.get('error',_)}")
    print(f"    删除成功", flush=True)


def test_search_by_title_and_tag():
    """搜索：标题和标签都能匹配。"""
    import urllib.parse
    ts = int(time.time())
    keyword = f"unique_search_keyword_{ts}"
    _, d = post("/experiments/", {
        "title": f"__test_{keyword}",
        "blocks": [{"type": "text", "content": "搜索测试"}],
        "tags": ["cat", "search_test"],
        "created_by": "auto_test",
    })
    assert_ok(_, f"创建失败: {d.get('error',_)}")
    exp_id = d.get("id") or d.get("exp_id")

    # 按标题关键词搜（URL 编码避免特殊字符问题）
    _, data = get(f"/experiments/?search={urllib.parse.quote(keyword)}")
    exps = data.get("experiments", [])
    assert_gt(len(exps), 0, f"按标题搜索不到 '{keyword}'")

    # 按标签搜
    _, data = get(f"/experiments/?search=search_test")
    exps = data.get("experiments", [])
    assert_gt(len(exps), 0, "按标签搜索不到")

    # 按创建者搜
    _, data = get(f"/experiments/?created_by=auto_test")
    assert_gt(len(data.get("experiments", [])), 0, "按创建者搜不到")

    delete(f"/experiments/{exp_id}")
    print(f"    搜索/过滤验证通过", flush=True)


def test_empty_experiment():
    """空内容实验的创建和删除。"""
    ts = int(time.time())
    _, d = post("/experiments/", {
        "title": f"__test_empty_{ts}",
        "blocks": [],
        "tags": [],
        "created_by": "auto_test",
    })
    assert_ok(_, f"创建空实验失败: {d.get('error',_)}")
    exp_id = d.get("id") or d.get("exp_id")

    _, exp = get(f"/experiments/{exp_id}")
    assert_eq(len(exp.get("blocks", [])), 0, "空实验 blocks 应为空")

    delete(f"/experiments/{exp_id}")
    print(f"    空实验 OK", flush=True)


TESTS = [
    ("CRUD 全流程",   test_create_read_update_delete),
    ("搜索与过滤",    test_search_by_title_and_tag),
    ("空实验",        test_empty_experiment),
]

if __name__ == "__main__":
    run_tests(TESTS, "实验记录测试")
