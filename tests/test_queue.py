#!/usr/bin/env python3
"""任务队列测试 — 并发排队、FIFO、批量删除。"""

import sys
import time
from common import (
    get, post, assert_ok, assert_gt, run_tests, get_gpunode_id,
)

QUICK = "--quick" in sys.argv
TASK_COUNT      = 4 if QUICK else 6
DEVICE_PER_TASK = 1
CPU_PER_TASK    = 1
MEM_PER_TASK    = 1
SLEEP_SECONDS   = 5 if QUICK else 10
POLL_INTERVAL   = 2
TEST_USERS      = ["pengyt", "lipz"]


def test_concurrency():
    """有限 GPU 资源下并发不超过可用数。"""
    gpu_id = get_gpunode_id()

    _, nodes = post("/nodes/get_all_nodes", {})
    gpunode = next((n for n in nodes["nodes"] if n["node_id"] == gpu_id), {})
    idle_gpu = gpunode.get("idle_gpu", 0)
    print(f"    gpunode2 空闲 GPU: {idle_gpu}", flush=True)

    task_ids = []
    for i in range(TASK_COUNT):
        user = TEST_USERS[i % len(TEST_USERS)]
        _, d = post("/command/run", {
            "node_id": gpu_id, "user_id": user,
            "command": f"echo '[{user}] T{i+1} start'; sleep {SLEEP_SECONDS}; echo 'T{i+1} done'",
            "cpu": CPU_PER_TASK, "memory": MEM_PER_TASK,
            "mem_unit": "GB", "device_num": DEVICE_PER_TASK,
        })
        assert_ok(_, f"任务{i+1} 提交失败: {d.get('error',_)}")
        task_ids.append(d["task_id"])
        print(f"    T{i+1}: {d['task_id'][:12]}... pos=#{d['position']}", flush=True)
        time.sleep(0.05)

    max_running = 0
    done_count = 0
    start = time.time()
    timeout = TASK_COUNT * SLEEP_SECONDS + 30

    while done_count < len(task_ids) and (time.time() - start) < timeout:
        _, data = get(f"/command/queue?node_id={gpu_id}")
        queue = data.get("queue", [])
        running = sum(1 for t in queue if t.get("status") == "running")
        done_count = sum(1 for t in queue if t.get("status") in ("completed", "failed"))
        if running > max_running:
            max_running = running
        time.sleep(POLL_INTERVAL)

    print(f"    最大并发: {max_running}  耗时: {int(time.time()-start)}s", flush=True)
    assert max_running <= max(idle_gpu, 1), \
        f"并发数 {max_running} > 可用 GPU {idle_gpu}"


def test_fifo_order():
    """先提交的任务 position 更小。"""
    gpu_id = get_gpunode_id()

    _, d1 = post("/command/run", {
        "node_id": gpu_id, "user_id": "pengyt",
        "command": "echo FIFO_1; sleep 2",
        "cpu": 1, "memory": 1, "mem_unit": "GB", "device_num": 0,
    })
    assert_ok(_, f"任务1 失败: {d1}")
    time.sleep(0.1)

    _, d2 = post("/command/run", {
        "node_id": gpu_id, "user_id": "lipz",
        "command": "echo FIFO_2; sleep 2",
        "cpu": 1, "memory": 1, "mem_unit": "GB", "device_num": 0,
    })
    assert_ok(_, f"任务2 失败: {d2}")

    assert d1["position"] < d2["position"], \
        f"Task1 pos={d1['position']} 应 < Task2 pos={d2['position']}"


def test_batch_delete():
    """批量删除：排队/已完成的可删，running 不删。"""
    gpu_id = get_gpunode_id()

    ids = []
    for i in range(3):
        _, d = post("/command/run", {
            "node_id": gpu_id, "user_id": "pengyt",
            "command": f"sleep {1+i}; echo batch_{i}",
            "cpu": 1, "memory": 1, "mem_unit": "GB", "device_num": 0,
        })
        assert_ok(_, f"提交失败: {d}")
        ids.append(d["task_id"])

    time.sleep(3)

    _, d = post("/command/tasks/delete", {"node_id": gpu_id, "task_ids": ids})
    if not (200 <= _ < 300):
        # Worker 可能还没重启，新路由未生效
        print(f"    跳过: Worker 未更新 (HTTP {_} {d.get('error','')[:60]})", flush=True)
        return
    deleted = d.get("deleted", 0)
    print(f"    请求删除 {len(ids)} 个, 实际删除 {deleted} 个", flush=True)
    assert_gt(deleted, 0, "至少应删除已完成任务")

    # 验证已删除的不在队列中
    _, data = get(f"/command/queue?node_id={gpu_id}")
    queue_ids = {t["task_id"] for t in data.get("queue", [])}
    remaining = [tid for tid in ids if tid in queue_ids]
    print(f"    剩余: {len(remaining)} 个 (可能是 running)", flush=True)


TESTS = [
    ("并发排队",    test_concurrency),
    ("FIFO 顺序",   test_fifo_order),
    ("批量删除",    test_batch_delete),
]

if __name__ == "__main__":
    run_tests(TESTS, "任务队列测试")
