package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/neusbox/neu_box/client/neubox/internal/api"
	"github.com/neusbox/neu_box/client/neubox/internal/dockerargs"
)

const (
	dockerRunUsage     = "neubox docker run <docker run 参数...>"
	dockerStartUsage   = "neubox docker start <容器> [docker start 的其他参数...]"
	dockerRestartUsage = "neubox docker restart <容器>"
	dockerStatusUsage  = "neubox docker status <容器>"

	// `docker start` 之后回查借条认领结果的节奏。hook 在 create 阶段就登记完
	// 了，正常第一次查就有结果；这两个数只是容忍抖动。
	dockerStartConfirmTimeout = 3 * time.Second
	dockerStartConfirmPoll    = 100 * time.Millisecond
)

// runDocker 分发 `neubox docker run` / `neubox docker start`。
//
// 客户端不碰 BPF、不碰 cgroup、不做授权判断 —— 它只负责把"这次启动该用哪个
// 沙盒"讲清楚：run 拼一行 annotation，start 存一张借条（annotation 改不了）。
// 讲错了 Worker 会在登记时拒掉，容器在设备侧被 fail-closed 拦下。
func (a *app) runDocker(args []string) int {
	if len(args) == 0 {
		return a.usageError("用法: " + dockerRunUsage)
	}
	switch args[0] {
	case "run":
	case "start":
		return a.runDockerStart(args[1:])
	case "restart":
		return a.runDockerRestart(args[1:])
	case "status":
		return a.runDockerStatus(args[1:])
	case "exec":
		return a.usageError("neubox docker exec 不借卡，也不改变运行中容器的设备；请用原生 docker exec。先用 neubox docker status <容器> 查看授权；详见 neubox docker help")
	case "help", "-h", "--help":
		a.printDockerHelp()
		return 0
	default:
		return a.usageError(fmt.Sprintf(
			"不支持 neubox docker %s；详见 neubox docker help", args[0]))
	}

	// `docker run` 之后的参数一个都不解析，原样透传（连同开头的 --）。
	sandboxName, code := a.resolveOwnSandbox()
	if code != 0 {
		return code
	}

	dockerBinary, err := a.lookPath("docker")
	if err != nil {
		a.printError("docker_not_found",
			"PATH 里找不到 docker 命令；请在装了 docker 的主机上运行 neubox docker run")
		return 1
	}

	argv := dockerargs.BuildDockerArgs(sandboxName, args[1:])
	// execve 的 argv[0] 必须是可执行文件名本身，syscall.Exec 不会替你补。
	// 少了它，docker 进程看到的 os.Args 就是 ["run", "--annotation", ...]，
	// 于是 --annotation 落到顶层 flag 的位置，报 "unknown flag: --annotation"。
	full := append([]string{dockerBinary}, argv...)
	// 成功即进程已被替换，不会再走到这里。
	if err := a.execFn(dockerBinary, full, os.Environ()); err != nil {
		a.printError("docker_exec_failed", fmt.Sprintf("启动 docker 失败: %v", err))
		return 1
	}
	return 0
}

