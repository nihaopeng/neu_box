// Package config 读 neu-box runtime 的运行时配置。
//
// 两个二进制共用同一份配置和同一套读取规则：环境变量优先，其次是配置文件
// （默认 /etc/neu-box/runtime.env，可用 NEU_BOX_CONFIG 指到别处），最后是内置
// 默认值。这个"环境变量覆盖文件"的顺序和 Worker 的 load_role_environment
// （src/neu_box/config.py:19，load_dotenv(override=False)）一致。
//
// 配置文件是主要通道：hook 是 runc 拉起来的，继承的是 dockerd 的环境，
// docker run -e 传不进去。环境变量那一层是给本地调试和临时覆盖用的。
//
// 配置读不动（文件缺失、语法错、显式指到的路径不存在）一律不致命：用默认值接着
// 干活，问题作为 warning 返回给调用方打 stderr。在容器创建路径上因为配置文件
// 打不开就拒绝启动，代价比配错了还大。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// 环境变量名 = 配置文件里的键名，两边共用一套名字（项目约定：NEU_BOX_* 前缀）。
const (
	EnvWorkerURL  = "NEU_BOX_WORKER_URL"
	EnvHookPath   = "NEU_BOX_HOOK"
	EnvHookPhase  = "NEU_BOX_HOOK_PHASE"
	EnvRealRunc   = "NEU_BOX_REAL_RUNC"
	EnvCapGuard   = "NEU_BOX_CAP_GUARD"
	EnvConfigPath = "NEU_BOX_CONFIG"
)

// DefaultPath 是 runtime 这个 role 的配置文件位置。
const DefaultPath = "/etc/neu-box/runtime.env"

// 默认值。端口和 Worker 的 NEU_BOX_PORT 默认值相同（deploy/config/worker.env.example），
// 换了端口要两边一起改 —— 契约里点名的"一份事实两处描述"。
//
// DefaultHookPhase 选 createRuntime：它是 OCI 规范里 prestart 的正式替代
// （prestart 已废弃，迟早会被 runc 摘掉），且已直连 runc（`runc run -b`，不经过
// dockerd）验证过行为和 prestart 一致 —— hook 被调用、读得到容器 mnt ns
// （mnt:[4026549739] ≠ hook 自己的 mnt:[4026531841]）和容器 cgroup scope，
// 退非 0 时 payload 不执行。phase 是 runc 自己的行为，Docker/containerd 只负责挑
// runtime 二进制，所以这一层验过就够。
//
// prestart 仍然可用，不是遗留垃圾：整条 Docker 链路（Docker 28.5.2 → containerd
// 1.7.28 → runc 1.3.3）端到端验过的只有它。createRuntime 在完整链路上还没跑过，
// 真机第一次跑就是它 —— 出问题就把 NEU_BOX_HOOK_PHASE 切回 prestart，这是退路。
const (
	DefaultWorkerURL = "http://127.0.0.1:59075"
	DefaultHookPath  = "/usr/local/bin/neu-box-hook"
	DefaultHookPhase = "createRuntime"
	DefaultRealRunc  = "/usr/local/bin/runc"
	// DefaultCapGuard 让 wrapper 剪掉 CAP_AUDIT_READ：容器请求全套能力位
	// （--privileged / --cap-add=ALL）时 Ascend 驱动会把它判成 admin，建出
	// 全量 UDA 设备表，沙盒隔离失效。可选值 drop（默认）/ deny / off，
	// 详见 cmd/neu-runtime/capguard.go 的文件头。
	DefaultCapGuard = "drop"
)

// Config 是两个二进制共用的运行配置。
type Config struct {
	WorkerURL string // neu-box-hook 上报的 Worker 地址
	HookPath  string // neu-box-runtime 写进 OCI hook 记录的 hook 可执行文件
	HookPhase string // 注入到哪个 OCI hook 阶段
	RealRunc  string // wrapper 后面真正接的 runtime
	CapGuard  string // drop（默认）/ deny / off，见 capguard.go
}

// Default 返回全默认值的配置。
func Default() Config {
	return Config{
		WorkerURL: DefaultWorkerURL,
		HookPath:  DefaultHookPath,
		HookPhase: DefaultHookPhase,
		RealRunc:  DefaultRealRunc,
		CapGuard:  DefaultCapGuard,
	}
}

