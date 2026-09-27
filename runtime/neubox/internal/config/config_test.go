package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleanEnv 清掉这台机器上可能存在的同名环境变量，免得测试结果跟着机器变。
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{EnvWorkerURL, EnvHookPath, EnvHookPhase, EnvRealRunc, EnvConfigPath} {
		t.Setenv(key, "")
	}
}

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("写配置文件：%v", err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	got := Default()
	want := Config{
		WorkerURL: "http://127.0.0.1:59075",
		HookPath:  "/usr/local/bin/neu-box-hook",
		HookPhase: "createRuntime",
		RealRunc:  "/usr/local/bin/runc",
		CapGuard:  "drop",
	}
	if got != want {
		t.Fatalf("默认值 = %+v，想要 %+v", got, want)
	}
	// 默认值改过一次（prestart → createRuntime）。写死在这里是为了让它改不动：
	// 换 phase 之前得先有真机验证，不能顺手改。
	if DefaultHookPhase != "createRuntime" {
		t.Fatalf("默认 phase = %q：createRuntime 是直连 runc 验过才当默认值的，别乱改", DefaultHookPhase)
	}
	// 默认必须是 drop：容器请求全套能力位时驱动会把它判成 admin，隔离会失效。
	if DefaultCapGuard != "drop" {
		t.Fatalf("默认 cap guard = %q，必须是 drop（理由见 cmd/neu-runtime/capguard.go）", DefaultCapGuard)
	}
}

func TestLoadReadsFile(t *testing.T) {
	cleanEnv(t)
	path := writeEnvFile(t, `
# Neu Box Runtime configuration
NEU_BOX_WORKER_URL=http://10.0.0.1:1234
NEU_BOX_HOOK=/opt/neu-box/neu-box-hook
NEU_BOX_HOOK_PHASE=createRuntime
NEU_BOX_REAL_RUNC=/opt/runc
`)
	cfg, warn := Load(path)
	if warn != nil {
		t.Fatalf("不该有警告：%v", warn)
	}
	if cfg.WorkerURL != "http://10.0.0.1:1234" || cfg.HookPath != "/opt/neu-box/neu-box-hook" ||
		cfg.HookPhase != "createRuntime" || cfg.RealRunc != "/opt/runc" {
		t.Fatalf("配置文件没生效：%+v", cfg)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	cleanEnv(t)
	path := writeEnvFile(t, "NEU_BOX_WORKER_URL=http://from-file:1\nNEU_BOX_HOOK_PHASE=createRuntime\n")
	// 顺序和 Worker 的 load_dotenv(override=False) 一致：环境变量赢。
	t.Setenv(EnvWorkerURL, "http://from-env:2")

	cfg, warn := Load(path)
	if warn != nil {
		t.Fatalf("不该有警告：%v", warn)
	}
	if cfg.WorkerURL != "http://from-env:2" {
		t.Fatalf("环境变量该覆盖文件：%q", cfg.WorkerURL)
	}
	if cfg.HookPhase != "createRuntime" {
		t.Fatalf("没被环境变量覆盖的键该保留文件里的值：%q", cfg.HookPhase)
	}
}

func TestLoadMissingFileUsesDefaultsAndWarns(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "nope.env")
	cfg, warn := Load(path)
	if warn == nil {
		t.Fatal("文件不存在必须给个警告（调用方要打 stderr）")
	}
	if !strings.Contains(warn.Error(), path) {
		t.Fatalf("警告里该带上路径：%v", warn)
	}
	if cfg != Default() {
		t.Fatalf("该退回默认值：%+v", cfg)
	}
	// 警告不是错误：真值仍然能用。
	if cfg.RealRunc == "" {
		t.Fatal("配置不可用")
	}
}

func TestLoadEmptyFileIsFine(t *testing.T) {
	cleanEnv(t)
	cfg, warn := Load(writeEnvFile(t, ""))
	if warn != nil {
		t.Fatalf("空文件是合法的：%v", warn)
	}
	if cfg != Default() {
		t.Fatalf("空文件该出默认值：%+v", cfg)
	}
}

