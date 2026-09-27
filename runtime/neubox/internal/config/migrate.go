package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ConfigVersion 是配置文件 schema 的版本。
//
// 它和二进制版本（仓库根的 VERSION / RPM 的 Version）刻意分开：二进制会因为跟
// 配置无关的原因往前走（修 bug、换默认 phase），而迁移只关心"这份文件现在是
// 什么形状"。worker 的 DB 用 schema_version 记同一件事，形状照它抄。
//
// 只增不减。文件里没有这个键 = 版本 0，也就是本键引入之前的形状。
const ConfigVersion = 1

// EnvVersion 是文件里记 schema 版本的那个键。
const EnvVersion = "NEU_BOX_CONFIG_VERSION"

// migrations 是版本梯子：migrations[i] 把版本 i 的文件提到 i+1。
//
// 现在是空的 —— v1 的定义就是"文件里开始记版本号"，没有别的形状变化，那一步
// 由 setKeys 补键完成。将来要做改名、拆键、改语义这类**宽容读取器推不出来**的
// 变更时，按顺序往这里追加，不要动已有的项。
var migrations = []func([]string) ([]string, bool){}

// fileKeys 是生成新文件时的键顺序，也是补缺失键时的顺序。
var fileKeys = []string{
	EnvVersion, EnvWorkerURL, EnvHookPath, EnvHookPhase, EnvRealRunc,
}

// fileHeader 是新文件的开头。只在新生成时写；已存在的文件迁移时不碰它的头，
// 免得把运维自己写的注释冲掉。
const fileHeader = `# Neu Box Runtime configuration（由 neu-box-config 生成/迁移）
#
# runtime 角色（neu-box-runtime + neu-box-hook）共用的配置。环境变量 NEU_BOX_*
# 优先于本文件，NEU_BOX_CONFIG 可以把读取整个指到别的文件。
#
# 运行时的读取是宽容的：缺键用内置默认值，认不出的行只打 warning。所以手改坏
# 了也不会挡住容器 —— 但迁移只认下面这些键，自己加的键会被原样保留、不参与迁移。
`

// builtinDefault 返回某个键的内置默认值；不认识的键返回空串。
func builtinDefault(key string) string {
	switch key {
	case EnvWorkerURL:
		return DefaultWorkerURL
	case EnvHookPath:
		return DefaultHookPath
	case EnvHookPhase:
		return DefaultHookPhase
	case EnvRealRunc:
		return DefaultRealRunc
	}
	return ""
}

// Options 是 Ensure 的输入。四个值对应安装脚本现场发现的事实：真 runtime 的
// 路径、worker 的地址、hook 的安装路径、hook 注入的 phase。留空表示"这个值我
// 没有发现"，那就用内置默认值。
type Options struct {
	Path      string // 空 = DefaultPath
	WorkerURL string
	HookPath  string
	HookPhase string
	RealRunc  string
	Force     bool // 忽略文件里已有的值，按内置默认值 + 上面的值整份重写
}

// Result 描述 Ensure 干了什么，给部署脚本打印。
type Result struct {
	Path        string
	Created     bool // 文件原本不存在，这次生成了
	Rewritten   bool // --force：整份重写
	Migrated    bool // 跑了版本梯子
	Updated     []string
	Warnings    []error // 认不出的行之类；不是失败，调用方打出来接着走
	FromVersion int
	ToVersion   int
}

