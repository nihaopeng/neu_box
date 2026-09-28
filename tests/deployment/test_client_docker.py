"""第 3 层 · client(neubox) 的容器路径（manifest 67-70、73-75、84-85）。

`test_client.py` 验的是 CLI 的基本路径（acquire / list / submit / cancel），
不需要 dockerd。这一组把 `neubox docker run` 起的容器**从生到死**走完整：注入
annotation → runtime hook 登记 → 容器里能不能用卡 → stop / start / release /
exec 各条路径。最典型的那条是：

    neubox acquire --device-num 2
    neubox docker run ... --name neu-test      # 登记到沙盒
    docker stop neu-test                       # 登记被注销，沙盒仍占着卡
    neubox release                             # 沙盒销毁；容器留着（停着）
    docker start neu-test                      # 重走 hook → 旧 annotation 404 → 起不来
    docker exec neu-test ...                   # 没 running，docker 直接拒

要点是：**释放之后拿不回卡，但不是靠删容器实现的**。删容器会连可写层一起丢，
用户可能还要 commit / cp 出来，所以 Worker 只停不删；隔离靠"进程退光 → mnt ns
死掉 → 驱动那张按 mnt ns 缓存的 UDA 表没人能用"+"重新 start 必然重走 hook 且
登记被拒"。原理见 `docs/isolation.md`。

84/85 走的是同一个现场的**另一条出口**：annotation 改不了，但 `neubox docker
start` 可以把这个 shell 现在的沙盒借给容器（借条，10 秒内有效、一次性），于是
跨 shell 换沙盒也能接着用同一个容器；不在沙盒里的 `neubox docker start`
会拒绝启动。契约见 `docs/container-registration.md`。

前置：同一 RPM 安装的 `neubox` 必须存在且版本达标，否则部署验收失败。
"""

from __future__ import annotations

import os
import re
import secrets
import shlex
import subprocess
import time

import pytest

from deployment_support import (
    CONTAINER_OPEN_OK,
    CONTAINER_PROBE_MARKER,
    container_probe_command,
    neubox_cli,
    neubox_sandbox_name,
    wait_container_log_count,
)


def _docker_run_args(name: str, node: str, image: str, command: str) -> list[str]:
    """`neubox docker run` 的参数：容器名 + 只挂一个设备节点 + 探测命令。"""
    return [
        "-d", "--name", name, "--device", node,
        "--entrypoint", "sh", image, "-c", command,
    ]


def _fields(output: str) -> dict[str, str]:
    """读取 CLI 的两列结果，空格对齐由 Go 输出单测单独验证。"""
    return dict(re.findall(r"(?m)^([a-z_]+):[ \t]+([^\r\n]+)$", output))


def _docker_run_in_sandbox(single_card, neubox_bin, device, args: list[str], *,
                           container_name: str) -> tuple[int, str, str]:
    """借一个 shell 进沙盒，在里面跑 `neubox docker run`；返回 (rc, 输出, 沙盒名)。

    `neubox docker run` 是按自己 PID 的 cgroup 反查沙盒的，所以必须在沙盒里跑；
    沙盒本身也用**真 neubox** 借（`neubox acquire --pid <shell>`），这一组不留
    任何绕过 CLI 的旁路。
    """
    shell, sandbox = _shell_with_card(single_card, neubox_bin, device)
    single_card.created_containers.append(container_name)
    # 每个参数都要 quote：`-c "sleep 600"` 里的引号属于 docker 的 argv，不能
    # 在拼 shell 命令时被吃掉（否则容器跑的是 `sh -c sleep`，立刻退出）。
    command = " ".join(
        shlex.quote(str(item))
        for item in [neubox_bin, "docker", "run", *args]
    )
    rc, output = shell.run(command)
    return rc, output, sandbox


