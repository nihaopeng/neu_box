package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// runShell creates a sandbox for this CLI process, then runs an interactive
// child shell inside it. The caller's shell stays outside the sandbox.
func (a *app) runShell(args []string) int {
	if a.jsonOutput {
		return a.usageError("shell 是交互命令，不支持 --json")
	}
	options := acquireOptions{}
	for index := 0; index < len(args); index++ {
		handled, err := consumeResourceOption(args, &index, &options.resourceOptions)
		if err != nil {
			return a.usageError(err.Error())
		}
		if !handled {
			return a.usageError(fmt.Sprintf("未知 shell 选项: %s；用法: neubox shell [资源选项]", args[index]))
		}
	}
	if err := validateResourceOptions(&options.resourceOptions); err != nil {
		return a.usageError(err.Error())
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	binary, err := a.lookPath(shell)
	if err != nil {
		a.printError("shell_not_found", fmt.Sprintf("未找到终端程序 %q：%v", shell, err))
		return 1
	}
	// Keep the supervisor alive throughout acquire, child startup and cleanup.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupts)
	// Only neubox and its children enter this sandbox. The existing terminal
	// shell remains outside, so leaving the child shell can release everything.
	options.pid = a.getPID()
	options.pidSet = true
	if code := a.runTerminalAcquire(options); code != 0 {
		return code
	}
	sandboxName := a.acquiredSandbox
	if sandboxName == "" {
		return a.internalError("invalid_worker_response", fmt.Errorf("Worker 响应缺少沙盒信息"))
	}
	select {
	case <-interrupts:
		if releaseCode := a.runRelease([]string{sandboxName}); releaseCode != 0 {
			return releaseCode
		}
		return 130
	default:
	}
	// The terminal sends Ctrl-C to both processes in its foreground group.
	// Keep this supervisor alive until the child exits, so release always runs.
	status, runErr := a.runFn(binary, []string{binary, "-i"}, os.Environ())
	if runErr != nil {
		a.printError("shell_start_failed", fmt.Sprintf("启动 shell 失败: %v", runErr))
		status = 1
	}
	if releaseCode := a.runRelease([]string{sandboxName}); releaseCode != 0 {
		printFields(a.errOut, outputField{"warning", "沙盒未能自动释放，请执行以下命令"}, outputField{"command", "neubox release " + sandboxName})
		if status == 0 {
			return releaseCode
		}
	}
	return status
}
