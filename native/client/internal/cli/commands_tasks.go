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

// defaultTasksWindow 是 tasks 默认视图的时间窗：已完成/失败的任务只展示
// finished_at 在窗口内的；活跃任务（queued/running）不受窗口限制。
const defaultTasksWindow = 2 * time.Hour

type tasksOptions struct {
	all   bool
	since time.Duration
}

func parseTasksOptions(args []string) (tasksOptions, error) {
	options := tasksOptions{since: defaultTasksWindow}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--all":
			options.all = true
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
		default:
			return options, fmt.Errorf("未知 tasks 选项: %s", args[index])
		}
	}
	return options, nil
}

func (a *app) runTasks(args []string) int {
	options, err := parseTasksOptions(args)
	if err != nil {
		return a.usageError(err.Error())
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

	if a.jsonOutput {
		if hidden == 0 {
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
		} else {
			item = append(item, outputField{"command", task.Command})
		}
		if task.Position > 0 {
			item = append(item, outputField{"position", fmt.Sprintf("#%d", task.Position)})
		}
		item = append(item, taskResourceFields(task)...)
		printFields(a.out, item...)
	}
	return 0
}

// filterTaskList 实现 tasks 默认视图：
//   - 活跃条目（status 为 queued/running，含排队中的 acquire 会话）永远保留；
//   - 已结束条目按 finished_at（缺失时退回 created_at）是否在窗口内决定；
//   - 无法解析或没有时间的条目不隐藏（宁多勿漏）。
//
// 返回保留的原始条目（JSON 输出时不丢 worker 额外字段）和被隐藏的条目数。
func filterTaskList(queue []json.RawMessage, options tasksOptions) ([]json.RawMessage, int) {
	if options.all {
		return queue, 0
	}
	cutoff := time.Now().Add(-options.since)
	visible := make([]json.RawMessage, 0, len(queue))
	for _, entry := range queue {
		if taskEntryVisible(entry, cutoff) {
			visible = append(visible, entry)
		}
	}
	return visible, len(queue) - len(visible)
}

func taskEntryVisible(entry json.RawMessage, cutoff time.Time) bool {
	var probe struct {
		Status     string   `json:"status"`
		CreatedAt  *float64 `json:"created_at"`
		FinishedAt *float64 `json:"finished_at"`
	}
	if err := json.Unmarshal(entry, &probe); err != nil {
		return true
	}
	if probe.Status == "queued" || probe.Status == "running" {
		return true
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
	TaskID     string      `json:"task_id"`
	Kind       string      `json:"kind"`
	ID         string      `json:"id"`
	UserID     string      `json:"user_id"`
	Command    string      `json:"command"`
	Status     string      `json:"status"`
	Position   int         `json:"position"`
	PID        int         `json:"pid"`
	Sandbox    string      `json:"sandbox_name"`
	CPU        int         `json:"cpu"`
	Mem        string      `json:"mem"`
	DeviceNum  int         `json:"device_num"`
	Devices    []string    `json:"devices"`
	CreatedAt  *float64    `json:"created_at"`
	FinishedAt *float64    `json:"finished_at"`
	Result     *taskResult `json:"result"`
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

func taskResourceFields(task taskResultResponse) []outputField {
	if task.CPU == 0 && (task.Mem == "" || task.Mem == "0") && task.DeviceNum == 0 && len(task.Devices) == 0 {
		return nil
	}
	devices := formatDevices(task.Devices)
	if len(task.Devices) == 0 && task.DeviceNum > 0 {
		devices = fmt.Sprintf("requested %d", task.DeviceNum)
	}
	return []outputField{
		{"devices", devices},
		{"cpu", formatCPU(task.CPU)},
		{"memory", formatMemory(task.Mem)},
	}
}