func TestLoadSyntax(t *testing.T) {
	cleanEnv(t)
	path := writeEnvFile(t, `
# 注释
   # 缩进的注释

export NEU_BOX_WORKER_URL=http://exported:1
NEU_BOX_HOOK=" /usr/local/bin/hook "   # 引号后面是注释
NEU_BOX_REAL_RUNC='/opt/runc'
坏行没有等号
`)
	cfg, warn := Load(path)
	if warn == nil {
		t.Fatal("认不出来的行该有警告")
	}
	if !strings.Contains(warn.Error(), "坏行没有等号") {
		t.Fatalf("警告里该带上那一行：%v", warn)
	}
	// 坏行不影响别的键。
	if cfg.WorkerURL != "http://exported:1" {
		t.Fatalf("export 前缀该被接受：%q", cfg.WorkerURL)
	}
	if cfg.HookPath != " /usr/local/bin/hook " {
		t.Fatalf("成对引号该被去掉（引号里的空格保留）：%q", cfg.HookPath)
	}
	if cfg.RealRunc != "/opt/runc" {
		t.Fatalf("单引号该被去掉：%q", cfg.RealRunc)
	}
}

func TestLoadEmptyValueDoesNotOverride(t *testing.T) {
	cleanEnv(t)
	cfg, _ := Load(writeEnvFile(t, "NEU_BOX_HOOK_PHASE=\nNEU_BOX_REAL_RUNC=/opt/runc\n"))
	if cfg.HookPhase != DefaultHookPhase {
		t.Fatalf("空值不该把默认值抹掉：%q", cfg.HookPhase)
	}
	if cfg.RealRunc != "/opt/runc" {
		t.Fatalf("同一文件里的其它键该生效：%q", cfg.RealRunc)
	}
}

func TestLoadUsesNEU_BOX_CONFIG(t *testing.T) {
	cleanEnv(t)
	path := writeEnvFile(t, "NEU_BOX_WORKER_URL=http://via-neu-box-config:1\n")
	t.Setenv(EnvConfigPath, path)

	cfg, warn := Load("")
	if warn != nil {
		t.Fatalf("不该有警告：%v", warn)
	}
	if cfg.WorkerURL != "http://via-neu-box-config:1" {
		t.Fatalf("NEU_BOX_CONFIG 指向的文件没被读：%+v", cfg)
	}
}

func TestLoadBrokenNEU_BOX_CONFIGWarnsWithoutFallback(t *testing.T) {
	cleanEnv(t)
	missing := filepath.Join(t.TempDir(), "absent.env")
	t.Setenv(EnvConfigPath, missing)

	cfg, warn := Load("")
	if warn == nil {
		t.Fatal("显式指到一个不存在的文件必须给警告")
	}
	if !strings.Contains(warn.Error(), EnvConfigPath) {
		t.Fatalf("警告里该点名 NEU_BOX_CONFIG：%v", warn)
	}
	// 不偷偷退回 /etc/neu-box/runtime.env：显式指定就该显式失败。
	if cfg != Default() {
		t.Fatalf("该用默认值：%+v", cfg)
	}
}

func TestLoadExplicitPathWinsOverEnv(t *testing.T) {
	cleanEnv(t)
	t.Setenv(EnvConfigPath, writeEnvFile(t, "NEU_BOX_WORKER_URL=http://from-env-config:1\n"))
	explicit := writeEnvFile(t, "NEU_BOX_WORKER_URL=http://explicit:1\n")

	cfg, warn := Load(explicit)
	if warn != nil {
		t.Fatalf("不该有警告：%v", warn)
	}
	if cfg.WorkerURL != "http://explicit:1" {
		t.Fatalf("显式路径参数该赢：%q", cfg.WorkerURL)
	}
}
