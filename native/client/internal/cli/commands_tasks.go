package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neusbox/neu_box/native/client/internal/api"
)

type taskQueueResponse struct {
	Queue        []json.RawMessage `json:"queue"`
	TotalPending int               `json:"total_pending"`
}

type queuePosition struct {
	Priority int `json:"priority"`
	Rank     int `json:"rank"`
}

func formatQueuePosition(position *queuePosition) string {
	if position == nil || position.Rank <= 0 {
		return "none"
	}
	return fmt.Sprintf("priority %d, rank %d", position.Priority, position.Rank)
}

// defaultTasksWindow 是 tasks 默认视图的时间窗：已完成/失败的任务只展示
// finished_at 在窗口内的；活跃任务（queued/running）不受窗口限制。
const defaultTasksWindow = 2 * time.Hour

type tasksOptions struct {
	all       bool
	history   bool
	sandboxes bool
	since     time.Duration
}

func parseTasksOptions(args []string) (tasksOptions, error) {
	options := tasksOptions{since: defaultTasksWindow}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--all":
			options.all = true
			options.history = true
		case "--history":
			options.history = true
		case "--sandboxes":
			options.sandboxes = true
		case "--since":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			duration, err := time.ParseDuration(raw)
			if err != nil || duration <= 0 {
				return options, fmt.Errorf("--since 必须是正数时间间隔，例如 30m 或 6h：%q", raw)
			}
			options.since = duration
			options.history = true
		default:
			return options, fmt.Errorf("未知 list 选项: %s", args[index])
		}
	}
	if options.sandboxes && options.history {
		return options, fmt.Errorf("--sandboxes 不能与历史记录选项同时使用")
	}
	return options, nil
}

func (a *app) runTasks(args []string) int { return a.runList(args) }

func (a *app) runList(args []string) int {
	options, err := parseTasksOptions(args)
	if err != nil {
		return a.usageError(err.Error())
	}
	if options.sandboxes {
		return a.runSandboxList(nil)
	}
	status, raw, err := a.worker.Request(
		http.MethodGet,
		"/tasks",
		nil,
		nil,
	)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	var response taskQueueResponse
	if err := api.DecodeJSON(raw, &response); err != nil {
		return a.internalError("invalid_worker_response", err)
	}
	visible, hidden := filterTaskList(response.Queue, options)
	// A sandbox normally belongs to a running task or an active acquire. Keep
	// any other live sandbox visible as its own row instead of silently dropping
	// an allocation from the overview.
	sandboxStatus, sandboxRaw, err := a.worker.Request(http.MethodGet, "/sandbox/list", nil, nil)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(sandboxStatus, sandboxRaw); err != nil {
		return a.workerFailure(sandboxStatus, sandboxRaw)
	}
	var sandboxes sandboxListResponse
	if err := api.DecodeJSON(sandboxRaw, &sandboxes); err != nil {
		return a.internalError("invalid_worker_response", err)
	}
	linked := make(map[string]bool)
	for _, entry := range visible {
		var item taskResultResponse
		if api.DecodeJSON(entry, &item) == nil && item.Sandbox != "" {
			linked[item.Sandbox] = true
		}
	}
	for _, sandbox := range sandboxes.Sandboxes {
		if linked[sandbox.Name] {
			continue
		}
		entry, err := json.Marshal(map[string]any{
			"id": sandbox.Name, "kind": "sandbox",
			"user_id": sandbox.Owner, "status": strings.ToLower(sandbox.State),
			"sandbox_name": sandbox.Name, "devices": sandbox.Devices,
			"cpu": sandbox.CPU, "mem": sandbox.Mem,
		})
		if err != nil {
			return a.internalError("invalid_worker_response", err)
		}
		visible = append(visible, entry)
	}

	if a.jsonOutput {
		if hidden == 0 && len(visible) == len(response.Queue) {
			// 没有过滤掉任何条目：直接透传 Worker 原始响应。
			_ = printJSON(a.out, raw)
		} else {
			// RawMessage 保留 Worker 返回的全部字段（eta/priority/target 等）。
			_ = printJSONValue(a.out, struct {
				Queue        []json.RawMessage `json:"queue"`
				TotalPending int               `json:"total_pending"`
			}{visible, response.TotalPending})
		}
		return 0
	}
	fields := []outputField{
		{"count", fmt.Sprint(len(visible))},
		{"pending", fmt.Sprint(response.TotalPending)},
	}
	if hidden > 0 {
		fields = append(fields, outputField{"hidden", fmt.Sprint(hidden)})
	}
	printFields(a.out, fields...)
	tasks := make([]taskResultResponse, 0, len(visible))
	for _, entry := range visible {
		var task taskResultResponse
		if err := api.DecodeJSON(entry, &task); err != nil {
			continue
		}
		tasks = append(tasks, task)
	}
	if len(tasks) == 0 {
		return 0
	}
	for _, task := range tasks {
		fmt.Fprintln(a.out)
		item := []outputField{
			{"id", entryID(task)},
			{"kind", entryKind(task)},
			{"state", task.Status},
			{"user", task.UserID},
		}
		if entryKind(task) == "acquire" {
			// acquire 是会话不是命令任务：没有 command / 日志 / 退出码。
			if task.PID != 0 {
				item = append(item, outputField{"pid", fmt.Sprint(task.PID)})
			}
			if task.Sandbox != "" {
				item = append(item, outputField{"sandbox", task.Sandbox})
			}
		} else if entryKind(task) == "task" {
			item = append(item, outputField{"command", task.Command})
		}
		if task.Sandbox != "" && entryKind(task) != "acquire" {
			item = append(item, outputField{"sandbox", task.Sandbox})
		}
		if task.QueuePosition != nil {
			item = append(item, outputField{"position", formatQueuePosition(task.QueuePosition)})
		}
		if entryKind(task) == "acquire" {
			if devices := taskDeviceText(task); devices != "" {
				item = append(item, outputField{"devices", devices})
			}
		} else {
			item = append(item, taskResourceFields(task)...)
		}
		printFields(a.out, item...)
	}
	return 0
}