// runDockerStart 是 `neubox docker start`：先把这个 shell 的沙盒借给容器，
// 再真去 start。
//
// 为什么需要这一层：容器上那行 annotation 是**建容器时**写死的，而
// `docker start` 既不接受 `--annotation`、也看不到调用方是谁（它是 dockerd
// 干的活）。沙盒一旦 release，旧 annotation 不再指向可用授权。这里把
// "这次 start 该用哪个沙盒"显式告诉 Worker（借条，10 秒内有效、一次性），
// hook 登记成功后确认认领；无法确认时尝试停止容器并返回失败。
func (a *app) runDockerStart(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return a.usageError("用法: " + dockerStartUsage +
			"（容器名放最前面，docker 自己的选项跟在它后面）")
	}
	attached, err := dockerStartAttachOption(args[1:])
	if err != nil {
		return a.usageError(err.Error())
	}
	if a.insideContainer() {
		return a.usageError("容器内不能按 PID 反查沙盒（PID namespace 与宿主机" +
			"不同）；请在宿主 shell 里执行 neubox docker start")
	}
	container := args[0]

	dockerBinary, err := a.lookPath("docker")
	if err != nil {
		a.printError("docker_not_found",
			"PATH 里找不到 docker 命令；请在装了 docker 的主机上运行 neubox docker start")
		return 1
	}
	info, err := a.inspectDockerContainer(dockerBinary, container)
	if err != nil {
		return a.internalError("docker_inspect_failed", err)
	}
	if info.State.Running {
		return a.usageError("容器已经运行；exec 只能沿用已有授权。需要重新借卡请显式使用 neubox docker restart " + container)
	}
	if strings.TrimSpace(info.HostConfig.Annotations["sandbox_cgroup"]) == "" {
		return a.usageError("容器创建时没有 sandbox_cgroup annotation，无法借卡；请用 neubox docker run 创建受管容器")
	}
	sandboxName, err := a.lendSandboxTo(info.ID)
	if err != nil {
		return a.internalError("sandbox_not_lent", err)
	}

	// 借条已经落账。-a 会等待容器退出，必须在等待期间检查绑定。
	argv := append([]string{dockerBinary, "start"}, args...)
	if attached {
		return a.runAttachedDockerStart(dockerBinary, argv, container, info.ID, sandboxName)
	}
	status, err := a.runFn(dockerBinary, argv, os.Environ())
	if err != nil {
		a.printError("docker_exec_failed", fmt.Sprintf("启动 docker 失败: %v", err))
		return 1
	}
	if status != 0 {
		return status
	}
	return a.confirmStartBinding(dockerBinary, container, info.ID, sandboxName)
}

// Docker's -a keeps the client in the foreground until the container exits.
// Watch the short-lived start intent while that client is still waiting, then
// return the Docker exit code once the workload finishes.
func (a *app) runAttachedDockerStart(binary string, argv []string, container, containerID, sandboxName string) int {
	type runResult struct {
		status int
		err    error
	}
	started, err := a.startFn(binary, argv, os.Environ())
	if err != nil {
		return a.internalError("docker_exec_failed", err)
	}
	finished := make(chan runResult, 1)
	go func() {
		status, err := started.Wait()
		finished <- runResult{status, err}
	}()
	abort := func(completed bool) {
		if !completed {
			if err := started.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				a.printWarning("docker_client_kill_failed", fmt.Sprintf("无法中止 docker start -a: %v", err))
			}
			select {
			case <-finished:
			case <-time.After(dockerStartConfirmTimeout):
				a.printWarning("docker_client_wait_failed", "中止后 docker start -a 仍未退出")
			}
		}
		a.stopAfterBindingFailure(binary, containerID, completed)
	}

	deadline := time.Now().Add(dockerStartConfirmTimeout)
	var completed *runResult
	for {
		state, actualSandbox, err := a.startIntentState(containerID)
		if err != nil {
			abort(completed != nil)
			return a.internalError("sandbox_binding_unknown", err)
		}
		if state == "consumed" {
			if actualSandbox != sandboxName {
				abort(completed != nil)
				return a.internalError("sandbox_binding_mismatch", fmt.Errorf("容器 %s 借条被沙盒 %q 认领，预期 %q", container, actualSandbox, sandboxName))
			}
			a.printStarted(container, containerID, sandboxName)
			if completed == nil {
				result := <-finished
				completed = &result
			}
			if completed.err != nil {
				return a.internalError("docker_exec_failed", completed.err)
			}
			return completed.status
		}
		if completed != nil || time.Now().After(deadline) {
			abort(completed != nil)
			if completed != nil && completed.err != nil {
				return a.internalError("docker_exec_failed", completed.err)
			} else if completed != nil && completed.status != 0 {
				return completed.status
			}
			a.printError("sandbox_not_bound", fmt.Sprintf("容器 %s 未绑定沙盒 %s；已尝试停止容器", container, sandboxName))
			return 1
		}
		select {
		case result := <-finished:
			completed = &result
		case <-time.After(dockerStartConfirmPoll):
		}
	}
}