// Ensure 让配置文件存在、且处于当前 schema 版本。
//
// 三条规则，从强到弱：
//
//  1. 文件不存在（或给了 Force）→ 按内置默认值 + Options 里的现场事实整份生成。
//  2. 已存在 → 跑版本梯子；补上缺失的键；**只改那些还等于内置默认值的键**。
//     这最后一条是给"包里的模板落了默认值、但本机事实不是默认值"那个老问题
//     用的（模板写死 /usr/local/bin/runc，本机 runc 不在那儿）。运维手改过的
//     值一律不动 —— 要整份重写就显式给 Force。
//  3. 不认识的键、注释、空行原样保留。这份文件大半是解释性注释，重排等于删。
func Ensure(opts Options) (Result, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		path = DefaultPath
	}
	res := Result{Path: path, ToVersion: ConfigVersion}

	raw, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	res.Created = !exists

	want := map[string]string{
		EnvWorkerURL: strings.TrimSpace(opts.WorkerURL),
		EnvHookPath:  strings.TrimSpace(opts.HookPath),
		EnvHookPhase: strings.TrimSpace(opts.HookPhase),
		EnvRealRunc:  strings.TrimSpace(opts.RealRunc),
	}

	if !exists || opts.Force {
		res.Rewritten = exists
		updates := map[string]string{EnvVersion: strconv.Itoa(ConfigVersion)}
		for _, key := range fileKeys {
			if key == EnvVersion {
				continue // 上面已经定了，fileKeys 里只是排在开头
			}
			value := want[key]
			if value == "" {
				value = builtinDefault(key)
			}
			updates[key] = value
		}
		lines, changed := setKeys(nil, updates)
		res.Updated = changed
		// 新生成 / --force 整份重写时带上文件头；迁移已存在的文件时不动它的头，
		// 免得把运维自己写的注释冲掉（见 fileHeader 的注释）。
		if err := writeFile(path, renderNewFile(lines)); err != nil {
			return res, err
		}
		return res, nil
	}

	lines := strings.Split(string(raw), "\n")
	values, warnings := parseValues(lines)
	from, err := parseVersion(values)
	if err != nil {
		return res, err
	}
	res.FromVersion = from

	lines, migrated, err := migrateLines(lines, from)
	if err != nil {
		return res, err
	}
	res.Migrated = migrated

	updates := map[string]string{EnvVersion: strconv.Itoa(ConfigVersion)}
	for _, key := range fileKeys {
		if key == EnvVersion {
			continue
		}
		value := want[key]
		if value == "" {
			continue // 没发现这个事实就不动它
		}
		switch current := values[key]; {
		case current == "":
			updates[key] = value // 缺键：补
		case current == builtinDefault(key) && value != current:
			updates[key] = value // 还是模板默认值，而本机事实不同：修
		}
	}

	lines, changed := setKeys(lines, updates)
	res.Updated = changed
	if len(changed) > 0 {
		if err := writeFile(path, render(lines)); err != nil {
			return res, err
		}
	}
	// 认不出的行只是 warning，不是失败：迁移照做，问题交回给调用方打印。
	// 把它当 error 返回会让部署脚本因为一行写得怪就整个停下来。
	res.Warnings = warnings
	return res, nil
}

// migrateLines 把文件按梯子从 from 提到 ConfigVersion。
func migrateLines(lines []string, from int) ([]string, bool, error) {
	if from > ConfigVersion {
		return nil, false, fmt.Errorf(
			"%s 是 %d，比本二进制认识的 %d 还新；先升级 neu-box-runtime 再跑",
			EnvVersion, from, ConfigVersion)
	}
	changed := false
	for version := from; version < ConfigVersion; version++ {
		if version >= len(migrations) || migrations[version] == nil {
			continue
		}
		next, stepChanged := migrations[version](lines)
		lines = next
		changed = changed || stepChanged
	}
	return lines, changed, nil
}

// parseValues 按 readEnvFile 同一套规则把行解析成键值，但**不报死错**：认不出
// 的行只作为 warning 返回。迁移是部署动作，也不该因为一行写得怪就停。
func parseValues(lines []string) (map[string]string, []error) {
	values := map[string]string{}
	var bad []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, rest, ok := strings.Cut(trimmed, "=")
		if !ok {
			bad = append(bad, trimmed)
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if key == "" {
			bad = append(bad, trimmed)
			continue
		}
		values[key] = parseValue(rest)
	}
	if len(bad) > 0 {
		return values, []error{fmt.Errorf("不认识的行：%s", strings.Join(bad, " / "))}
	}
	return values, nil
}

// parseVersion 取出 schema 版本。没有这个键就是版本 0。
func parseVersion(values map[string]string) (int, error) {
	raw := strings.TrimSpace(values[EnvVersion])
	if raw == "" {
		return 0, nil
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version < 0 {
		return 0, fmt.Errorf("%s 不是非负整数：%q", EnvVersion, raw)
	}
	return version, nil
}

// assignment 判断一行是不是 KEY=VALUE，是就返回键。
func assignment(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", false
	}
	key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
	if key == "" {
		return "", false
	}
	return key, true
}

