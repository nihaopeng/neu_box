// neu-box-runtime：neu-box 的 runc wrapper。
//
// 它在 /etc/docker/daemon.json 里占 runc 的位置，并且是 **default-runtime**
// （键和值都叫 neu-box-runtime，和二进制同名）：
//
//	{
//	  "default-runtime": "neu-box-runtime",
//	  "runtimes": { "neu-box-runtime": { "path": "/usr/local/bin/neu-box-runtime" } }
//	}
//
// 也就是说这台机器上所有容器（包括跟我们完全无关的业务容器）的启动都从这条路
// 走。行为分两半：
//
//	认识的容器   create 或 run 且 bundle 里 annotations.sandbox_cgroup 非空时
//	             才动 config.json —— 往 hooks[<phase>] 追加一条 neu-box-hook
//	             记录；然后 argv 原样交给真 runc。
//	能力位守卫   所有 create/run 的容器都过（与 annotation 无关）：容器请求了
//	             全套 capabilities（--privileged / --cap-add=ALL）时，Ascend
//	             驱动会把它判成 admin 并给它的 mount namespace 建出全量 UDA
//	             设备表，我们的 eBPF 在那条路径上不在场 —— 隔离会静默失效。
//	             默认（NEU_BOX_CAP_GUARD=drop）从四个能力集合里剪掉
//	             CAP_AUDIT_READ，让它掉出 admin；详见 capguard.go。
//	其余一切     （别的子命令、没 --bundle、config.json 读不了、JSON 坏了、
//	             没有 annotation…）一律原样转发，一个字节都不动。
//
// 这条界线的依据是"我们到底知不知道这是什么容器"：
//
//	看不明白 → 不能因为 wrapper 自己的问题挡住无关容器，原样转发；
//	          如果是沙盒容器，它会在 BPF 那里被拒（fail-closed，设计如此）。
//	看明白了（annotation 就在眼前）→ 答应登记就必须做到，做不到就挡住，
//	          宁可创建失败也不要放一个"起得来但设备全被拒"的容器过去。
//
// 注入到哪个 phase 由 NEU_BOX_HOOK_PHASE 决定，默认 createRuntime。
//
//	✅ createRuntime  直连 runc 验证过（`runc run -b`，不经过 dockerd）：hook 被
//	                  调用、读得到容器 mnt ns（mnt:[4026549739] ≠ hook 自己的
//	                  mnt:[4026531841]）和容器 cgroup scope 及其 inode，hook 退
//	                  非 0 时 payload 不执行。phase 是 runc 自己的行为，
//	                  Docker/containerd 只负责挑 runtime 二进制，这一层验过就够。
//	                  选它当默认还因为 prestart 在 OCI 规范里已废弃，迟早会被
//	                  runc 摘掉。
//	✅ prestart       整条 Docker 链路（Docker 28.5.2 → containerd 1.7.28 →
//	                  runc 1.3.3）端到端验证过：hook 里能读到容器 host PID /
//	                  容器 mnt ns / /system.slice/docker-<id>.scope 及其 inode；
//	                  hook 退非 0 时容器创建失败、payload 不执行。
//
// 这两个都留着，prestart 不是遗留垃圾：**整条 Docker 链路上验过的只有 prestart**，
// createRuntime 在完整链路上还没跑过 —— 真机第一次跑就是它。出问题就把
// NEU_BOX_HOOK_PHASE 切回 prestart，这是唯一的退路。
//
// 没验过的是"整条 Docker 链路上跑 createRuntime"，不是 createRuntime 本身。
//
// 它是 containerd/dockerd 拉起来的，不是交互终端：日志一律走 stderr，不碰 stdout。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/neusbox/neu_box/native/runtime/internal/config"
)

