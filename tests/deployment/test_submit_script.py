"""第 3 层 · 真 neubox submit --script 的宿主与容器工作流。

单元测试验证请求格式；这里验证 CLI 读入脚本、Worker 排队执行脚本、返回业务
退出码、收尾释放设备的整条链。不会在开发机普通 ``pytest tests/`` 中收集。

需要与 Worker 同版的 neubox；容器用例另外需要已配置的 Docker 默认 runtime
和本机已有的带 /bin/sh 镜像。前置由 neubox_bin、single_card、container_image
三个 fixture 检查。
"""

from __future__ import annotations

import os
import re
import secrets
import shlex
import subprocess
import time
from pathlib import Path


def _submit_script(neubox_bin, deployment, *options: str,
                   script_input: str | None = None) -> str:
    """经真实 CLI 提交，并立即把任务登记进用例收尾清单。"""
    environment = dict(
        os.environ, NEU_BOX_URL=deployment.url, NEU_BOX_USER=deployment.user,
    )
    result = subprocess.run(
        [neubox_bin, "submit", *options], input=script_input,
        capture_output=True, text=True, timeout=30, env=environment,
    )
    output = f"{result.stdout}\n{result.stderr}"
    assert result.returncode == 0, (
        f"neubox submit --script 失败（rc={result.returncode}）：\n{output[:2000]}"
    )
    match = re.search(r"(?m)^task:\s*([0-9a-f]{12})\s*$", output)
    assert match, f"提交响应缺少 task_id：\n{output[:2000]}"
    task_id = match.group(1)
    deployment.created_tasks.append(task_id)
    return task_id


def _wait_device_idle(deployment, device: int, timeout: float = 60.0) -> None:
    """只等本用例借的卡回池，避免其他卡的占用影响断言。"""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if device in deployment.idle_minors():
            return
        time.sleep(deployment.poll)
    assert device in deployment.idle_minors(), f"指定设备 {device} 未回到空闲池"


def test_submit_file_snapshots_multiline_heredoc_before_queue_runs(
        neubox_bin, single_card):
    """排队后改本地文件，不改变已提交脚本；Bash 按原换行解释 heredoc。"""
    device = single_card.require_idle(1)[0]
    blocker = single_card.submit("sleep 120", device_ids=[device])
    single_card.wait_task_running(blocker)

    original = f"script-original-{secrets.token_hex(4)}"
    changed = f"script-changed-{secrets.token_hex(4)}"
    source = Path(single_card.tempdir) / f"submit-{secrets.token_hex(4)}.sh"
    source.write_text(
        "#!/bin/bash\nset -e\nwords=(alpha beta)\ncat <<'EOF'\n"
        + original + "\n  $HOME literal\nEOF\n"
        + "printf 'bash=%s\\n' \"${words[1]}\"\n",
        encoding="utf-8",
    )
    task_id = _submit_script(
        neubox_bin, single_card, "--device", str(device),
        "--workdir", single_card.tempdir,
        "--script", str(source),
    )
    try:
        queued = single_card.queue_entry(task_id)
        assert queued is not None and queued["status"] == "queued", queued
        source.write_text(f"echo {changed}\n", encoding="utf-8")
    finally:
        single_card.client.delete_tasks([blocker])
        single_card.wait_task(blocker)

    task = single_card.wait_task(task_id)
    log = single_card.task_log_text(task_id)
    assert task["status"] == "completed" and task["result"]["returncode"] == 0, (
        f"任务未成功：{task}\n{log[:2000]}"
    )
    assert original in log and "  $HOME literal" in log and "bash=beta" in log, log
    assert changed not in log, f"任务用了排队后修改的脚本：\n{log}"
    _wait_device_idle(single_card, device)