// filterTaskList 默认只显示活跃请求。--history 包含时间窗内的终态记录，
// --all 包含 Worker 返回的全部最近记录。
//
// 返回保留的原始条目（JSON 输出时不丢 worker 额外字段）和被隐藏的条目数。
func filterTaskList(queue []json.RawMessage, options tasksOptions) ([]json.RawMessage, int) {
	if options.all {
		return queue, 0
	}
	cutoff := time.Now().Add(-options.since)
	visible := make([]json.RawMessage, 0, len(queue))
	for _, entry := range queue {
		if taskEntryVisible(entry, cutoff, options.history) {
			visible = append(visible, entry)
		}
	}
	return visible, len(queue) - len(visible)
}

func taskEntryVisible(entry json.RawMessage, cutoff time.Time, history bool) bool {
	var probe struct {
		Status     string   `json:"status"`
		CreatedAt  *float64 `json:"created_at"`
		FinishedAt *float64 `json:"finished_at"`
	}
	if err := json.Unmarshal(entry, &probe); err != nil {
		return true
	}
	if probe.Status == "queued" || probe.Status == "allocating" ||
		probe.Status == "running" || probe.Status == "active" {
		return true
	}
	if !history {
		return false
	}
	timestamp := probe.FinishedAt
	if timestamp == nil {
		timestamp = probe.CreatedAt
	}
	if timestamp == nil {
		return true
	}
	return time.Unix(int64(*timestamp), 0).After(cutoff)
}

// entryKind / entryID 兼容两类条目：任务用 task_id，acquire 会话用 request_id。
// 老 Worker 不带 kind/id 字段时按任务处理。
func entryKind(entry taskResultResponse) string {
	if entry.Kind != "" {
		return entry.Kind
	}
	return "task"
}

func entryID(entry taskResultResponse) string {
	if entry.ID != "" {
		return entry.ID
	}
	return entry.TaskID
}

type taskResult struct {
	ReturnCode *int `json:"returncode"`
	TimedOut   bool `json:"timed_out"`
	Error      any  `json:"error"`
}