// setKeys 按行改写：updates 里的键就地替换成 KEY=value，缺的键按 fileKeys 的
// 顺序补在末尾。其余行（注释、空行、不认识的键）一个字节都不动。
func setKeys(lines []string, updates map[string]string) ([]string, []string) {
	out := make([]string, 0, len(lines)+len(fileKeys))
	seen := map[string]bool{}
	changed := map[string]bool{}
	for _, line := range lines {
		key, ok := assignment(line)
		if !ok {
			out = append(out, line)
			continue
		}
		value, want := updates[key]
		if !want {
			out = append(out, line)
			continue
		}
		seen[key] = true
		replacement := key + "=" + value
		if line != replacement {
			changed[key] = true
		}
		out = append(out, replacement)
	}
	for _, key := range fileKeys {
		value, want := updates[key]
		if !want || seen[key] {
			continue
		}
		out = append(out, key+"="+value)
		changed[key] = true
	}
	return out, sortedKeys(changed)
}

// sortedKeys 让 Result.Updated 的顺序稳定，脚本和测试才好断。
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// render 把行拼回文件正文：行之间用 \n，末尾留一个换行。
func render(lines []string) []byte {
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if body == "" {
		return nil
	}
	return []byte(body + "\n")
}

// renderNewFile 在新生成 / --force 整份重写时用：文件头 + 正文。已存在的文件迁移
// 时走 render，不碰它自己的头。
func renderNewFile(lines []string) []byte {
	return append([]byte(fileHeader), render(lines)...)
}

// writeFile 原子落盘：同目录临时文件 + rename。不原子的话，wrapper 可能读到半
// 截文件 —— 它是每个容器创建都要跑一次的短命进程。
//
// 权限按包里的约定 0640。WriteFile/CreateTemp 的 mode 受 umask 影响，所以显式
// Chmod 一次。
func writeFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("建目录 %s 失败: %w", dir, err)
	}
	handle, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("在 %s 建临时文件失败: %w", dir, err)
	}
	tmp := handle.Name()
	defer os.Remove(tmp) // rename 成功之后这里是空操作

	if _, err := handle.Write(content); err != nil {
		handle.Close()
		return fmt.Errorf("写 %s 失败: %w", tmp, err)
	}
	if err := handle.Chmod(0o640); err != nil {
		handle.Close()
		return fmt.Errorf("改 %s 权限失败: %w", tmp, err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("关 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("把 %s 换到 %s 失败: %w", tmp, path, err)
	}
	return nil
}

// Snapshot 是一份只读的配置快照，用于排障。
type Snapshot struct {
	Path        string
	FileVersion int
	Missing     bool
	Values      map[string]string
	Sources     map[string]string // file / env / default
}

// Inspect 读一份配置快照，不改任何东西。生效值的算法和 Load 一致：默认值打底，
// 文件覆盖，环境变量最后覆盖。
func Inspect(path string) (Snapshot, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	snapshot := Snapshot{
		Path:    path,
		Values:  map[string]string{},
		Sources: map[string]string{},
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		snapshot.Missing = true
	case err != nil:
		return snapshot, fmt.Errorf("读取 %s 失败: %w", path, err)
	default:
		values, _ := parseValues(strings.Split(string(raw), "\n"))
		version, verr := parseVersion(values)
		if verr != nil {
			return snapshot, verr
		}
		snapshot.FileVersion = version
		for key, value := range values {
			if value == "" {
				continue
			}
			snapshot.Values[key] = value
			snapshot.Sources[key] = "file"
		}
	}
	for _, key := range fileKeys {
		if key == EnvVersion {
			continue
		}
		if env := strings.TrimSpace(os.Getenv(key)); env != "" {
			snapshot.Values[key] = env
			snapshot.Sources[key] = "env"
			continue
		}
		if _, ok := snapshot.Values[key]; !ok {
			snapshot.Values[key] = builtinDefault(key)
			snapshot.Sources[key] = "default"
		}
	}
	return snapshot, nil
}
