// neu-box-config：管理 runtime 角色的配置文件（默认 /etc/neu-box/runtime.env）。
//
// 为什么是第三个二进制：neu-box-runtime（wrapper）的 argv 就是 runc 的 argv，
// 加不了子命令，加了就撞；neu-box-hook 的契约是"从 stdin 读 OCI state"，往里
// 塞配置入口会让两件事互相干扰。所以配置的生成与迁移单独一个程序。
//
// 为什么迁移在部署时跑、不在容器创建路径上跑：runtime 没有常驻进程，能对应
// "启动时"的就是安装/升级那一刻。那时单线程、没有容器在起、失败能当场看见。
// 运行时的读取路径保持只读且宽容（见 internal/config），所以**忘了跑这个命令
// 也不会让容器起不来** —— 只是文件停在旧版本。
//
// 和读取路径相反：这是部署工具，出事就该大声失败、退非 0，让脚本停下来。
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/neusbox/neu_box/runtime/neubox/internal/config"
)

// version 是软件版本，构建时用 -ldflags 注进来（仓库根的 VERSION）。
//
// 注意它和 config.ConfigVersion 是两件事：这个是"二进制多老"，那个是"配置文件
// 什么形状"。迁移只看后者。
var version = "dev"

const usage = `neu-box-config — 生成/迁移 runtime 角色的配置文件

用法:
    neu-box-config init [选项]     生成或迁移配置（幂等，可以反复跑）
    neu-box-config show [--path P] 打印生效值和它们的来源，不改任何东西
    neu-box-config version         打印软件版本和配置 schema 版本

init 选项:
    --path <文件>        默认 /etc/neu-box/runtime.env
    --real-runc <路径>   wrapper 后面真正接的 runtime（安装脚本现场发现）
    --worker-url <URL>   hook 上报的 worker 地址
    --hook <路径>        wrapper 注入 OCI bundle 的 hook 路径
    --hook-phase <名>    createRuntime（默认）或 prestart
    --force              忽略文件里已有的值，整份重写

已存在的键只有在**还等于内置默认值**时才会被上面这些值覆盖；手改过的值不动。
不认识的键、注释、空行一律原样保留。
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "show":
		return runShow(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "neu-box-config %s（config schema %d）\n",
			version, config.ConfigVersion)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令: %s\n\n%s", args[0], usage)
		return 2
	}
}

// parseInitArgs 认 --key=value 和 --key value 两种写法。手写解析：这里只有五个
// 选项，引 flag 包反而多一层"为什么 wrapper 不用它"的解释。
func parseInitArgs(args []string, stderr io.Writer) (config.Options, bool) {
	opts := config.Options{}
	set := func(name, value string) bool {
		switch name {
		case "--path":
			opts.Path = value
		case "--real-runc":
			opts.RealRunc = value
		case "--worker-url":
			opts.WorkerURL = value
		case "--hook":
			opts.HookPath = value
		case "--hook-phase":
			opts.HookPhase = value
		default:
			fmt.Fprintf(stderr, "未知参数: %s\n\n%s", name, usage)
			return false
		}
		return true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--force" {
			opts.Force = true
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		if !inline {
			switch name {
			case "--path", "--real-runc", "--worker-url", "--hook", "--hook-phase":
			default:
				fmt.Fprintf(stderr, "未知参数: %s\n\n%s", arg, usage)
				return opts, false
			}
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "%s 缺值\n\n%s", name, usage)
				return opts, false
			}
			i++
			value = args[i]
		}
		if !set(name, value) {
			return opts, false
		}
	}
	return opts, true
}

func runInit(args []string, stdout, stderr io.Writer) int {
	opts, ok := parseInitArgs(args, stderr)
	if !ok {
		return 2
	}
	result, err := config.Ensure(opts)
	reportInit(stdout, result, err)
	if err != nil {
		fmt.Fprintf(stderr, "neu-box-config: %v\n", err)
		return 1
	}
	// 认不出的行不是失败：迁移已经照做了，只是提醒一声，部署脚本不该因此停下。
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "neu-box-config: 警告：%v\n", warning)
	}
	return 0
}

// reportInit 把 Ensure 干了什么讲清楚：迁移最怕"悄悄啥也没说"，运维下次还是会问
// "到底迁没迁"。
func reportInit(stdout io.Writer, result config.Result, err error) {
	path := result.Path
	if path == "" {
		path = config.DefaultPath
	}
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "%s: 未完成（见下面的错误）\n", path)
	case result.Created:
		fmt.Fprintf(stdout, "%s: 已生成（schema %d）\n", path, result.ToVersion)
	case result.Rewritten:
		fmt.Fprintf(stdout, "%s: 已整份重写（schema %d）\n", path, result.ToVersion)
	case len(result.Updated) == 0:
		fmt.Fprintf(stdout, "%s: 已是最新（schema %d），未改动\n",
			path, result.ToVersion)
	default:
		what := "已补键"
		if result.Migrated {
			what = fmt.Sprintf("已从 schema %d 迁移到 %d",
				result.FromVersion, result.ToVersion)
		}
		fmt.Fprintf(stdout, "%s: %s，改动 %s\n",
			path, what, strings.Join(result.Updated, ", "))
	}
}

func runShow(args []string, stdout, stderr io.Writer) int {
	path := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, inline := strings.Cut(arg, "=")
		if !inline {
			switch name {
			case "--path":
				if i+1 >= len(args) {
					fmt.Fprintf(stderr, "--path 缺值\n\n%s", usage)
					return 2
				}
				i++
				value = args[i]
			case "-h", "--help":
				fmt.Fprint(stdout, usage)
				return 0
			default:
				fmt.Fprintf(stderr, "未知参数: %s\n\n%s", arg, usage)
				return 2
			}
		} else if name != "--path" {
			fmt.Fprintf(stderr, "未知参数: %s\n\n%s", arg, usage)
			return 2
		}
		path = value
	}

	snapshot, err := config.Inspect(path)
	if err != nil {
		fmt.Fprintf(stderr, "neu-box-config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "配置文件: %s\n", snapshot.Path)
	if snapshot.Missing {
		fmt.Fprintf(stdout, "文件版本: 不存在（运行时按内置默认值工作）\n")
	} else {
		fmt.Fprintf(stdout, "文件版本: schema %d（本二进制支持到 %d）\n",
			snapshot.FileVersion, config.ConfigVersion)
	}
	keys := make([]string, 0, len(snapshot.Values))
	for key := range snapshot.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(stdout, "  %-24s = %-38s [%s]\n",
			key, snapshot.Values[key], snapshot.Sources[key])
	}
	return 0
}