def test_submit_stdin_script_reports_main_exit_code(neubox_bin, single_card):
    """脚本 stdin 是提交内容；入口 exit 23 决定任务结果，后续行不执行。"""
    before = f"before-exit-{secrets.token_hex(4)}"
    after = f"after-exit-{secrets.token_hex(4)}"
    script = f"#!/bin/bash\nprintf '%s\\n' {shlex.quote(before)}\nexit 23\necho {after}\n"
    task_id = _submit_script(
        neubox_bin, single_card, "--device-num", "0",
        "--workdir", single_card.tempdir, "--script", "-",
        script_input=script,
    )
    task = single_card.wait_task(task_id)
    log = single_card.task_log_text(task_id)
    assert task["status"] == "failed" and task["result"]["returncode"] == 23, (
        f"业务退出码丢失：{task}\n{log[:2000]}"
    )
    assert before in log and after not in log, log


def test_submit_script_docker_run_rm_binds_card_and_cleans_up(
        neubox_bin, single_card, container_image):
    """脚本里的包装 run 使用本任务沙盒；前台退出后 --rm 与设备清理均完成。"""
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)
    name = f"neu-box-submit-{secrets.token_hex(4)}"
    marker = f"script-docker-ok-{secrets.token_hex(4)}"
    # 只登记本用例独有的容器名；失败时 case_cleanup 可以安全回收它。
    single_card.created_containers.append(name)
    probe = f"(exec 3<{shlex.quote(node)}) 2>/dev/null && echo {marker}"
    docker_command = shlex.join([
        "neubox", "docker", "run", "--rm", "--name", name,
        "--device", node, "--entrypoint", "sh", container_image,
        "-c", probe,
    ])
    script = "#!/bin/bash\nset -e\n" + docker_command + "\n"
    task_id = _submit_script(
        neubox_bin, single_card, "--device", str(device),
        "--workdir", single_card.tempdir, "--script", "-",
        script_input=script,
    )
    task = single_card.wait_task(task_id, timeout=max(single_card.task_timeout, 90.0))
    log = single_card.task_log_text(task_id)
    assert task["status"] == "completed" and task["result"]["returncode"] == 0, (
        f"容器脚本未成功：{task}\n{log[:2000]}"
    )
    assert marker in log, f"容器没有读到被授权的设备节点 {node}：\n{log[:2000]}"
    single_card.wait_container_removed(name)
    _wait_device_idle(single_card, device)


def test_cancel_running_script_stops_rm_container_and_returns_its_card(
        neubox_bin, single_card, container_image):
    """取消前台容器脚本：任务进入 cancelled，容器和本任务指定的卡都回收。"""
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)
    name = f"neu-box-submit-cancel-{secrets.token_hex(4)}"
    marker = f"container-ready-{secrets.token_hex(4)}"
    single_card.created_containers.append(name)
    payload = f"(exec 3<{shlex.quote(node)}) 2>/dev/null && echo {marker} && sleep 300"
    command = shlex.join([
        "neubox", "docker", "run", "--rm", "--name", name,
        "--device", node, "--entrypoint", "sh", container_image,
        "-c", payload,
    ])
    task_id = _submit_script(
        neubox_bin, single_card, "--device", str(device),
        "--workdir", single_card.tempdir, "--script", "-",
        script_input="#!/bin/bash\nset -e\n" + command + "\n",
    )
    cancel_requested = False
    try:
        single_card.wait_task_running(task_id, timeout=60)
        single_card.wait_log_contains(task_id, marker, timeout=60)
        container_id = single_card.container_id_of(name)
        sandbox = single_card.wait_container_registered(container_id, timeout=30)
        assert sandbox == f"sbx_{single_card.user}_{task_id}.slice", (
            f"容器登记到了 {sandbox}，预期任务 {task_id} 的沙盒"
        )

        environment = dict(
            os.environ, NEU_BOX_URL=single_card.url, NEU_BOX_USER=single_card.user,
        )
        cancelled = subprocess.run(
            [neubox_bin, "cancel", task_id], capture_output=True, text=True,
            timeout=30, env=environment,
        )
        assert cancelled.returncode == 0, (
            f"取消任务失败（rc={cancelled.returncode}）：\n"
            f"{cancelled.stdout}\n{cancelled.stderr}"
        )
        cancel_requested = True
        task = single_card.wait_task(task_id, timeout=max(single_card.task_timeout, 90.0))
        assert task["status"] == "cancelled", (
            f"取消后任务状态错误：{task}\n{single_card.task_log_text(task_id)[:2000]}"
        )
        single_card.wait_container_removed(name, timeout=90)
        _wait_device_idle(single_card, device)
    finally:
        if not cancel_requested:
            # 断言失败时先请求 Worker 正常取消；case_cleanup 还会兜底删除
            # 本用例唯一命名的容器并清理登记的 task_id。
            try:
                single_card.client.delete_tasks([task_id])
            except BaseException:
                pass  # 不用清理异常覆盖原始断言；case_cleanup 会再试


