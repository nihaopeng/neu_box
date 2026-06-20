#!/usr/bin/env python3
"""节点状态测试 — 验证节点在线、资源数值合法。"""

from common import (
    get, post, assert_ok, assert_eq, assert_gt, run_tests, get_gpunode_id,
)


def test_all_nodes_online_and_valid():
    """所有节点在线，资源数值合法。"""
    _, data = post("/nodes/get_all_nodes", {})
    assert_ok(_, "获取节点列表失败")
    nodes = data.get("nodes", [])
    assert_gt(len(nodes), 0, "节点列表为空")

    for node in nodes:
        name = node.get("name", "?")
        nid = node.get("node_id", "?")
        assert_eq(node.get("status"), "online", f"{name} 不在线")
        assert_gt(node.get("total_cpu", 0), 0, f"{name} total_cpu 为 0")
        assert_gt(node.get("total_mem", 0), 0, f"{name} total_mem 为 0")
        assert node.get("idle_cpu", -1) >= 0, f"{name} idle_cpu 为负数"
        assert node.get("idle_devices", 0) <= node.get("total_devices", 0), \
            f"{name} idle_devices > total_devices"
        print(f"    {name}: CPU={node['total_cpu']}核 idle={node['idle_cpu']}%, "
              f"设备={node['idle_devices']}/{node['total_devices']}, "
              f"Mem={node['idle_mem']//(1024**3)}GB 空闲", flush=True)


def test_single_node_status():
    """单个节点 GET /nodes/<id>/status 返回字段完整。"""
    gpu_id = get_gpunode_id()
    _, data = get(f"/nodes/{gpu_id}/status")
    assert_ok(_, f"获取节点状态失败: {data}")
    if "error" in data:
        print(f"    节点离线: {data.get('error')}", flush=True)
        return
    for f in ("status", "total_cpu", "idle_cpu", "total_mem", "idle_mem",
              "total_devices", "idle_devices", "active_sandboxes"):
        assert f in data, f"缺少字段 {f}"
    assert_gt(data.get("total_cpu", 0), 0, "total_cpu 不应为 0")
    print(f"    CPU {data['total_cpu']}核, "
          f"设备 {data['idle_devices']}/{data['total_devices']}, "
          f"Mem {data['idle_mem']//(1024**3)}GB idle, "
          f"sandboxes={data['active_sandboxes']}", flush=True)


def test_node_config_list():
    """config.json 节点列表可读。"""
    _, data = get("/nodes/config")
    assert_ok(_, "获取 config 失败")
    nodes = data.get("nodes", [])
    assert_gt(len(nodes), 0, "config 节点列表为空")
    for n in nodes:
        assert "name" in n and "host" in n and "port" in n, \
            f"节点缺少 name/host/port: {n}"
    print(f"    {len(nodes)} 个配置节点", flush=True)


TESTS = [
    ("所有节点在线",    test_all_nodes_online_and_valid),
    ("单个节点状态",    test_single_node_status),
    ("config 列表",     test_node_config_list),
]

if __name__ == "__main__":
    run_tests(TESTS, "节点状态测试")
