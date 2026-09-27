package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neusbox/neu_box/runtime/neubox/internal/config"
)

// runCLI 跑一次命令，返回退出码和两个输出。所有用例都显式带 --path，绝不碰这台
// 机器真正的 /etc/neu-box/runtime.env。
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	for _, key := range []string{
		config.EnvWorkerURL, config.EnvHookPath, config.EnvHookPhase,
		config.EnvRealRunc, config.EnvConfigPath,
	} {
		t.Setenv(key, "")
	}
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "runtime.env")
}

func TestInitCreatesFileAndSaysSo(t *testing.T) {
	path := tempPath(t)
	code, stdout, stderr := runCLI(t,
		"init", "--path", path, "--real-runc", "/opt/runc/bin/runc")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "已生成") {
		t.Fatalf("stdout 没说明生成了什么：%s", stdout)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件：%v", err)
	}
	if !strings.Contains(string(raw), config.EnvRealRunc+"=/opt/runc/bin/runc") {
		t.Fatalf("--real-runc 没写进去：\n%s", raw)
	}
}

// 第二种写法（--key=value）也要认，部署脚本里两种都可能出现。
func TestInitAcceptsInlineValues(t *testing.T) {
	path := tempPath(t)
	code, _, stderr := runCLI(t,
		"init", "--path="+path, "--worker-url=http://127.0.0.1:61234")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), config.EnvWorkerURL+"=http://127.0.0.1:61234") {
		t.Fatalf("--worker-url 没写进去：\n%s", raw)
	}
}

// 幂等：部署脚本升级时会重复调用，第二次不该是一场空响。
func TestInitIsIdempotentAndSaysSo(t *testing.T) {
	path := tempPath(t)
	if code, _, stderr := runCLI(t, "init", "--path", path); code != 0 {
		t.Fatalf("第一次退出码 = %d，stderr=%s", code, stderr)
	}
	code, stdout, stderr := runCLI(t, "init", "--path", path)
	if code != 0 {
		t.Fatalf("第二次退出码 = %d，stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "未改动") {
		t.Fatalf("第二次应当报未改动：%s", stdout)
	}
}

func TestInitRefusesNewerSchemaWithNonZeroExit(t *testing.T) {
	path := tempPath(t)
	if err := os.WriteFile(path, []byte(config.EnvVersion+"=99\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	code, _, stderr := runCLI(t, "init", "--path", path)
	if code == 0 {
		t.Fatal("schema 比本二进制新的时候必须退非零")
	}
	if !strings.Contains(stderr, "neu-box-config:") {
		t.Fatalf("错误没打到 stderr：%s", stderr)
	}
}

func TestShowPrintsValuesAndSources(t *testing.T) {
	path := tempPath(t)
	if code, _, stderr := runCLI(t,
		"init", "--path", path, "--real-runc", "/opt/runc/bin/runc"); code != 0 {
		t.Fatalf("准备配置失败：%s", stderr)
	}
	code, stdout, stderr := runCLI(t, "show", "--path", path)
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"schema 1", config.EnvRealRunc, "/opt/runc/bin/runc", "[file]",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("show 输出里没有 %q：\n%s", want, stdout)
		}
	}
}

// show 在文件不存在时不是错误：运行时的读取本来就按默认值工作。
func TestShowToleratesMissingFile(t *testing.T) {
	code, stdout, stderr := runCLI(t, "show", "--path", filepath.Join(t.TempDir(), "nope"))
	if code != 0 {
		t.Fatalf("文件不存在不该退非零：%d %s", code, stderr)
	}
	if !strings.Contains(stdout, "不存在") {
		t.Fatalf("应当说明文件不存在：%s", stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"没有子命令", nil},
		{"未知子命令", []string{"frobnicate"}},
		{"init 的未知参数", []string{"init", "--bogus"}},
		{"init 缺值", []string{"init", "--path"}},
		{"show 的未知参数", []string{"show", "--bogus=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			if code != 2 {
				t.Fatalf("退出码 = %d，想要 2（stderr=%s）", code, stderr)
			}
			if stderr == "" {
				t.Fatal("用法错误要打 stderr")
			}
		})
	}
}

// 认不出的行只该警告，不该让部署脚本（install.sh 里那句 || die）整个停下来：
// 退出码 0 + stderr 有警告，迁移本身照做。
func TestInitWarnsButExitsZero(t *testing.T) {
	path := tempPath(t)
	if err := os.WriteFile(path, []byte("这行没有等号\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	code, _, stderr := runCLI(t, "init", "--path", path)
	if code != 0 {
		t.Fatalf("认不出的行不该退非零：%d，stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "警告") || !strings.Contains(stderr, "不认识的行") {
		t.Fatalf("stderr 没给出警告：%s", stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件：%v", err)
	}
	if !strings.Contains(string(raw), config.EnvVersion+"=1") {
		t.Fatalf("警告不该挡住迁移：\n%s", raw)
	}
}

func TestVersionPrintsBothVersions(t *testing.T) {
	code, stdout, stderr := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	// 软件版本和配置 schema 版本是两件事，输出里都要有，否则排障时还是分不清
	// "该不该迁移"。
	if !strings.Contains(stdout, "config schema") || !strings.Contains(stdout, version) {
		t.Fatalf("version 输出不完整：%s", stdout)
	}
}
