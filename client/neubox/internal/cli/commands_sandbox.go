package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/neusbox/neu_box/client/neubox/internal/api"
)

func (a *app) runRelease(args []string) int {
	if len(args) > 1 {
		return a.usageError("用法: neubox release [sandbox_name]")
	}
	sandboxName := ""
	if len(args) == 1 {
		sandboxName = strings.TrimSpace(args[0])
	} else {
		shellPID := a.getPPID()
		raw, err := a.readFile(fmt.Sprintf("/proc/%d/cgroup", shellPID))
		if err != nil {
			return a.internalError("cgroup_read_failed", errors.New("无法读取当前 shell 的 cgroup 信息"))
		}
		sandboxName = sandboxNameFromCgroup(raw)
	}
	if sandboxName == "" {
		return a.usageError("当前 shell 不在沙盒中；如需释放其他沙盒，请指定 sandbox_name")
	}
	// 报上自己的 host PID：neubox 是 acquire 借出去的那个 shell fork 出来的
	// 子进程，cgroup 成员身份随 fork 继承 —— 它住在沙盒 cgroup 里却没有
	// origin，而销毁的最后一步是 cgroup.kill，不带这一项就会把自己一起杀掉
	// （zsh: killed、退出码 137）。Worker 拿到它会把**它自己**先搬回父进程的
	// origin，再销毁沙盒。
	payload := map[string]any{
		"sandbox_name": sandboxName,
		"host_pid":     a.getPID(),
	}
	status, raw, err := a.worker.Request(http.MethodPost, "/sandbox/release", nil, payload)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	if a.jsonOutput {
		_ = printJSON(a.out, raw)
		return 0
	}
	fmt.Fprintln(a.out, "[neubox] 沙盒已释放")
	fmt.Fprintf(a.out, "    sandbox: %s\n", sandboxName)
	return 0
}

type sandboxRecord struct {
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`
	CPU     int      `json:"cpu"`
	Mem     string   `json:"mem"`
	Devices []string `json:"devices"`
	State   string   `json:"state"`
}

type sandboxListResponse struct {
	Sandboxes      []sandboxRecord `json:"sandboxes"`
	CurrentSandbox *string         `json:"current_sandbox"`
}

func (a *app) runList(args []string) int {
	if len(args) != 0 {
		return a.usageError("用法: neubox list")
	}
	status, raw, err := a.worker.Request(http.MethodGet, "/sandbox/list", nil, nil)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	var response sandboxListResponse
	if err := api.DecodeJSON(raw, &response); err != nil {
		return a.internalError("invalid_worker_response", err)
	}
	if a.jsonOutput {
		_ = printJSON(a.out, raw)
		return 0
	}
	fmt.Fprintln(a.out, "[neubox] 沙盒列表")
	if len(response.Sandboxes) == 0 {
		fmt.Fprintln(a.out, "    (无)")
		return 0
	}
	for _, sandbox := range response.Sandboxes {
		owner := sandbox.Owner
		if owner == "" {
			owner = "?"
		}
		devices := "—"
		if len(sandbox.Devices) > 0 {
			devices = strings.Join(sandbox.Devices, ",")
		}
		resources := make([]string, 0, 2)
		if sandbox.CPU != 0 {
			resources = append(resources, fmt.Sprintf("CPU=%d", sandbox.CPU))
		}
		if sandbox.Mem != "" && sandbox.Mem != "0" {
			resources = append(resources, "mem="+sandbox.Mem)
		}
		resourceText := "资源不限"
		if len(resources) > 0 {
			resourceText = strings.Join(resources, " ")
		}
		fmt.Fprintf(a.out, "    %s\n", sandbox.Name)
		fmt.Fprintf(a.out, "        用户: %s  |  设备: %s  |  %s\n", owner, devices, resourceText)
	}
	return 0
}

func (a *app) runStatus(args []string) int {
	if len(args) != 0 {
		return a.usageError("用法: neubox status")
	}
	shellPID := a.getPPID()
	raw, err := a.readFile(fmt.Sprintf("/proc/%d/cgroup", shellPID))
	if err != nil {
		return a.internalError("cgroup_read_failed", errors.New("无法读取 cgroup 信息"))
	}
	sandboxName := sandboxNameFromCgroup(raw)
	var details *sandboxRecord
	if sandboxName != "" {
		query := url.Values{"pid": []string{strconv.Itoa(shellPID)}}
		// Local cgroup status remains useful while the Worker is unavailable.
		// Resource details are optional, so do not wait for the normal 30s timeout.
		worker := *a.worker
		httpClient := *a.worker.HTTP
		httpClient.Timeout = 2 * time.Second
		worker.HTTP = &httpClient
		status, body, err := worker.Request(http.MethodGet, "/sandbox/status", query, nil)
		if err == nil && status >= 200 && status < 300 {
			var response struct {
				SandboxName string         `json:"sandbox_name"`
				Sandbox     *sandboxRecord `json:"sandbox"`
			}
			if api.DecodeJSON(body, &response) == nil && response.SandboxName == sandboxName {
				details = response.Sandbox
			}
		}
	}
	return a.printShellStatus(shellPID, sandboxName, details)
}

func (a *app) printShellStatus(shellPID int, sandboxName string, details *sandboxRecord) int {
	if a.jsonOutput {
		var sandboxValue any
		if sandboxName != "" {
			sandboxValue = sandboxName
		}
		payload := map[string]any{
			"pid":     shellPID,
			"sandbox": sandboxValue,
		}
		if details != nil {
			payload["devices"] = details.Devices
			payload["state"] = details.State
		}
		_ = printJSONValue(a.out, payload)
		return 0
	}
	fmt.Fprintln(a.out, "[neubox] Shell 状态")
	fmt.Fprintf(a.out, "    pid: %d\n", shellPID)
	if sandboxName == "" {
		fmt.Fprintln(a.out, "    sandbox: none")
	} else {
		fmt.Fprintf(a.out, "    sandbox: %s\n", sandboxName)
		if details != nil {
			fmt.Fprintf(a.out, "    devices: %s\n", strings.Join(details.Devices, ","))
			if details.State != "" {
				fmt.Fprintf(a.out, "    state: %s\n", details.State)
			}
		}
		fmt.Fprintln(a.out, "    release: neubox release")
	}
	return 0
}

func sandboxNameFromCgroup(raw []byte) string {
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		marker := strings.Index(line, "sandbox_")
		if marker < 0 {
			continue
		}
		name := line[marker+len("sandbox_"):]
		if separator := strings.IndexRune(name, '/'); separator >= 0 {
			name = name[:separator]
		}
		if name != "" {
			return name
		}
	}
	return ""
}

func (a *app) runJoin(args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return a.usageError("用法: neubox join <sandbox_name>")
	}
	if a.insideContainer() {
		return a.usageError("容器终端不需要 join：从已 acquire 的 shell 启动子进程即可继承沙盒。")
	}
	sandboxName := strings.TrimSpace(args[0])
	shellPID := a.getPPID()
	payload := map[string]any{
		"username":     a.config.username,
		"pid":          shellPID,
		"sandbox_name": sandboxName,
	}
	status, raw, err := a.worker.Request(http.MethodPost, "/sandbox/join", nil, payload)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	if a.jsonOutput {
		_ = printJSON(a.out, raw)
		return 0
	}
	fmt.Fprintln(a.out, "[neubox] 已加入沙盒")
	fmt.Fprintf(a.out, "    sandbox: %s\n", sandboxName)
	fmt.Fprintf(a.out, "    pid: %d\n", shellPID)
	return 0
}