const (
	// annotationKey 是契约规定的 annotation 键，值是沙盒名。
	annotationKey = "sandbox_cgroup"

	// hookTimeoutSeconds 写进 OCI hook 记录的 timeout 字段。
	// 契约规定 hook 自身 10s，其中 Worker HTTP 请求 8s（见 cmd/neu-hook 的
	// httpTimeout）—— HTTP 超时必须严格小于这个值，否则 runc 杀 hook 的时候
	// 连错误都拿不到。
	hookTimeoutSeconds = 10

	// configFileName 是 OCI bundle 里的配置文件名（runtime-spec 规定）。
	configFileName = "config.json"
)

// hookPhases 是 OCI 规范里 hooks 对象仅有的六个键。
//
// 写错 phase 的后果是 hook 永远不被调用：runc 按结构体解析 config.json，未知的
// hook 键静默丢掉。结果是容器起得来、在 BPF 那里被拒 —— 是 fail-closed，但现场
// 极难查（"annotation 也给了，hook 就是不跑"）。所以宁可在 wrapper 这一层就
// 拦下来，别让一个拼错的阶段名悄悄失效。
var hookPhases = map[string]bool{
	"prestart":        true,
	"createRuntime":   true,
	"createContainer": true,
	"startContainer":  true,
	"poststart":       true,
	"poststop":        true,
}

// injectableSubcommands 是会被注入 hook 的子命令。
//
// containerd 只用 create（Docker 路径）；run（= create + start）是给本机直接用
// `runc run -b <bundle>` 验证用的，行为和走 Docker 一致。其余子命令一个字都不改
// 地转发。
var injectableSubcommands = map[string]bool{
	"create": true,
	"run":    true,
}

// runcCommands 是 runc 的子命令名。
//
// 不能简单地取"第一个不以 - 开头的参数"当子命令：containerd 是把全局 flag 的
// 值当独立参数传进来的，实际 argv 形如
//
//	runc --root /run/docker/runtime-runc/moby --log /run/.../log.json \
//	     --log-format json create --bundle /run/.../bundle --pid-file ... <id>
//
// 那样取到的是 --root 的值。按已知子命令名找就没有这个歧义。
var runcCommands = map[string]bool{
	"checkpoint": true,
	"create":     true,
	"delete":     true,
	"events":     true,
	"exec":       true,
	"features":   true,
	"kill":       true,
	"list":       true,
	"pause":      true,
	"ps":         true,
	"restore":    true,
	"resume":     true,
	"run":        true,
	"spec":       true,
	"start":      true,
	"state":      true,
	"update":     true,
}

