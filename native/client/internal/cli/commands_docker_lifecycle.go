package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neusbox/neu_box/native/client/internal/api"
)

const (
	dockerStopConfirmTimeout = 10 * time.Second
	dockerStopConfirmPoll    = 100 * time.Millisecond
)

type dockerContainerInfo struct {
	ID    string `json:"Id"`
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	HostConfig struct {
		Annotations map[string]string `json:"Annotations"`
	} `json:"HostConfig"`
}

func (a *app) inspectDockerContainer(binary, container string) (dockerContainerInfo, error) {
	var info dockerContainerInfo
	raw, err := a.outputFn(binary, "inspect", "--format", "{{json .}}", container)
	if err != nil {
		return info, fmt.Errorf("docker inspect %s 失败: %w", container, err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, fmt.Errorf("docker inspect %s 返回的不是合法 JSON: %w", container, err)
	}
	if len(info.ID) != 64 {
		return info, fmt.Errorf("docker inspect %s 没有返回完整容器 ID", container)
	}
	return info, nil
}

type dockerBinding struct {
	SandboxName *string        `json:"sandbox_name"`
	Sandbox     *sandboxRecord `json:"sandbox"`
}

func (a *app) lookupDockerBinding(containerID string) (dockerBinding, error) {
	var binding dockerBinding
	query := url.Values{"container": []string{containerID}}
	status, raw, err := a.worker.Request(http.MethodGet, "/sandbox/status", query, nil)
	if err != nil {
		return binding, err
	}
	if err := api.ResponseError(status, raw); err != nil {
		return binding, err
	}
	if err := api.DecodeJSON(raw, &binding); err != nil {
		return binding, err
	}
	return binding, nil
}

func (a *app) runDockerStatus(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return a.usageError("用法: " + dockerStatusUsage)
	}
	binary, err := a.lookPath("docker")
	if err != nil {
		a.printError("docker_not_found", "未找到 Docker CLI。请确认 Docker 已安装且 docker 命令位于 PATH 中")
		return 1
	}
	container := args[0]
	info, err := a.inspectDockerContainer(binary, container)
	if err != nil {
		return a.internalError("docker_inspect_failed", err)
	}
	managed := strings.TrimSpace(info.HostConfig.Annotations["sandbox_cgroup"]) != ""
	var binding dockerBinding
	var bindingErr error
	if info.State.Running {
		binding, bindingErr = a.lookupDockerBinding(info.ID)
	}
	if a.jsonOutput {
		payload := map[string]any{
			"container":       container,
			"container_id":    info.ID,
			"container_state": info.State.Status,
			"managed":         managed,
			"sandbox":         nil,
			"devices":         nil,
		}
		if bindingErr != nil {
			payload["authorization"] = "unknown"
			payload["error"] = bindingErr.Error()
		} else if binding.SandboxName != nil && binding.Sandbox != nil {
			payload["authorization"] = "bound"
			payload["sandbox"] = *binding.SandboxName
			payload["devices"] = binding.Sandbox.Devices
			payload["sandbox_state"] = binding.Sandbox.State
			payload["cpu"] = binding.Sandbox.CPU
			payload["memory"] = binding.Sandbox.Mem
		} else if binding.SandboxName != nil {
			payload["authorization"] = "unknown"
			payload["sandbox"] = *binding.SandboxName
			payload["error"] = "容器授权信息不完整，请检查 Worker 状态"
		} else {
			payload["authorization"] = "none"
		}
		_ = printJSONValue(a.out, payload)
		if bindingErr != nil || binding.SandboxName != nil && binding.Sandbox == nil {
			return 1
		}
		return 0
	}
	managedText := "no"
	if managed {
		managedText = "yes"
	}
	fields := []outputField{
		{"container", container},
		{"container_state", info.State.Status},
		{"managed", managedText},
	}
	if bindingErr != nil {
		printFields(a.out, append(fields, outputField{"sandbox", "unknown"}, outputField{"devices", "unknown"})...)
		a.printError("worker_request_failed", fmt.Sprintf("无法查询容器授权: %v", bindingErr))
		return 1
	}
	if binding.SandboxName == nil {
		printFields(a.out, append(fields, outputField{"sandbox", "none"}, outputField{"devices", "none"})...)
		return 0
	}
	fields = append(fields, outputField{"sandbox", *binding.SandboxName})
	if binding.Sandbox == nil {
		printFields(a.out, append(fields, outputField{"devices", "unknown"})...)
		a.printWarning("sandbox_record_missing", "容器授权信息不完整，请检查 Worker 状态")
		return 1
	}
	printFields(a.out, append(fields,
		outputField{"state", binding.Sandbox.State},
		outputField{"devices", formatDevices(binding.Sandbox.Devices)},
		outputField{"cpu", formatCPU(binding.Sandbox.CPU)},
		outputField{"memory", formatMemory(binding.Sandbox.Mem)},
	)...)
	return 0
}

