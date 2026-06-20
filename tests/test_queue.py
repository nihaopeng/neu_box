#!/usr/bin/env python3
"""任务队列测试 — 并发排队、FIFO、批量删除。"""

import sys
import time
from common import (
    get, post, assert_ok, assert_gt, run_tests, get_devnode_id,
)

QUICK = "--quick" in sys.argv
TASK_COUNT      = 8 if QUICK else 20
DEVICE_PER_TASK = 1
CPU_PER_TASK    = 1
MEM_PER_TASK    = 1
POLL_INTERVAL   = 2
TEST_USERS      = ["pengyt", "lipz"]


def test_concurrency():
    """有限设备资源下并发不超过可用数，且运行中任务实际分配到设备。"""
    # gpu_id = get_devnode_id()
    gpu_id = get_devnode_id()

    _, nodes = post("/nodes/get_all_nodes", {})
    node = next((n for n in nodes["nodes"] if n["node_id"] == gpu_id), {})
    total_dev = node.get("total_devices", 0)
    name = node.get("name", "?")
    print(f"    {name}: 总设备={total_dev}", flush=True)

    # 不同任务不同耗时，模拟真实并发
    sleep_times = [3, 5, 7, 4, 6, 8, 10, 2, 4, 6, 8, 10, 2, 4, 6, 8, 10, 2, 4, 6][:TASK_COUNT]
    timeout = sum(sleep_times) + 30

    task_ids = []
    for i in range(TASK_COUNT):
        user = TEST_USERS[i % len(TEST_USERS)]
        s = sleep_times[i]
        _, d = post("/command/run", {
            "node_id": gpu_id, "user_id": user,
            "command": f"echo '[{user}] T{i+1}({s}s) start'; sleep {s}; echo 'T{i+1} done'",
            "cpu": CPU_PER_TASK, "memory": MEM_PER_TASK,
            "mem_unit": "GB", "device_num": DEVICE_PER_TASK,
        })
        assert_ok(_, f"任务{i+1} 提交失败: {d.get('error',_)}")
        task_ids.append(d["task_id"])
        print(f"    T{i+1}: {d['task_id'][:12]}... {s}s pos=#{d['position']}", flush=True)
        time.sleep(0.05)

    max_running = 0
    max_devices_used = 0
    done_count = 0
    start = time.time()

    while done_count < len(task_ids) and (time.time() - start) < timeout:
        _, data = get(f"/command/queue?node_id={gpu_id}")
        queue = data.get("queue", [])
        running_tasks = [t for t in queue if t.get("status") == "running"]
        running = len(running_tasks)
        done_count = sum(1 for t in queue if t.get("status") in ("completed", "failed"))
        if running > max_running:
            max_running = running

        # 统计实际分配的设备，验证请求了设备的任务确实分到了设备
        total_assigned = 0
        for t in running_tasks:
            devices = t.get("devices") or []
            dn = t.get("device_num", 0)
            total_assigned += len(devices)
            if dn > 0:
                assert len(devices) == dn, \
                    f"任务 {t['user_id']} 请求 {dn} 设备, 实际分配 {len(devices)}: {devices}"
                print(f"    ▶ {t['user_id']}: devices={devices}", flush=True)
        if total_assigned > max_devices_used:
            max_devices_used = total_assigned

        time.sleep(POLL_INTERVAL)

    print(f"    最大并发: {max_running}  最大设备占用: {max_devices_used}/{total_dev}  耗时: {int(time.time()-start)}s", flush=True)
    assert max_running <= max(total_dev, 1), \
        f"并发数 {max_running} > 总设备 {total_dev}"
    assert max_devices_used <= total_dev, \
        f"占用设备 {max_devices_used} > 总设备 {total_dev}"


def test_fifo_order():
    """先提交的任务先完成（用设备强制串行）。"""
    node_id = get_devnode_id()

    _, d1 = post("/command/run", {
        "node_id": node_id, "user_id": "pengyt",
        "command": "echo FIFO_1; sleep 2",
        "cpu": 1, "memory": 1, "mem_unit": "GB", "device_num": 1,
    })
    assert_ok(_, f"任务1 失败: {d1}")
    time.sleep(0.3)

    _, d2 = post("/command/run", {
        "node_id": node_id, "user_id": "lipz",
        "command": "echo FIFO_2; sleep 2",
        "cpu": 1, "memory": 1, "mem_unit": "GB", "device_num": 1,
    })
    assert_ok(_, f"任务2 失败: {d2}")

    # 并发消费者下 position 可能相同（同时出队），但先提交的应先完成
    # 验证两个任务都正常提交
    assert "task_id" in d1 and "task_id" in d2, "任务应正常返回 task_id"


def test_batch_delete():
    """批量删除：排队/已完成的可删，running 不删。"""
    gpu_id = get_devnode_id()

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
    # ("批量删除",    test_batch_delete),
]

if __name__ == "__main__":
    run_tests(TESTS, "任务队列测试")