def _shell_with_card(single_card, neubox_bin, device: int):
    """借一个 shell 进沙盒并占住一张卡；返回 ``(shell, 沙盒名)``。

    用真 neubox 借（`neubox acquire --pid <shell>`）：之后在这个 shell 里跑的
    `neubox docker run` / `neubox docker start` 都按自己 PID 的 cgroup 反查沙盒。
    """
    shell = single_card.sandbox_shell()
    sandbox = neubox_sandbox_name(
        neubox_bin, "acquire", "--pid", str(shell.pid), "--device", str(device),
    )
    # CLI 建的沙盒不走 acquire_sandbox，得自己登记进收尾清单（失败路径上要靠它
    # 把卡放回去）。
    single_card.track_sandbox(sandbox)
    return shell, sandbox


def _assert_open(single_card, reference: str, node: str, *, expect_ok: bool,
                 context: str) -> None:
    """在容器里 open 一个设备节点，只看退出码（镜像里不一定有 libc 消息目录）。"""
    probe = single_card.docker(
        "exec", reference, "sh", "-c",
        f"(exec 3<{node}) 2>/dev/null && echo OK || echo DENIED",
        timeout=60,
    )
    verdict = (probe.stdout or "").strip()
    if expect_ok:
        assert verdict == "OK", (
            f"{context}，但容器里打不开 {node}：verdict={verdict!r} "
            f"rc={probe.returncode} 输出={(probe.stdout or '')[:300]}"
        )
    else:
        assert "OK" not in verdict, (
            f"{context}，容器里却打开了 {node}：verdict={verdict!r} "
            f"输出={(probe.stdout or '')[:300]}"
        )


def _assert_sandbox_holds_device(single_card, sandbox: str, device: int) -> None:
    """只观察本用例借出的卡，避免其他卡的外部占用扰动全局空闲总数。"""
    record = single_card.find_sandbox(sandbox)
    assert record is not None, f"容器 stop 后沙盒 {sandbox} 不见了"
    held = {int(str(item).rsplit(":", 1)[-1]) for item in record["devices"]}
    assert device in held, f"沙盒 {sandbox} 不再持有卡 {device}：{record}"
    assert device not in single_card.idle_minors(), (
        f"沙盒 {sandbox} 仍持有卡 {device}，但 /status 把它报为空闲"
    )


def _wait_device_idle(single_card, device: int, timeout: float = 60.0) -> None:
    """等待本用例借出的卡回池；其他卡可能被外部程序临时占用。"""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if device in single_card.idle_minors():
            return
        time.sleep(single_card.poll)
    pytest.fail(
        f"沙盒释放后卡 {device} 仍未回池："
        f"dev_status={single_card.status().get('dev_status')}",
        pytrace=False,
    )


def _start_expecting_refused(single_card, reference: str, sandbox: str) -> None:
    """沙盒已释放的受管容器：原生 start 不能在无授权状态运行。"""
    started = single_card.docker("start", reference, timeout=90)
    assert started.returncode != 0, (
        f"沙盒 {sandbox} 已释放，原生 `docker start` 不得放行受管容器"
    )
    single_card.wait_container_stopped(reference)
    container_id = single_card.container_id_of(reference)
    assert single_card.sandbox_of_container(container_id).json().get(
        "sandbox_name") is None, "启动失败的容器不该有登记"