def test_submit_script_starts_stopped_container_and_preserves_it(
        neubox_bin, single_card, container_image):
    """已有停止容器可在任务里重新借卡、exec，结束后保留原容器。"""
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)
    tag = secrets.token_hex(4)
    name = f"neu-box-submit-staged-{tag}"
    marker = f"staged-exec-ok-{tag}"
    gate_dir = Path(single_card.tempdir) / f"staged-gate-{tag}"
    gate_dir.mkdir()
    release_gate = gate_dir / "continue"
    single_card.created_containers.append(name)

    # start 只接管已有 sandbox_cgroup annotation 的受管容器。用真实临时沙盒
    # 给原生 docker create 配置 annotation，释放它以后再让任务重新借卡。
    preparer = single_card.spawn_sleeper(180)
    with single_card.sandbox(
        single_card.acquire_payload(preparer.pid, device_ids=[device]),
    ) as prepared:
        old_sandbox = prepared["sandbox_name"]
        created = single_card.docker(
            "create", "--name", name,
            "--annotation", f"sandbox_cgroup={old_sandbox}",
            "--device", node,
            "-v", f"{gate_dir}:/neu-box-gate:ro",
            "--entrypoint", "sh", container_image, "-c", "sleep 300",
        )
        assert created.returncode == 0, (
            f"原生 docker create 失败：\n{(created.stdout or '')[:2000]}"
        )
        container_id = single_card.container_id_of(name)
        single_card.wait_container_stopped(name)
    single_card.wait_sandbox_gone(old_sandbox)
    _wait_device_idle(single_card, device)

    probe = (
        f"(exec 3<{shlex.quote(node)}) 2>/dev/null || exit 41; "
        f"echo {marker}; "
        "i=0; while [ \"$i\" -lt 60 ]; do "
        "[ -e /neu-box-gate/continue ] && exit 0; "
        "i=$((i+1)); sleep 1; done; exit 42"
    )
    script = "#!/bin/bash\nset -e\n" + "\n".join([
        shlex.join(["neubox", "docker", "start", name]),
        shlex.join(["docker", "exec", name, "sh", "-c", probe]),
        shlex.join(["docker", "stop", name]),
    ]) + "\n"
    task_id = _submit_script(
        neubox_bin, single_card, "--device", str(device),
        "--workdir", single_card.tempdir, "--script", "-",
        script_input=script,
    )
    try:
        single_card.wait_task_running(task_id, timeout=60)
        single_card.wait_log_contains(task_id, marker, timeout=60)
        bound = single_card.wait_container_registered(container_id, timeout=30)
        expected = f"sbx_{single_card.user}_{task_id}.slice"
        assert bound == expected, (
            f"容器 {container_id} 绑定到 {bound}，预期任务沙盒 {expected}"
        )
    finally:
        # 任一断言失败也要放行 exec；case_cleanup 负责兜底取消任务和删容器。
        release_gate.touch()

    task = single_card.wait_task(task_id, timeout=max(single_card.task_timeout, 90.0))
    log = single_card.task_log_text(task_id)
    assert task["status"] == "completed" and task["result"]["returncode"] == 0, (
        f"停止容器脚本未成功：{task}\n{log[:2000]}"
    )
    assert marker in log, f"docker exec 没能读取设备节点 {node}：\n{log[:2000]}"
    single_card.wait_container_stopped(name)
    single_card.wait_container_unregistered(container_id)
    assert single_card.container_id_of(name) == container_id, (
        "任务结束后已有容器应保留原 ID"
    )
    _wait_device_idle(single_card, device)