type taskResultResponse struct {
	TaskID        string         `json:"task_id"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	UserID        string         `json:"user_id"`
	Command       string         `json:"command"`
	Status        string         `json:"status"`
	Position      int            `json:"position"`
	QueuePosition *queuePosition `json:"queue_position"`
	PID           int            `json:"pid"`
	Sandbox       string         `json:"sandbox_name"`
	CPU           int            `json:"cpu"`
	Mem           string         `json:"mem"`
	DeviceNum     int            `json:"device_num"`
	DeviceIDs     []string       `json:"device_ids"`
	Devices       []string       `json:"devices"`
	CreatedAt     *float64       `json:"created_at"`
	FinishedAt    *float64       `json:"finished_at"`
	Result        *taskResult    `json:"result"`
}

func (a *app) runResult(args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return a.usageError("用法: neubox result <task_id>")
	}
	taskID := strings.TrimSpace(args[0])
	pathID := url.PathEscape(taskID)
	status, raw, err := a.worker.Request(http.MethodGet, "/tasks/"+pathID, nil, nil)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	var response taskResultResponse
	if err := api.DecodeJSON(raw, &response); err != nil {
		return a.internalError("invalid_worker_response", err)
	}

	logStatus, logRaw, logErr := a.worker.Request(
		http.MethodGet,
		"/tasks/"+pathID+"/log",
		url.Values{"raw": []string{"1"}},
		nil,
	)
	if logErr != nil {
		return a.requestError(fmt.Errorf("获取任务日志: %w", logErr))
	}
	if err := api.ResponseError(logStatus, logRaw); err != nil {
		return a.workerFailure(logStatus, logRaw)
	}

	if a.jsonOutput {
		var output map[string]any
		if err := api.DecodeJSON(raw, &output); err != nil {
			return a.internalError("invalid_worker_response", err)
		}
		output["log"] = string(logRaw)
		_ = printJSONValue(a.out, output)
		return 0
	}

	fields := []outputField{
		{"task", response.TaskID},
		{"state", response.Status},
	}
	if response.Result != nil && response.Result.ReturnCode != nil {
		fields = append(fields, outputField{"return_code", fmt.Sprint(*response.Result.ReturnCode)})
		if response.Result.TimedOut {
			fields = append(fields, outputField{"timed_out", "true"})
		}
		if response.Result.Error != nil {
			fields = append(fields, outputField{"error", fmt.Sprint(response.Result.Error)})
		}
	}
	fields = append(fields,
		outputField{"user", response.UserID},
		outputField{"command", response.Command},
	)
	fields = append(fields, taskResourceFields(response)...)
	timestamp := response.FinishedAt
	if timestamp == nil {
		timestamp = response.CreatedAt
	}
	if timestamp != nil {
		formatted := time.Unix(int64(*timestamp), 0).Local().Format("01-02 15:04")
		fields = append(fields, outputField{"time", formatted})
	}
	printFields(a.out, fields...)
	if len(logRaw) > 0 {
		fmt.Fprintln(a.out)
		fmt.Fprintln(a.out, "log:")
		fmt.Fprintln(a.out, strings.TrimRight(string(logRaw), "\r\n"))
	}
	return 0
}

func taskDeviceText(task taskResultResponse) string {
	if len(task.Devices) > 0 {
		return formatDevices(task.Devices)
	}
	if len(task.DeviceIDs) > 0 {
		return "requested " + formatDevices(task.DeviceIDs)
	}
	if task.DeviceNum > 0 {
		return fmt.Sprintf("requested %d", task.DeviceNum)
	}
	return ""
}

func taskResourceFields(task taskResultResponse) []outputField {
	devices := taskDeviceText(task)
	if task.CPU == 0 && (task.Mem == "" || task.Mem == "0") && devices == "" {
		return nil
	}
	if devices == "" {
		devices = "none"
	}
	return []outputField{
		{"devices", devices},
		{"cpu", formatCPU(task.CPU)},
		{"memory", formatMemory(task.Mem)},
	}
}