def _stage_stopped_released_container(single_card, neubox_bin, container_image,
                                      device: int, node: str):
    """搭出"容器停着、沙盒已 release"的现场；返回 ``(沙盒名, 容器名, 容器 id)``。

    这是那条经典路径的中段，73/74/75 三条用例共用，每一步都断言 —— 否则后面
    的失败信息会指到错误的环节上。
    """
    name = f"neu-box-client-{secrets.token_hex(4)}"
    rc, output, sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, node, container_image,
                         container_probe_command(node)),
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"

    container_id = single_card.container_id_of(name)
    assert single_card.wait_container_registered(container_id) == sandbox, (
        f"容器没有登记到它所在的沙盒 {sandbox}"
    )
    text = wait_container_log_count(
        single_card, name, CONTAINER_PROBE_MARKER, 1,
    )
    assert CONTAINER_OPEN_OK in text, (
        f"容器里打不开自己沙盒的卡，后面的路径没有意义：\n{text[:1000]}"
    )

    # ① stop：登记被注销（pidfd），但沙盒自己还占着这张卡。
    stopped = single_card.docker("stop", "-t", "2", name, timeout=90)
    assert stopped.returncode == 0, (stopped.stdout or "")[:500]
    single_card.wait_container_unregistered(container_id)
    _assert_sandbox_holds_device(single_card, sandbox, device)

    # ② release：沙盒销毁、卡回池；容器**留着**（停着）。
    neubox_cli(neubox_bin, "release", sandbox)
    single_card.wait_sandbox_gone(sandbox)
    _wait_device_idle(single_card, device)
    single_card.wait_container_stopped(name)
    return sandbox, name, container_id


def test_client_docker_run_registers_container_in_own_sandbox(
        neubox_bin, single_card, container_image):
    """67 · 真 neubox：`docker run` 注入 annotation → 容器登记到自己那个沙盒。

    这是 client 侧最关键的一条：annotation 拼错、argv 位置错（历史上少传
    argv[0] 就会整条命令失败）、或者按 PID 反查到别的沙盒，都会在这里露出来。
    """
    device = single_card.require_idle(1)[0]
    name = f"neu-box-client-{secrets.token_hex(4)}"
    node = single_card.device_node(device)

    rc, output, sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, node, container_image, "sleep 600"),
        container_name=name,
    )
    assert rc == 0, f"沙盒 {sandbox} 里的 `neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"

    container_id = single_card.container_id_of(name)
    registered = single_card.wait_container_registered(container_id)
    assert registered == sandbox, (
        f"容器登记到了 {registered}，期望它所在的沙盒 {sandbox}"
    )

    # annotation 的值必须就是沙盒名（不能是 cgroup 路径/basename）。
    annotations = single_card.docker(
        "inspect", "-f", '{{index .HostConfig.Annotations "sandbox_cgroup"}}',
        name, timeout=60,
    )
    assert annotations.returncode == 0, annotations.stderr[:500]
    assert annotations.stdout.strip() == sandbox, (
        f"容器上的 annotation 是 {annotations.stdout.strip()!r}，期望沙盒名 {sandbox!r}"
    )

    single_card.remove_container(name)


def test_client_docker_run_refuses_without_sandbox(neubox_bin, container_image):
    """68 · 不在沙盒里的 `neubox docker run`：拒绝启动，不降级成"没 annotation 的裸跑"。"""
    result = subprocess.run(
        [neubox_bin, "docker", "run", "--rm", "--entrypoint", "true",
         container_image],
        capture_output=True, text=True, timeout=120,
    )
    output = f"{result.stdout}\n{result.stderr}"
    assert result.returncode != 0, (
        f"不在沙盒里竟然也起来了（rc=0）：\n{output[:1000]}"
    )
    assert "acquire" in output or "沙盒" in output, (
        f"报错没有说清要先 acquire：\n{output[:1000]}"
    )


def test_client_docker_run_container_sees_only_reserved_card(
        neubox_bin, single_card, container_image):
    """69 · client 起的容器里：自己沙盒的卡能开，别人的/空闲的卡开不了。"""
    device = single_card.require_idle(1)[0]
    other = [minor for minor in single_card.idle_minors() if minor != device]
    name = f"neu-box-client-{secrets.token_hex(4)}"

    rc, output, sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, single_card.device_node(device), container_image,
                         "sleep 600"),
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"

    single_card.wait_container_registered(single_card.container_id_of(name))
    _assert_open(single_card, name, single_card.device_node(device),
                 expect_ok=True,
                 context=f"沙盒 {sandbox} 持有卡 {device}")
    if other:
        _assert_open(single_card, name, single_card.device_node(other[0]),
                     expect_ok=False,
                     context=f"沙盒 {sandbox} 只持有 {device}，卡 {other[0]} 是空闲的")

    single_card.remove_container(name)