// hookRecord 是写进 config.json 的 OCI hook 记录（runtime-spec 的 Hook 对象）。
type hookRecord struct {
	Path    string   `json:"path"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
}

func main() {
	cfg, warn := config.Load("")
	if warn != nil {
		logf(os.Stderr, "%v", warn)
	}
	os.Exit(run(os.Args[1:], cfg, os.Stderr))
}

// run 是 main 的全部逻辑，返回进程退出码。
func run(args []string, cfg config.Config, stderr io.Writer) int {
	argv, err := prepare(args, cfg, stderr)
	if err != nil {
		logf(stderr, "拒绝转发：%v", err)
		return 1
	}

	binary, err := exec.LookPath(cfg.RealRunc)
	if err != nil {
		logf(stderr, "找不到真 runtime %q：%v", cfg.RealRunc, err)
		return 1
	}
	if err := guardSelfExec(binary); err != nil {
		logf(stderr, "拒绝转发：%v", err)
		return 1
	}
	// 用 exec 换掉自己，不是 fork 一个子进程：runc 的 stdin/stdout/退出码/信号
	// 都得原样透出去，中间夹一层进程只会把这三样都搞坏。
	if err := syscall.Exec(binary, execArgv(binary, argv), os.Environ()); err != nil {
		logf(stderr, "exec %s 失败：%v", binary, err)
		return 1
	}
	return 0 // 到不了：exec 成功的话这个进程已经是 runc 了
}

// execArgv 拼交给 exec 的 argv：argv[0] 是真 runtime 的路径，后面原样跟着
// 收到的参数。
func execArgv(binary string, args []string) []string {
	return append([]string{binary}, args...)
}

// guardSelfExec 挡住 NEU_BOX_REAL_RUNC 指着 wrapper 自己的情况。
//
// 那样每次 exec 都回到自己，容器创建会一直挂着不返回 —— 在这台机器上是"所有容器
// 都卡住"，比直接失败难查得多。装机脚本写错路径就会撞上这个。
// 查不到自己是谁时放行：不值得因为这个拒绝启动。
func guardSelfExec(binary string) error {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return nil
	}
	binaryInfo, err := os.Stat(binary)
	if err != nil {
		return nil
	}
	if os.SameFile(selfInfo, binaryInfo) {
		return fmt.Errorf("NEU_BOX_REAL_RUNC=%s 指向 wrapper 自己，会无限递归", binary)
	}
	return nil
}

// prepare 检查 runc 的 argv，必要时改写 <bundle>/config.json，返回要转发给真
// runc 的 argv（永远是收到的那一份，逐字）。
//
// 返回 error 只有一种情况：已经确认这是沙盒容器（annotation 就在 config.json
// 里），但注入没做成。这时调用方失败退出 —— 放过去的话容器照常起来、然后在
// BPF 那里被拒，用户看到的是一个没头没尾的"设备不可用"，比创建失败难查得多。
// 其余所有看不懂的输入都走原样转发，不返回 error。
func prepare(args []string, cfg config.Config, stderr io.Writer) ([]string, error) {
	sub := subcommand(args)
	if sub == "exec" {
		// `docker exec` 的能力位不在 bundle 里，而在 containerd 交给
		// `runc exec --process <file>` 的那份 Process JSON 里 —— 容器 init 的 spec
		// 我们在 create 时剪过，但 exec 出来的是 docker 按容器自己的 HostConfig
		// 现算的，所以这条也要单独剪（实测 exec 进程的 CapEff 仍是全量）。
		processPath := execProcessFile(args)
		if processPath == "" {
			// 找不到就剪不了，原样转发（和"看不明白就转发"一个口径）。但要说一声：
			// 这条路径静默失效的后果是 exec 出来的进程可能仍是 admin。
			logf(stderr, "exec 没带 --process，找不到进程的 capabilities，原样转发")
		}
		changed, err := applyCapGuardToExecProcess(
			processPath, cfg.CapGuard,
			func(format string, args ...any) { logf(stderr, format, args...) },
		)
		if err != nil {
			return nil, err
		}
		if changed {
			logf(stderr,
				"已移除 %s：exec 出的进程带着全套能力位（docker 按容器 HostConfig "+
					"现算的），Ascend 驱动会把它判成 admin 并建出全量 UDA 设备表",
				capGuardDropCapability)
		}
		return args, nil
	}
	if !injectableSubcommands[sub] {
		return args, nil // 逐字转发，config.json 碰都不碰
	}

	bundle, ok := bundleFlag(args)
	if !ok || bundle == "" {
		// 没给 --bundle（runc 会当成当前目录）。找不到 bundle 就找不到
		// config.json，判断不了该不该注入。
		logf(stderr, "%s 没带 --bundle，无法定位 config.json，原样转发", sub)
		return args, nil
	}

	configPath := filepath.Join(bundle, configFileName)

	// 能力位守卫：**所有**容器都过这一关，不只是沙盒容器。容器带着全套
	// capabilities（--privileged / --cap-add=ALL）时 Ascend 驱动会把它判成
	// admin，给这个 mount namespace 建出一张全量 UDA 设备表，而我们 worker 的
	// eBPF 在那条路径上不在场 —— 沙盒隔离会静默失效。详见 capguard.go 文件头。
	changed, err := applyCapGuard(configPath, cfg.CapGuard, func(format string, args ...any) {
		logf(stderr, format, args...)
	})
	if err != nil {
		return nil, err
	}
	if changed {
		logf(stderr,
			"已移除 %s：%s 请求了全部能力位（等价 --privileged/--cap-add=ALL），"+
				"Ascend 驱动会把这类容器判成 admin 并建出全量 UDA 设备表，NPU 隔离会失效",
			capGuardDropCapability, configPath)
	}

	annotation, err := scanBundle(configPath)
	if err != nil {
		// 读不了 / 不是合法 JSON：同样判断不了。这台机器上所有容器都从这条路
		// 走，wrapper 自己的问题不能挡住别人。
		logf(stderr, "%v，不注入 hook，原样转发", err)
		return args, nil
	}
	if annotation == "" {
		// 契约规定的 fail-closed 路径：容器照常起来，然后在 BPF 那里被拒。
		logf(stderr, "%s 里没有 %s annotation，不注入 hook，原样转发", configPath, annotationKey)
		return args, nil
	}

	// 到这儿 annotation 已经在手里了，这是我们的容器，只能成功不能将就。
	if !hookPhases[cfg.HookPhase] {
		return nil, fmt.Errorf("NEU_BOX_HOOK_PHASE=%q 不是 OCI 的 hook 阶段（可选：prestart/createRuntime/createContainer/startContainer/poststart/poststop）", cfg.HookPhase)
	}
	// 这里会把 config.json 再读一遍：多读一个 2KB 的文件换两个函数各自独立、
	// 各自可测，划算。
	injected, err := injectHook(configPath, cfg.HookPhase, cfg.HookPath)
	if err != nil {
		return nil, err
	}
	if injected {
		logf(stderr, "已给沙盒 %s 注入 %s hook（path=%s, timeout=%ds）：%s",
			annotation, cfg.HookPhase, cfg.HookPath, hookTimeoutSeconds, configPath)
	} else {
		logf(stderr, "沙盒 %s 的 %s hook 已经在 config.json 里，不重复加：%s",
			annotation, cfg.HookPhase, configPath)
	}
	return args, nil
}

// scanBundle 读 <bundle>/config.json，返回 annotations.sandbox_cgroup。
//
// 返回 error 表示"这个 bundle 看不明白"（文件读不了、不是合法 JSON、
// annotations 类型不对），调用方据此原样转发。没有 annotation 是正常的
// ("" , nil) —— 大多数容器都是这种。
func scanBundle(configPath string) (string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("读 %s 失败（%v）", configPath, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", fmt.Errorf("%s 不是合法 JSON（%v）", configPath, err)
	}
	rawAnnotations, ok := top["annotations"]
	if !ok {
		return "", nil
	}
	var annotations map[string]string
	if err := json.Unmarshal(rawAnnotations, &annotations); err != nil {
		return "", fmt.Errorf("%s 的 annotations 不是字符串映射（%v）", configPath, err)
	}
	return annotations[annotationKey], nil
}

// subcommand 找出 argv 里的 runc 子命令，找不到返回空串。
func subcommand(args []string) string {
	for _, arg := range args {
		if runcCommands[arg] {
			return arg
		}
	}
	return ""
}

// bundleFlag 取 bundle 的值。三种写法都认：--bundle DIR、--bundle=DIR、
// 以及 runc 自己的短别名 -b DIR。
//
// -b 不在契约里（契约只写了 --bundle 两种），加它是因为契约支持 run 的理由是
// "本机直接用 runc run 测" —— 而 `runc run` 的现成写法就是 `runc run -b <dir>`，
// 不认 -b 的话这条路等于没通。runc 里 -b 只出现在 create/run 上，不会认错。
func bundleFlag(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "--bundle" || arg == "-b" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if value, ok := strings.CutPrefix(arg, "--bundle="); ok {
			return value, true
		}
	}
	return "", false
}

// injectHook 读 config.json，annotations.sandbox_cgroup 非空时往 hooks[phase]
// 追加一条本 hook 的记录，返回是否改动了文件。
//
// 三种情况返回 (false, nil)，都不碰文件：没有 annotation、hook 已经在同一个
// phase 里了、以及 hooks 里本来就没有这个 phase。
func injectHook(configPath, phase, hookPath string) (bool, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false, fmt.Errorf("读 %s：%w", configPath, err)
	}

	// 顶层用 RawMessage 过一遍：只动 annotations/hooks 两块，其余字段（root、
	// process、linux、mounts…）原字节保留，不会被解析再序列化搞坏。
	// 代价是顶层键顺序会变成字典序 —— runc 按 JSON 解析，无所谓。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false, fmt.Errorf("解析 %s：%w", configPath, err)
	}

	annotations := map[string]string{}
	if rawAnnotations, ok := top["annotations"]; ok {
		if err := json.Unmarshal(rawAnnotations, &annotations); err != nil {
			return false, fmt.Errorf("解析 %s 的 annotations：%w", configPath, err)
		}
	}
	if annotations[annotationKey] == "" {
		return false, nil
	}

	// hooks 也逐 phase 用 RawMessage 保住：别的 phase（本机的 Ascend Docker
	// Runtime 就挂在 prestart 上）和同一个 phase 里别人的 hook 都不能动。
	hooks := map[string]json.RawMessage{}
	if rawHooks, ok := top["hooks"]; ok {
		if err := json.Unmarshal(rawHooks, &hooks); err != nil {
			return false, fmt.Errorf("解析 %s 的 hooks：%w", configPath, err)
		}
	}

	var phaseHooks []json.RawMessage
	if rawPhase, ok := hooks[phase]; ok {
		if err := json.Unmarshal(rawPhase, &phaseHooks); err != nil {
			return false, fmt.Errorf("解析 %s 的 hooks.%s：%w", configPath, phase, err)
		}
		for _, item := range phaseHooks {
			var existing struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(item, &existing) == nil && existing.Path == hookPath {
				return false, nil // 已经在里面了，不重复加
			}
		}
	}

	record, err := json.Marshal(hookRecord{
		Path: hookPath,
		// args[0] 按惯例是 hook 自己的名字，runc 拿它当 argv[0]。
		// hook 从 stdin 读 OCI state、不解析 argv，这里只是让现场好看。
		Args:    []string{filepath.Base(hookPath)},
		Timeout: hookTimeoutSeconds,
	})
	if err != nil {
		return false, fmt.Errorf("序列化 hook 记录：%w", err)
	}
	phaseHooks = append(phaseHooks, record)

	encodedPhase, err := json.Marshal(phaseHooks)
	if err != nil {
		return false, fmt.Errorf("序列化 hooks.%s：%w", phase, err)
	}
	hooks[phase] = encodedPhase

	encodedHooks, err := json.Marshal(hooks)
	if err != nil {
		return false, fmt.Errorf("序列化 hooks：%w", err)
	}
	top["hooks"] = encodedHooks

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return false, fmt.Errorf("序列化 %s：%w", configPath, err)
	}
	out = append(out, '\n')
	if err := writeFileAtomic(configPath, out); err != nil {
		return false, fmt.Errorf("写 %s：%w", configPath, err)
	}
	return true, nil
}

// writeFileAtomic 先写同目录的临时文件再 rename：runc 紧接着就要读这个文件，
// 半截的 config.json 会让它报一个和真实原因八竿子打不着的语法错。
// 权限沿用原文件的。
func writeFileAtomic(path string, data []byte) error {
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+configFileName+".neu-box-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName) // rename 成功后这里什么也不做

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(perm); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// logf 写 stderr。容器创建路径上的日志都从这里走，绝不碰 stdout。
func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "neu-box-runtime: "+format+"\n", args...)
}
