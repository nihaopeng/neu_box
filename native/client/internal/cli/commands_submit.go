package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/neusbox/neu_box/native/client/internal/api"
)

type commandTarget struct {
	Type    string            `json:"type"`
	Image   string            `json:"image,omitempty"`
	Workdir *string           `json:"workdir,omitempty"`
	User    *string           `json:"user,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Mounts  []commandMount    `json:"mounts,omitempty"`
}

type commandMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type commandRequest struct {
	UserID      string         `json:"user_id"`
	Command     string         `json:"command,omitempty"`
	CommandArgv []string       `json:"command_argv,omitempty"`
	Script      string         `json:"script,omitempty"`
	DeviceNum   int            `json:"device_num"`
	DeviceIDs   []string       `json:"device_ids"`
	CPU         int            `json:"cpu"`
	Memory      int            `json:"memory"`
	MemUnit     string         `json:"mem_unit"`
	Priority    int            `json:"priority"`
	Target      *commandTarget `json:"target,omitempty"`
}

type commandResponse struct {
	TaskID        string         `json:"task_id"`
	Position      int            `json:"position"`
	QueuePosition *queuePosition `json:"queue_position"`
	Priority      int            `json:"priority"`
	Error         string         `json:"error"`
}

func (a *app) runSubmit(args []string) int {
	options, err := parseSubmitOptions(args)
	if err != nil {
		return a.usageError(err.Error())
	}
	return a.submitCommand(options)
}

func parseSubmitOptions(args []string) (submitOptions, error) {
	options := submitOptions{environment: make(map[string]string)}

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if options.commandSet || options.scriptPath != "" {
				return options, errors.New("submit 只能指定一个命令")
			}
			if index+1 >= len(args) || args[index+1] == "" {
				return options, errors.New("submit 缺少命令；请在 -- 后指定命令")
			}
			options.commandArgv = append([]string(nil), args[index+1:]...)
			break
		}
		if handled, err := consumeResourceOption(args, &index, &options.resourceOptions); handled || err != nil {
			if err != nil {
				return options, err
			}
			continue
		}

		switch argument {
		case "--command":
			if options.commandSet || options.scriptPath != "" {
				return options, errors.New("submit 只能指定一个命令")
			}
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.command = strings.TrimSpace(raw)
			options.commandSet = true
		case "--script":
			if options.commandSet || options.scriptPath != "" {
				return options, errors.New("submit 只能指定一个命令")
			}
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			if strings.TrimSpace(raw) == "" {
				return options, errors.New("--script 需要文件路径或 -（标准输入）")
			}
			options.scriptPath = raw
		case "--container":
			return options, errors.New("submit 不支持 --container。新容器请在 -- 后运行 neubox docker run；已有的停止容器请在脚本中使用 neubox docker start")
		case "--image":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.image = strings.TrimSpace(raw)
			if options.image == "" {
				return options, errors.New("--image 不能为空")
			}
		case "--priority":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.priority, err = nonNegativeInteger("--priority", raw)
			if err != nil {
				return options, err
			}
		case "--wait":
			options.wait = true
		case "--workdir":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.workdir = raw
		case "--container-user":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.containerUser = raw
		case "--mount":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.mounts = append(options.mounts, raw)
		case "--project":
			options.project = true
		case "--output":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			options.output = raw
		case "--env":
			raw, err := optionValue(args, &index)
			if err != nil {
				return options, err
			}
			key, envValue, found := strings.Cut(raw, "=")
			if !found {
				key = raw
				envValue, found = os.LookupEnv(key)
				if !found {
					return options, fmt.Errorf("本地环境变量 %s 不存在", key)
				}
			}
			if strings.TrimSpace(key) == "" {
				return options, fmt.Errorf("--env 必须是 KEY 或 KEY=VALUE: %s", raw)
			}
			options.environment[key] = envValue
		default:
			if strings.HasPrefix(argument, "-") {
				return options, fmt.Errorf("未知 submit 选项: %s", argument)
			}
			return options, fmt.Errorf("无法识别 submit 参数 %q；命令必须放在 -- 后", argument)
		}
	}

	if options.scriptPath == "" && len(options.commandArgv) == 0 && strings.TrimSpace(options.command) == "" {
		return options, errors.New("submit 缺少命令；请在 -- 后指定命令，或使用 --script FILE|-")
	}
	if options.image == "" && (options.containerUser != "" || len(options.mounts) > 0 || options.project || options.output != "") {
		return options, errors.New("--container-user/--mount/--project/--output 必须配合 --image")
	}
	if options.image != "" && options.scriptPath != "" {
		return options, errors.New("--script 仅用于宿主机任务；容器任务请在脚本中使用 neubox docker run")
	}
	if err := validateResourceOptions(&options.resourceOptions); err != nil {
		return options, err
	}
	return options, nil
}

func joinCommandArguments(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, quoteCommandArgument(argument))
	}
	return strings.Join(quoted, " ")
}

func quoteCommandArgument(argument string) string {
	if argument == "" {
		return "''"
	}
	if strings.IndexFunc(argument, func(character rune) bool {
		return !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			!strings.ContainsRune("_@%+=:,./-", character)
	}) == -1 {
		return argument
	}
	return "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
}

func (a *app) submitCommand(options submitOptions) int {
	if options.wait && a.jsonOutput {
		return a.usageError("submit --wait 会连续输出日志，不支持 --json")
	}
	var script string
	if options.scriptPath != "" {
		var raw []byte
		var err error
		if options.scriptPath == "-" {
			input := a.in
			if input == nil {
				input = os.Stdin
			}
			raw, err = io.ReadAll(input)
		} else {
			raw, err = a.readFile(options.scriptPath)
		}
		if err != nil {
			return a.usageError(fmt.Sprintf("读取脚本 %s 失败: %v", options.scriptPath, err))
		}
		if !utf8.Valid(raw) || strings.ContainsRune(string(raw), '\x00') {
			return a.usageError("脚本必须是 UTF-8 文本，不能包含 NUL 字节")
		}
		script = string(raw)
		if strings.TrimSpace(script) == "" {
			return a.usageError("脚本不能为空")
		}
	}
	workingDirectory, err := a.getwd()
	if err != nil {
		return a.usageError(fmt.Sprintf("无法读取当前工作目录: %v", err))
	}
	payload := commandRequest{
		UserID:      a.config.username,
		Command:     options.command,
		CommandArgv: append([]string(nil), options.commandArgv...),
		Script:      script,
		DeviceNum:   options.deviceNum,
		DeviceIDs:   options.deviceIDs,
		CPU:         options.cpu,
		Memory:      options.memory,
		MemUnit:     "GB",
		Priority:    options.priority,
	}
	if options.image != "" {
		// The old image executor still accepts a Docker command string. New
		// host submissions keep argv intact in command_argv.
		if len(options.commandArgv) > 0 {
			payload.Command = joinCommandArguments(options.commandArgv)
			payload.CommandArgv = nil
		}
		mounts, workdir, err := a.dockerSubmitMounts(options, workingDirectory)
		if err != nil {
			return a.usageError(err.Error())
		}
		payload.Target = &commandTarget{
			Type:    "docker",
			Image:   options.image,
			Workdir: nullableString(workdir),
			User:    nullableString(options.containerUser),
			Env:     options.environment,
			Mounts:  mounts,
		}
	} else {
		workdir := workingDirectory
		if options.workdir != "" {
			workdir = options.workdir
		}
		if !filepath.IsAbs(workdir) {
			workdir = filepath.Join(workingDirectory, workdir)
		}
		payload.Target = &commandTarget{
			Type: "host", Workdir: nullableString(workdir), Env: options.environment,
		}
	}
	status, raw, err := a.worker.Request(http.MethodPost, "/tasks", nil, payload)
	if err != nil {
		return a.requestError(err)
	}
	if err := api.ResponseError(status, raw); err != nil {
		return a.workerFailure(status, raw)
	}
	var response commandResponse
	if err := api.DecodeJSON(raw, &response); err != nil || response.TaskID == "" {
		if err == nil {
			err = errors.New("Worker 响应缺少 task_id")
		}
		return a.internalError("invalid_worker_response", err)
	}
	if a.jsonOutput {
		_ = printJSON(a.out, raw)
		return 0
	}
	fields := []outputField{
		{"result", "submitted"},
		{"task", response.TaskID},
	}
	if response.QueuePosition != nil {
		fields = append(fields, outputField{"position", formatQueuePosition(response.QueuePosition)})
	}
	deviceText := formatDevices(options.deviceIDs)
	if len(options.deviceIDs) == 0 && options.deviceNum > 0 {
		deviceText = fmt.Sprintf("requested %d", options.deviceNum)
	}
	memoryText := "0"
	if options.memory > 0 {
		memoryText = fmt.Sprintf("%dG", options.memory)
	}
	fields = append(fields,
		outputField{"devices", deviceText},
		outputField{"cpu", formatCPU(options.cpu)},
		outputField{"memory", formatMemory(memoryText)},
	)
	if payload.Target != nil {
		if payload.Target.Type == "docker" {
			fields = append(fields, outputField{"image", payload.Target.Image})
			for _, mount := range payload.Target.Mounts {
				mode := "read-only"
				if !mount.ReadOnly {
					mode = "writable"
				}
				fields = append(fields, outputField{"mount", fmt.Sprintf("%s → %s (%s)", mount.Source, mount.Target, mode)})
			}
		} else if payload.Target.Workdir != nil {
			fields = append(fields, outputField{"workdir", *payload.Target.Workdir})
		}
	}
	switch {
	case options.scriptPath != "":
		fields = append(fields, outputField{"script", options.scriptPath})
	case len(options.commandArgv) > 0:
		fields = append(fields, outputField{"command", joinCommandArguments(options.commandArgv)})
	default:
		fields = append(fields, outputField{"command", options.command})
	}
	printFields(a.out, fields...)
	if options.wait {
		return a.runWait([]string{response.TaskID})
	}
	return 0
}

func absoluteMountSource(source, cwd string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("挂载源路径不能为空")
	}
	absolute := source
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(cwd, absolute)
	}
	absolute = filepath.Clean(absolute)
	if _, err := os.Stat(absolute); err != nil {
		return "", fmt.Errorf("挂载源 %s 不可访问: %w", absolute, err)
	}
	return absolute, nil
}

func (a *app) dockerSubmitMounts(options submitOptions, cwd string) ([]commandMount, string, error) {
	mounts := make([]commandMount, 0, len(options.mounts)+2)
	workdir := options.workdir
	if options.project {
		mounts = append(mounts, commandMount{Source: cwd, Target: "/workspace"})
		if workdir == "" {
			workdir = "/workspace"
		}
	}
	for _, spec := range options.mounts {
		parts := strings.Split(spec, ":")
		if len(parts) < 2 || len(parts) > 3 || !strings.HasPrefix(parts[1], "/") {
			return nil, "", fmt.Errorf("--mount 格式为 宿主路径:容器绝对路径[:ro|rw]: %q", spec)
		}
		mode := "ro"
		if len(parts) == 3 {
			mode = parts[2]
		}
		if mode != "ro" && mode != "rw" {
			return nil, "", fmt.Errorf("--mount 只接受 ro 或 rw: %q", spec)
		}
		source, err := absoluteMountSource(parts[0], cwd)
		if err != nil {
			return nil, "", err
		}
		mounts = append(mounts, commandMount{Source: source, Target: parts[1], ReadOnly: mode == "ro"})
	}
	if options.output != "" {
		parts := strings.Split(options.output, ":")
		if len(parts) > 2 || parts[0] == "" {
			return nil, "", errors.New("--output 格式为 宿主目录[:容器绝对目录]")
		}
		target := "/outputs"
		if len(parts) == 2 {
			target = parts[1]
		}
		if !strings.HasPrefix(target, "/") {
			return nil, "", errors.New("--output 容器目录必须是绝对路径")
		}
		source := parts[0]
		if !filepath.IsAbs(source) {
			source = filepath.Join(cwd, source)
		}
		source = filepath.Clean(source)
		if err := os.MkdirAll(source, 0755); err != nil {
			return nil, "", fmt.Errorf("创建输出目录 %s 失败: %w", source, err)
		}
		mounts = append(mounts, commandMount{Source: source, Target: target})
	}
	return mounts, workdir, nil
}