def test_client_release_stops_client_started_container(
        neubox_bin, single_card, container_image):
    """70 · `neubox release` 把 client 起的容器一并停掉（授权撤销、容器保留）。"""
    device = single_card.require_idle(1)[0]
    name = f"neu-box-client-{secrets.token_hex(4)}"

    rc, output, sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, single_card.device_node(device), container_image,
                         "sleep 600"),
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"
    container_id = single_card.container_id_of(name)
    assert single_card.wait_container_registered(container_id) == sandbox

    neubox_cli(neubox_bin, "release", sandbox)
    single_card.wait_sandbox_gone(sandbox)
    single_card.wait_container_stopped(name)
    _wait_device_idle(single_card, device)


def test_client_container_stop_then_start_uses_card_again(
        neubox_bin, single_card, container_image):
    """73 · 沙盒还在时 `docker stop` → `docker start`：容器能回来、重新登记、还能用卡。

    这是"支持 stop / start"的正例：stop 让 Worker 注销登记（pidfd 事件），但沙盒
    自己还占着卡；start 重走整条 create → runtime hook → 重新登记到同一个沙盒
    （新的 mnt ns），容器里的 NPU 照旧能用。
    """
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)
    name = f"neu-box-client-{secrets.token_hex(4)}"

    rc, output, sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, node, container_image,
                         container_probe_command(node)),
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"
    container_id = single_card.container_id_of(name)
    assert single_card.wait_container_registered(container_id) == sandbox
    first = wait_container_log_count(single_card, name, CONTAINER_PROBE_MARKER, 1)
    assert CONTAINER_OPEN_OK in first, first[:1000]

    stopped = single_card.docker("stop", "-t", "2", name, timeout=90)
    assert stopped.returncode == 0, (stopped.stdout or "")[:500]
    single_card.wait_container_unregistered(container_id)
    _assert_sandbox_holds_device(single_card, sandbox, device)

    started = single_card.docker("start", name, timeout=90)
    assert started.returncode == 0, (
        f"沙盒 {sandbox} 还活着，`docker start` 却失败了："
        f"{(started.stdout or '')[:500]}"
    )
    assert single_card.wait_container_registered(container_id) == sandbox, (
        f"容器重启后没有重新登记到沙盒 {sandbox}"
    )
    text = wait_container_log_count(single_card, name, CONTAINER_PROBE_MARKER, 2)
    assert text.count(CONTAINER_OPEN_OK) >= 2, (
        f"重启后容器里打不开自己沙盒的卡（登记在、授权没恢复）：\n{text[:1500]}"
    )

    neubox_cli(neubox_bin, "release", sandbox)
    single_card.wait_sandbox_gone(sandbox)
    _wait_device_idle(single_card, device)


def test_client_release_then_native_start_is_rejected(
        neubox_bin, single_card, container_image):
    """74 · stop 之后再 release：容器留着，原生 start 被拒绝。

    release 时 `containers` 表里已经看不到它了（stop 时就注销），而 `neubox
    docker run` 起的容器没有 label，所以它会活过 release；再 start 时 hook 拿到
    "没有这个沙盒" → 拒绝启动。
    """
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)

    sandbox, name, _container_id = _stage_stopped_released_container(
        single_card, neubox_bin, container_image, device, node)

    # 容器还在（只是停着）—— 这是"不删容器"的直接回归：可写层没丢。
    _start_expecting_refused(single_card, name, sandbox)
    # 卡已经回池，失败的启动不能把它重新占回去。
    _wait_device_idle(single_card, device)

    single_card.remove_container(name)