func dockerStartAttachOption(options []string) (bool, error) {
	attached := false
	for index := 0; index < len(options); index++ {
		option := options[index]
		switch {
		case option == "-a" || option == "--attach" || option == "-ai" || option == "-ia":
			attached = true
		case option == "-i" || option == "--interactive":
			attached = true
		case option == "--checkpoint" || option == "--checkpoint-dir" ||
			strings.HasPrefix(option, "--checkpoint=") || strings.HasPrefix(option, "--checkpoint-dir="):
			return false, errors.New("neubox docker start 不支持 checkpoint 恢复；当前 runtime 只在普通容器创建时登记设备授权")
		case option == "--detach-keys":
			index++
			if index == len(options) {
				return false, fmt.Errorf("docker start 选项 %s 缺少参数", option)
			}
		case strings.HasPrefix(option, "--detach-keys="):
		case option == "-a=true" || option == "--attach=true" ||
			option == "-i=true" || option == "--interactive=true":
			attached = true
		case option == "-a=false" || option == "--attach=false" ||
			option == "-i=false" || option == "--interactive=false":
		case strings.HasPrefix(option, "-"):
			return false, fmt.Errorf("不支持的 docker start 选项 %s", option)
		default:
			return false, fmt.Errorf("neubox docker start 只接受一个容器；额外参数 %q", option)
		}
	}
	return attached, nil
}