func (a *app) waitDockerUnregistered(containerID string) error {
	deadline := time.Now().Add(dockerStopConfirmTimeout)
	for {
		binding, err := a.lookupDockerBinding(containerID)
		if err != nil {
			return fmt.Errorf("容器已停止，但无法确认旧授权已撤销: %w", err)
		}
		if binding.SandboxName == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("容器已停止，但旧授权仍未撤销；请检查 Worker 后再启动")
		}
		time.Sleep(dockerStopConfirmPoll)
	}
}

func (a *app) runDockerRestart(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return a.usageError("用法: " + dockerRestartUsage)
	}
	if a.insideContainer() {
		return a.usageError("请在宿主机终端中执行 neubox docker restart")
	}
	binary, err := a.lookPath("docker")
	if err != nil {
		a.printError("docker_not_found", "未找到 Docker CLI。请确认 Docker 已安装且 docker 命令位于 PATH 中")
		return 1
	}
	container := args[0]
	info, err := a.inspectDockerContainer(binary, container)
	if err != nil {
		return a.internalError("docker_inspect_failed", err)
	}
	if !info.State.Running {
		return a.usageError("容器已经停止；请使用 neubox docker start " + container)
	}
	if strings.TrimSpace(info.HostConfig.Annotations["sandbox_cgroup"]) == "" {
		return a.usageError("该容器不受 Neu Box 管理，无法更换设备授权。请使用 neubox docker run 创建容器")
	}
	if _, code := a.resolveOwnSandbox(); code != 0 {
		return code
	}
	// Fail before stopping the running workload if Worker status is unavailable.
	if _, err := a.lookupDockerBinding(info.ID); err != nil {
		return a.internalError("worker_request_failed", err)
	}
	// Docker prints the container name on success. Capture it so this managed
	// command keeps its own two-column result (including in --json mode).
	if _, err := a.outputFn(binary, "stop", container); err != nil {
		return a.internalError("docker_stop_failed", err)
	}
	if err := a.waitDockerUnregistered(info.ID); err != nil {
		return a.internalError("container_unregistration_failed", err)
	}
	sandboxName, err := a.lendSandboxTo(info.ID)
	if err != nil {
		return a.internalError("sandbox_not_lent", fmt.Errorf("容器已停止，但设备授权失败: %w", err))
	}
	if _, err := a.outputFn(binary, "start", container); err != nil {
		return a.internalError("docker_start_failed", err)
	}
	bound, err := a.waitStartBinding(info.ID, sandboxName)
	if err != nil || !bound {
		// A failed binding must not leave a supposedly GPU-ready container running.
		_, stopErr := a.outputFn(binary, "stop", container)
		if stopErr != nil {
			a.printWarning("docker_stop_failed", fmt.Sprintf("设备授权未确认，停止容器失败：%v。容器可能仍在运行", stopErr))
		}
		if err != nil {
			return a.internalError("sandbox_binding_unknown", fmt.Errorf("重启后无法确认授权: %w", err))
		}
		message := "重启后无法确认容器的设备授权；请检查 Docker 和 Worker 状态"
		if stopErr == nil {
			message += "；容器已停止"
		} else {
			message += "；容器可能仍在运行"
		}
		a.printError("sandbox_not_bound", message)
		return 1
	}
	if a.jsonOutput {
		_ = printJSONValue(a.out, map[string]any{"container": container, "container_id": info.ID, "sandbox": sandboxName, "result": "restarted"})
		return 0
	}
	printFields(a.out,
		outputField{"result", "restarted"},
		outputField{"container", container},
		outputField{"sandbox", sandboxName},
	)
	return 0
}