def test_client_released_card_goes_to_next_sandbox(
        neubox_bin, single_card, container_image):
    """75 · 停着的老容器不会挡住同一张卡交给下一个沙盒。

    接 74 的现场：同一张卡用真 neubox 重新 acquire，新容器能开这张卡；老容器
    start 被拒绝，不会抢走卡。
    """
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)

    sandbox, old_name, _old_id = _stage_stopped_released_container(
        single_card, neubox_bin, container_image, device, node)
    single_card.wait_container_stopped(old_name)
    # 原生启动旧容器必须被拒绝，不能把卡占回去。
    _start_expecting_refused(single_card, old_name, sandbox)

    new_name = f"neu-box-client-{secrets.token_hex(4)}"
    rc, output, new_sandbox = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(new_name, node, container_image, "sleep 600"),
        container_name=new_name,
    )
    assert rc == 0, f"同一张卡重新 acquire 后 `neubox docker run` 失败：\n{output[:2000]}"
    assert new_sandbox != sandbox, (
        f"新沙盒名和释放掉的那个一样（{new_sandbox}）—— 沙盒名不该复用"
    )
    new_id = single_card.container_id_of(new_name)
    assert single_card.wait_container_registered(new_id) == new_sandbox
    _assert_open(single_card, new_name, node, expect_ok=True,
                 context=f"卡 {device} 已经交给新沙盒 {new_sandbox}")

    # 老容器仍然停止，没有从新沙盒借到卡。
    single_card.wait_container_stopped(old_name)

    neubox_cli(neubox_bin, "release", new_sandbox)
    single_card.wait_sandbox_gone(new_sandbox)
    _wait_device_idle(single_card, device)
    single_card.remove_container(new_name)
    single_card.remove_container(old_name)