// Load 返回运行配置，永远返回一份能用的东西。
//
// path 为空表示按惯例解析：先看 NEU_BOX_CONFIG，再看 /etc/neu-box/runtime.env。
// 非空时只读这个文件（测试用）。
//
// 返回的 error 是**警告**，不是失败：文件缺失、有认不出来的行、显式指到的路径
// 不存在都从这里出来，调用方打 stderr 即可。环境变量始终优先于文件。
func Load(path string) (Config, error) {
	cfg := Default()

	resolved, err := resolvePath(path)
	var warnings []error
	if err != nil {
		warnings = append(warnings, err)
	} else {
		values, err := readEnvFile(resolved)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				warnings = append(warnings, fmt.Errorf("配置文件 %s 不存在，使用内置默认值", resolved))
			} else {
				warnings = append(warnings, fmt.Errorf("读取 %s 失败（%v），能认的键照用", resolved, err))
			}
		}
		apply(&cfg, values)
	}

	apply(&cfg, map[string]string{
		EnvWorkerURL: os.Getenv(EnvWorkerURL),
		EnvHookPath:  os.Getenv(EnvHookPath),
		EnvHookPhase: os.Getenv(EnvHookPhase),
		EnvRealRunc:  os.Getenv(EnvRealRunc),
		EnvCapGuard:  os.Getenv(EnvCapGuard),
	})
	if !ValidCapGuard(cfg.CapGuard) {
		warnings = append(warnings, fmt.Errorf(
			"%s=%q 不是 drop/deny/off，用默认值 %s",
			EnvCapGuard, cfg.CapGuard, DefaultCapGuard))
		cfg.CapGuard = DefaultCapGuard
	}
	return cfg, errors.Join(warnings...)
}

// ValidCapGuard 判断能力位守卫的模式是否认识。
func ValidCapGuard(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "drop", "deny", "off":
		return true
	}
	return false
}

// resolvePath 决定读哪个文件。显式给了路径（参数或 NEU_BOX_CONFIG）就用它，
// 否则用 role 约定的位置。
func resolvePath(explicit string) (string, error) {
	if explicit == "" {
		explicit = strings.TrimSpace(os.Getenv(EnvConfigPath))
	}
	if explicit == "" {
		return DefaultPath, nil
	}
	if _, err := os.Stat(explicit); err != nil {
		return explicit, fmt.Errorf("%s 指向的配置文件 %s 读不到：%v", EnvConfigPath, explicit, err)
	}
	return explicit, nil
}

// apply 把非空的值盖到 cfg 上。空值不覆盖 —— 与 load_dotenv 的行为一致：
// 没配就是没配，不该把已有值抹成空串。
func apply(cfg *Config, values map[string]string) {
	if v := values[EnvWorkerURL]; v != "" {
		cfg.WorkerURL = v
	}
	if v := values[EnvHookPath]; v != "" {
		cfg.HookPath = v
	}
	if v := values[EnvHookPhase]; v != "" {
		cfg.HookPhase = v
	}
	if v := values[EnvRealRunc]; v != "" {
		cfg.RealRunc = v
	}
	if v := values[EnvCapGuard]; v != "" {
		cfg.CapGuard = strings.ToLower(strings.TrimSpace(v))
	}
}

// readEnvFile 解析 dotenv 形式的配置文件。
//
// 出错时返回的 map 也可能是非空的：能认的行照常返回，认不出来的地方通过 err
// 报出去。文件不存在时 err 包装了 fs.ErrNotExist。
func readEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	var bad []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			bad = append(bad, line)
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if key == "" {
			bad = append(bad, line)
			continue
		}
		values[key] = parseValue(value)
	}
	if len(bad) > 0 {
		return values, fmt.Errorf("不认识的行：%s", strings.Join(bad, " / "))
	}
	return values, nil
}

// parseValue 处理 dotenv 的值：成对引号去掉引号、不带引号的值去掉行尾注释。
//
// 这里的取舍和 worker 那边的 python-dotenv 对齐 —— 两个 role 的 .env 是同一套
// 格式，同一个文件拿给两个解析器读不该读出两种结果。
func parseValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if quote := value[0]; quote == '"' || quote == '\'' {
		// 带引号：配对引号之后的东西全是注释，引号里的原样保留（包括空格）。
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return value[1 : 1+end]
		}
		return value[1:] // 只有开头没结尾，按字面收下剩下的
	}
	// 不带引号：空白后面的 # 起注释。KEY=#x 这种没有空白的按值处理。
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}