// lendSandboxTo 存借条：把本进程（= 当前 shell）所在的沙盒借给这个容器。
//
// Worker 侧自己按 PID 反查沙盒并校验属主，客户端报的就是自己的 PID —— 它说
// 不出一个不属于自己的沙盒名。查不到沙盒、沙盒不是自己的、沙盒正在销毁，都
// 在这一步被拒，容器不会被启动。
func (a *app) lendSandboxTo(containerID string) (string, error) {
	payload := map[string]any{
		"username":     a.config.username,
		"pid":          a.getPID(),
		"container_id": containerID,
	}
	status, raw, err := a.worker.Request(
		http.MethodPost, "/container/intent", nil, payload)
	if err != nil {
		return "", fmt.Errorf("请求 Worker 失败: %w", err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		message, code := api.ErrorDetails(status, raw)
		if code != "" {
			return "", fmt.Errorf("HTTP %d %s (%s)", status, message, code)
		}
		return "", fmt.Errorf("HTTP %d %s", status, message)
	}
	var response struct {
		SandboxName string `json:"sandbox_name"`
	}
	if err := api.DecodeJSON(raw, &response); err != nil {
		return "", fmt.Errorf("Worker 返回的不是合法 JSON: %w", err)
	}
	sandboxName := strings.TrimSpace(response.SandboxName)
	if sandboxName == "" {
		return "", errors.New("Worker 没有返回借出去的沙盒名")
	}
	return sandboxName, nil
}

// confirmStartBinding 回查借条有没有被认领。
//
// Worker 只有完整登记成功后才把借条标成 consumed。
func (a *app) confirmStartBinding(dockerBinary, container, containerID, sandboxName string) int {
	bound, err := a.waitStartBinding(containerID, sandboxName)
	if err != nil {
		a.stopAfterBindingFailure(dockerBinary, containerID, true)
		return a.internalError("sandbox_binding_unknown", fmt.Errorf("无法确认容器 %s 是否已绑定沙盒 %s: %w", container, sandboxName, err))
	}
	if bound {
		return a.printStarted(container, containerID, sandboxName)
	}
	a.stopAfterBindingFailure(dockerBinary, containerID, true)
	a.printError("sandbox_not_bound", fmt.Sprintf("容器 %s 未绑定沙盒 %s；已尝试停止容器", container, sandboxName))
	return 1
}

func (a *app) waitStartBinding(containerID, expectedSandbox string) (bool, error) {
	deadline := time.Now().Add(dockerStartConfirmTimeout)
	for {
		state, sandboxName, err := a.startIntentState(containerID)
		if err != nil {
			return false, err
		}
		if state == "consumed" {
			if sandboxName != expectedSandbox {
				return false, fmt.Errorf("借条被沙盒 %q 认领，预期 %q", sandboxName, expectedSandbox)
			}
			return true, nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(dockerStartConfirmPoll)
	}
	return false, nil
}

// startIntentState 查借条的认领状态；没有借条时返回空串。
func (a *app) startIntentState(containerID string) (string, string, error) {
	query := url.Values{
		"container_id": []string{containerID},
		"username":     []string{a.config.username},
		"pid":          []string{strconv.Itoa(a.getPID())},
	}
	status, raw, err := a.worker.Request(
		http.MethodGet, "/container/intent", query, nil)
	if err != nil {
		return "", "", err
	}
	if err := api.ResponseError(status, raw); err != nil {
		message, _ := api.ErrorDetails(status, raw)
		return "", "", fmt.Errorf("HTTP %d %s", status, message)
	}
	var response struct {
		State       *string `json:"state"`
		SandboxName *string `json:"sandbox_name"`
	}
	if err := api.DecodeJSON(raw, &response); err != nil {
		return "", "", err
	}
	if response.State == nil {
		return "", "", nil
	}
	if response.SandboxName == nil {
		return *response.State, "", nil
	}
	return *response.State, *response.SandboxName, nil
}

func (a *app) stopAfterBindingFailure(dockerBinary, containerID string, startCompleted bool) {
	_, stopErr := a.outputFn(dockerBinary, "stop", containerID)
	if stopErr == nil {
		return
	}
	// If an attached client was killed while dockerd was still starting the
	// container, an early stop can report "not running". Recheck for a short
	// period and stop any delayed start. A completed Docker start needs only
	// one inspection because its daemon request has already returned.
	deadline := time.Now()
	if !startCompleted {
		deadline = deadline.Add(2 * time.Second)
	}
	var inspectErr error
	for {
		info, err := a.inspectDockerContainer(dockerBinary, containerID)
		inspectErr = err
		if err == nil && info.State.Running {
			_, stopErr = a.outputFn(dockerBinary, "stop", containerID)
			if stopErr == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			if err == nil && !info.State.Running {
				return
			}
			break
		}
		time.Sleep(dockerStartConfirmPoll)
	}
	if inspectErr != nil {
		a.printWarning("docker_inspect_failed", fmt.Sprintf("停止后无法确认容器 %s 状态: %v", shorthandContainerID(containerID), inspectErr))
	}
	a.printWarning("docker_stop_failed", fmt.Sprintf("无法停止授权未确认的容器 %s: %v", shorthandContainerID(containerID), stopErr))
}

func (a *app) printStarted(container, containerID, sandboxName string) int {
	if a.jsonOutput {
		_ = printJSONValue(a.out, map[string]any{
			"container":    container,
			"container_id": containerID,
			"sandbox":      sandboxName,
			"bound":        true,
		})
		return 0
	}
	printFields(a.out,
		outputField{"result", "started"},
		outputField{"container", shorthandContainerID(containerID)},
		outputField{"sandbox", sandboxName},
	)
	return 0
}

// shorthandContainerID 用 docker 自己的短 ID 口径（前 12 位）。
func shorthandContainerID(containerID string) string {
	if len(containerID) > 12 {
		return containerID[:12]
	}
	return containerID
}

// resolveOwnSandbox 反查本进程所在的沙盒，失败时自己打印错误并返回非 0 退出码。
//
// neubox 是 shell / acquire 管理的那个 shell 的子进程，cgroup 成员身份是继承的，
// 所以报**自己的** PID 就能让 Worker 反查到沙盒名。查不到就报错让用户先
// acquire —— 不猜、也不退化成"不加 annotation 照样起"：没有 annotation 的
// 容器拿不到设备，而且是在容器内第一次初始化时才炸，比在命令行上报错难查得多。
func (a *app) resolveOwnSandbox() (string, int) {
	// 容器里 PID 是 namespace 内视角，报给宿主机 Worker 会撞上别的进程，
	// 反查出一个别人的沙盒名。这条是错误分支，不在主路径上；先报错，
	// 异常处理以后再说。
	if a.insideContainer() {
		a.printError("sandbox_lookup_unsupported",
			"容器内无法按 PID 反查沙盒（PID namespace 与宿主机不同）；"+
				"请改用原生 docker run --annotation sandbox_cgroup=<沙盒名>")
		return "", 1
	}

	selfPID := a.getPID()
	query := url.Values{"pid": []string{strconv.Itoa(selfPID)}}
	status, raw, err := a.worker.Request(http.MethodGet, "/sandbox/status", query, nil)
	if err != nil {
		return "", a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return "", a.workerFailure(status, raw)
	}
	var response struct {
		SandboxName *string `json:"sandbox_name"`
	}
	if err := api.DecodeJSON(raw, &response); err != nil {
		return "", a.internalError("invalid_worker_response", err)
	}
	if response.SandboxName == nil || strings.TrimSpace(*response.SandboxName) == "" {
		a.printError("not_in_sandbox", fmt.Sprintf(
			"当前进程 (pid %d) 不在任何沙盒中；请先执行 neubox shell 或 neubox acquire，"+
				"或改用原生 docker run --annotation sandbox_cgroup=<沙盒名>", selfPID))
		return "", 1
	}
	return strings.TrimSpace(*response.SandboxName), 0
}

func (a *app) printDockerHelp() {
	fmt.Fprint(a.out, `neubox docker — 管理容器与沙盒的授权关系

用法:`+"\n    "+dockerRunUsage+`
    `+dockerStartUsage+`
    `+dockerRestartUsage+`
    `+dockerStatusUsage+`

说明:
    docker run 的参数一个不改，只在最前面补一行 annotation：
        neubox docker run --rm -it ubuntu bash
      = docker run --annotation sandbox_cgroup=<沙盒名> --rm -it ubuntu bash

    容器必须带这行 annotation：Worker 靠它把容器登记到沙盒名下，没登记的
    容器即使卡空着也一律拿不到设备（fail-closed）。需按 docs/deployment.md 配置
    Docker 默认 runtime，并由管理员重启 dockerd。

    沙盒按本进程 PID 反查（neubox 是 shell / acquire 管理的 shell 的子进程，cgroup
    身份是继承的）；查不到直接报错，不会退化成不加 annotation 启动。

    docker start 的事多一层：容器上那行 annotation 是建容器时写死的，而
    `+"`docker start`"+` 既没有 --annotation、也看不到是谁在调。所以这里先把这个
    shell 的沙盒存成一张借条（10 秒内有效、一次性），hook 登记时认领；
    认领失败会报错并尝试停止容器。原生 docker start 不会建立新的借条，
    不能用它换卡。需要等待容器原程序结束时使用 neubox docker start <容器> -a。
    checkpoint 恢复不经过当前 runtime 的普通 create 授权路径，因此拒绝。

    docker restart 用于运行中的受管容器：先停止并等待旧授权撤销，再按当前
    shell 的沙盒重新启动。它会中断容器里的工作；没有 annotation 的容器
    无法通过 restart 获得设备。已停止的容器请用 docker start。

    docker status 合并 Docker 运行状态与 Worker 当前登记；已停止的容器没有
    当前设备授权。查询到的设备是授权记录，不代表驱动健康状态。

    neubox docker exec 不提供借卡功能，也不会转发命令。进入运行中的容器请用
    原生 docker exec -it <容器> bash；执行前可用 neubox docker status 查询。

    没有自己的选项：要显式指定沙盒、或者要自己写 annotation，直接用原生
    docker run --annotation sandbox_cgroup=<沙盒名> ... 就好。
`)
}