def test_client_docker_start_lends_the_current_sandbox(
        neubox_bin, single_card, container_image):
    """84 · 跨 shell：`neubox docker start` 把当前沙盒借给老容器，容器照旧能用卡。

    这是"annotation 改不了"的正解。现场和 74/75 一样（容器停着、沙盒已 release、
    annotation 还指着那个死沙盒），区别只在最后一步：在**新沙盒的 shell 里**敲
    `neubox docker start`，借条被 hook 认领，容器绑到**现在的**沙盒上 —— 容器还是
    原来那个（ID 不变、可写层还在、annotation 一个字没改）。
    """
    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)
    name = f"neu-box-client-{secrets.token_hex(4)}"

    # ① 沙盒 A 里用真 neubox 起容器，确认它真能用卡。
    rc, output, sandbox_a = _docker_run_in_sandbox(
        single_card, neubox_bin, device,
        _docker_run_args(name, node, container_image, "sleep 600"),
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"
    container_id = single_card.container_id_of(name)
    assert single_card.wait_container_registered(container_id) == sandbox_a
    _assert_open(single_card, name, node, expect_ok=True,
                 context=f"沙盒 {sandbox_a} 持有卡 {device}")

    # ② stop + release：容器留着（停着），卡回池，annotation 还指着死掉的沙盒 A。
    stopped = single_card.docker("stop", "-t", "2", name, timeout=90)
    assert stopped.returncode == 0, (stopped.stdout or "")[:500]
    single_card.wait_container_unregistered(container_id)
    neubox_cli(neubox_bin, "release", sandbox_a)
    single_card.wait_sandbox_gone(sandbox_a)
    _wait_device_idle(single_card, device)
    single_card.wait_container_stopped(name)

    annotation = single_card.docker(
        "inspect", "-f", '{{index .HostConfig.Annotations "sandbox_cgroup"}}',
        name, timeout=60,
    )
    assert annotation.returncode == 0, annotation.stderr[:500]
    assert annotation.stdout.strip() == sandbox_a, (
        f"容器上的 annotation 不该被改写，实际 {annotation.stdout.strip()!r}"
    )

    # ③ 另一个 shell 借走同一张卡 —— 这就是"跨 shell 换沙盒"。
    shell_b, sandbox_b = _shell_with_card(single_card, neubox_bin, device)
    assert sandbox_b != sandbox_a, "新沙盒名不该复用旧名字"
    rc, output = shell_b.run(" ".join(
        shlex.quote(str(item))
        for item in [neubox_bin, "docker", "start", name]
    ))
    assert rc == 0, (
        f"沙盒 {sandbox_b} 里的 `neubox docker start` 失败（rc={rc}）：\n{output[:2000]}"
    )

    assert single_card.container_id_of(name) == container_id, (
        "借条改绑不该换容器：还是同一个容器，可写层不能丢"
    )
    assert single_card.wait_container_registered(container_id) == sandbox_b, (
        f"容器没有绑到借条里的沙盒 {sandbox_b}（借条没被认领？）"
    )
    _assert_open(single_card, name, node, expect_ok=True,
                 context=f"借条生效后容器该能用沙盒 {sandbox_b} 的卡 {device}")

    # ④ 收尾：release 新沙盒 → 按它登记的容器被一并停掉。
    neubox_cli(neubox_bin, "release", sandbox_b)
    single_card.wait_sandbox_gone(sandbox_b)
    single_card.wait_container_stopped(name)
    _wait_device_idle(single_card, device)
    single_card.remove_container(name)


def test_client_docker_start_without_a_sandbox_is_rejected(
        neubox_bin, single_card, container_image):
    """85 · 不在沙盒里借不到卡时，neubox docker start 不启动容器。"""
    caller = single_card.client.sandbox_status(pid=os.getpid()).json()
    if caller.get("sandbox_name"):
        pytest.fail(
            "前置缺失：跑验收的进程自己就在沙盒 "
            f"{caller['sandbox_name']} 里，测不了'不在沙盒里'的分支；"
            "请从不在沙盒里的 shell 执行 sudo /usr/libexec/neu-box/neuboxctl/neuboxctl test",
            pytrace=False,
        )

    device = single_card.require_idle(1)[0]
    node = single_card.device_node(device)

    sandbox, name, container_id = _stage_stopped_released_container(
        single_card, neubox_bin, container_image, device, node)
    single_card.wait_container_stopped(name)

    result = subprocess.run(
        [neubox_bin, "docker", "start", name],
        capture_output=True, text=True, timeout=120,
    )
    output = f"{result.stdout}\n{result.stderr}"
    assert result.returncode != 0, (
        f"借不到沙盒时必须拒绝 start（沙盒 {sandbox} 已经 release）："
        f"rc={result.returncode}\n{output[:1000]}"
    )
    running = single_card.docker(
        "inspect", "--format", "{{.State.Running}}", name, timeout=30)
    assert running.stdout.strip() == "false", (
        f"容器应保持停止，实际 State.Running={running.stdout.strip()!r}"
    )
    assert single_card.sandbox_of_container(container_id).json().get(
        "sandbox_name") is None, "没有借条就不该有登记"
    assert "沙盒" in output, (
        f"命令行应说明借卡失败：\n{output[:1000]}"
    )

    _wait_device_idle(single_card, device)
    single_card.remove_container(name)


def test_client_docker_status_and_restart_rebind_running_container(
        neubox_bin, multi_card, container_image):
    """真 Docker + Worker：运行中 restart 换绑沙盒，设备权限随之变化。"""
    device_a, device_b = multi_card.require_idle(2)
    node_a = multi_card.device_node(device_a)
    node_b = multi_card.device_node(device_b)
    name = f"neu-box-client-{secrets.token_hex(4)}"

    # 两张设备节点都预先映射进容器，后面的拒绝必须由沙盒授权决定。
    rc, output, sandbox_a = _docker_run_in_sandbox(
        multi_card, neubox_bin, device_a,
        ["-d", "--name", name, "--device", node_a, "--device", node_b,
         "--entrypoint", "sh", container_image, "-c", "sleep 600"],
        container_name=name,
    )
    assert rc == 0, f"`neubox docker run` 失败（rc={rc}）：\n{output[:2000]}"
    container_id = multi_card.container_id_of(name)
    assert multi_card.wait_container_registered(container_id) == sandbox_a
    status_a = neubox_cli(neubox_bin, "docker", "status", name)
    fields_a = _fields(status_a)
    assert fields_a.get("sandbox") == sandbox_a and fields_a.get("managed") == "yes", (
        f"运行中容器的 status 没报告原沙盒：\n{status_a}"
    )
    _assert_open(multi_card, name, node_a, expect_ok=True,
                 context=f"沙盒 {sandbox_a} 应能打开卡 {device_a}")
    _assert_open(multi_card, name, node_b, expect_ok=False,
                 context=f"沙盒 {sandbox_a} 不应打开卡 {device_b}")

    # exec 只复用既有授权；neubox 的同名子命令必须明确拒绝，不能悄悄透传。
    rejected = subprocess.run(
        [neubox_bin, "docker", "exec", name, "sh"],
        capture_output=True, text=True, timeout=30,
    )
    assert rejected.returncode != 0 and "docker exec" in rejected.stderr, (
        f"neubox docker exec 没有拒绝：rc={rejected.returncode} "
        f"stderr={rejected.stderr[:500]}"
    )
    assert multi_card.sandbox_of_container(container_id).json().get(
        "sandbox_name") == sandbox_a, "拒绝 exec 后容器授权发生了变化"

    # A 尚在运行，B 借另一张卡；restart 必须对运行中容器执行 stop → 借条 → start。
    shell_b, sandbox_b = _shell_with_card(multi_card, neubox_bin, device_b)
    assert sandbox_b != sandbox_a
    rc, output = shell_b.run(" ".join(
        shlex.quote(str(item))
        for item in [neubox_bin, "docker", "restart", name]
    ))
    assert rc == 0, (
        f"沙盒 {sandbox_b} 里的 `neubox docker restart` 失败（rc={rc}）：\n{output[:2000]}"
    )
    assert multi_card.container_id_of(name) == container_id, "restart 不应换容器 ID"
    assert multi_card.wait_container_registered(container_id) == sandbox_b
    rebound_status = neubox_cli(neubox_bin, "docker", "status", name)
    rebound_fields = _fields(rebound_status)
    assert rebound_fields.get("container_state") == "running" and rebound_fields.get("sandbox") == sandbox_b, (
        f"restart 后 status 没报告新沙盒：\n{rebound_status}"
    )
    _assert_open(multi_card, name, node_a, expect_ok=False,
                 context=f"restart 换到沙盒 {sandbox_b} 后旧卡 {device_a} 应被拒绝")
    _assert_open(multi_card, name, node_b, expect_ok=True,
                 context=f"restart 换到沙盒 {sandbox_b} 后新卡 {device_b} 应可用")

    # 旧沙盒释放不能误停已经换绑到 B 的容器。
    neubox_cli(neubox_bin, "release", sandbox_a)
    multi_card.wait_sandbox_gone(sandbox_a)
    multi_card.created_sandboxes.remove(sandbox_a)
    _wait_device_idle(multi_card, device_a)
    _assert_open(multi_card, name, node_b, expect_ok=True,
                 context="释放旧沙盒后新沙盒的授权应保留")

    stopped = multi_card.docker("stop", "-t", "2", name, timeout=90)
    assert stopped.returncode == 0, (stopped.stdout or "")[:500]
    multi_card.wait_container_unregistered(container_id)
    stopped_status = neubox_cli(neubox_bin, "docker", "status", name)
    stopped_fields = _fields(stopped_status)
    assert stopped_fields.get("container_state") == "exited" and stopped_fields.get("sandbox") == "none", (
        f"已停止容器不该报告当前授权：\n{stopped_status}"
    )
    _assert_sandbox_holds_device(multi_card, sandbox_b, device_b)

    neubox_cli(neubox_bin, "release", sandbox_b)
    multi_card.wait_sandbox_gone(sandbox_b)
    multi_card.created_sandboxes.remove(sandbox_b)
    _wait_device_idle(multi_card, device_b)
