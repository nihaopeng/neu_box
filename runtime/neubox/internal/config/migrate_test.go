package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s：%v", path, err)
	}
	return string(raw)
}

func TestEnsureCreatesFileFromDefaultsAndFacts(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")

	result, err := Ensure(Options{
		Path:      path,
		WorkerURL: "http://127.0.0.1:61234",
		RealRunc:  "/opt/runc/bin/runc",
	})
	if err != nil {
		t.Fatalf("Ensure：%v", err)
	}
	if !result.Created || result.Rewritten {
		t.Fatalf("应报「已生成」：%+v", result)
	}
	content := readFile(t, path)
	// 新生成的文件要带上文件头（否则 render 里的 fileHeader 是死代码）。
	if !strings.HasPrefix(content, "# Neu Box Runtime configuration") {
		t.Fatalf("新生成的文件没有文件头：\n%s", content)
	}
	for _, want := range []string{
		EnvVersion + "=1",
		EnvWorkerURL + "=http://127.0.0.1:61234",
		EnvHookPath + "=/usr/local/bin/neu-box-hook",
		EnvHookPhase + "=createRuntime",
		EnvRealRunc + "=/opt/runc/bin/runc",
	} {
		if !strings.Contains(content, want+"\n") {
			t.Fatalf("生成的文件里没有 %q：\n%s", want, content)
		}
	}
	// 版本键不能是空的（实现里 fileKeys 含 EnvVersion，曾经被补键循环覆盖成空）。
	if strings.Contains(content, EnvVersion+"=\n") {
		t.Fatalf("版本键是空的：\n%s", content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat：%v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o640 {
		t.Fatalf("权限 = %o，想要 640", mode)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	opts := Options{Path: path, RealRunc: "/opt/runc/bin/runc"}

	if _, err := Ensure(opts); err != nil {
		t.Fatalf("第一次 Ensure：%v", err)
	}
	first := readFile(t, path)

	result, err := Ensure(opts)
	if err != nil {
		t.Fatalf("第二次 Ensure：%v", err)
	}
	if len(result.Updated) != 0 || result.Migrated || result.Created {
		t.Fatalf("第二次不该有任何改动：%+v", result)
	}
	if second := readFile(t, path); second != first {
		t.Fatalf("文件被改动了：\n第一次:\n%s\n第二次:\n%s", first, second)
	}
}

// 包里模板落下来的形状：没有版本键，四个键都是内置默认值。迁移要补版本键，并且
// 把"还是默认值"的那两个键换成现场发现的事实。
func TestEnsureMigratesLegacyTemplate(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	legacy := "# 运维自己的注释，不许丢\n" +
		EnvWorkerURL + "=" + DefaultWorkerURL + "\n" +
		EnvHookPath + "=" + DefaultHookPath + "\n" +
		EnvHookPhase + "=" + DefaultHookPhase + "\n" +
		EnvRealRunc + "=" + DefaultRealRunc + "\n" +
		"NEU_BOX_自己加的=原样保留\n"
	if err := os.WriteFile(path, []byte(legacy), 0o640); err != nil {
		t.Fatalf("写旧文件：%v", err)
	}

	result, err := Ensure(Options{
		Path:      path,
		WorkerURL: "http://127.0.0.1:61234",
		RealRunc:  "/opt/runc/bin/runc",
	})
	if err != nil {
		t.Fatalf("Ensure：%v", err)
	}
	if result.Created || result.Rewritten {
		t.Fatalf("已存在的文件不该报「已生成/重写」：%+v", result)
	}
	content := readFile(t, path)
	// 已存在的文件迁移时不动它自己的头：不该凭空塞进我们的文件头，运维的注释也
	// 要原样留着。
	if strings.HasPrefix(content, "# Neu Box Runtime configuration") {
		t.Fatalf("迁移不该给已存在的文件加文件头：\n%s", content)
	}
	for _, want := range []string{
		"# 运维自己的注释，不许丢",
		"NEU_BOX_自己加的=原样保留",
		EnvVersion + "=1",
		EnvWorkerURL + "=http://127.0.0.1:61234",
		EnvRealRunc + "=/opt/runc/bin/runc",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("迁移后丢了 %q：\n%s", want, content)
		}
	}
	// hook 的路径和 phase 没给现场事实，保持默认。
	if !strings.Contains(content, EnvHookPath+"="+DefaultHookPath) {
		t.Fatalf("hook 路径被动了：\n%s", content)
	}
}

func TestEnsureKeepsOperatorEdits(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	edited := EnvVersion + "=1\n" +
		EnvWorkerURL + "=http://127.0.0.1:9999\n" + // 手改过：不是默认值
		EnvHookPath + "=" + DefaultHookPath + "\n" +
		EnvHookPhase + "=prestart\n" + // 手改过：切了退路
		EnvRealRunc + "=" + DefaultRealRunc + "\n" // 还是默认值
	if err := os.WriteFile(path, []byte(edited), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	if _, err := Ensure(Options{
		Path:      path,
		WorkerURL: "http://127.0.0.1:61234",
		HookPhase: "createRuntime",
		RealRunc:  "/opt/runc/bin/runc",
	}); err != nil {
		t.Fatalf("Ensure：%v", err)
	}
	content := readFile(t, path)
	if !strings.Contains(content, EnvWorkerURL+"=http://127.0.0.1:9999") {
		t.Fatalf("手改过的 worker 地址被覆盖了：\n%s", content)
	}
	if !strings.Contains(content, EnvHookPhase+"=prestart") {
		t.Fatalf("手改过的 hook phase 被覆盖了：\n%s", content)
	}
	// 还是默认值的那个键要被现场事实修掉 —— 这就是模板埋错 runc 路径的老问题。
	if !strings.Contains(content, EnvRealRunc+"=/opt/runc/bin/runc") {
		t.Fatalf("等于默认值的键没被本机事实修正：\n%s", content)
	}
}

func TestEnsureForceRewritesEverything(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	edited := "# 注释\n" + EnvWorkerURL + "=http://127.0.0.1:9999\n"
	if err := os.WriteFile(path, []byte(edited), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	result, err := Ensure(Options{Path: path, RealRunc: "/opt/runc/bin/runc", Force: true})
	if err != nil {
		t.Fatalf("Ensure：%v", err)
	}
	if !result.Rewritten {
		t.Fatalf("--force 应报重写：%+v", result)
	}
	content := readFile(t, path)
	if strings.Contains(content, "9999") || strings.Contains(content, "# 注释") {
		t.Fatalf("--force 应当整份重写：\n%s", content)
	}
	if !strings.HasPrefix(content, "# Neu Box Runtime configuration") {
		t.Fatalf("--force 重写应当带上文件头：\n%s", content)
	}
	if !strings.Contains(content, EnvWorkerURL+"="+DefaultWorkerURL) {
		t.Fatalf("--force 之后应当是内置默认值：\n%s", content)
	}
}

// 梯子本身要能被验证：现在没有真实的迁移步骤，所以临时装一个假的进来，证明
// "按版本顺序跑、改了就算迁移过"这套机制是活的，而不是摆设。
func TestEnsureRunsMigrationLadderInOrder(t *testing.T) {
	cleanEnv(t)
	original := migrations
	defer func() { migrations = original }()

	var ran []int
	migrations = []func([]string) ([]string, bool){
		func(lines []string) ([]string, bool) { // 0 → 1
			ran = append(ran, 1)
			lines, _ = setKeys(lines, map[string]string{EnvHookPhase: "prestart"})
			return lines, true
		},
	}
	if ConfigVersion != 1 {
		t.Fatalf("这个用例是按 ConfigVersion=1 写的；改了版本要一起改它")
	}

	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, []byte(EnvHookPhase+"=createRuntime\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	// 文件没有版本键 = 版本 0，所以 0→1 这一步要跑，而且跑完要补上版本键。
	// 注意 setKeys 在上面的迁移步骤里改过 hook phase，迁移不会把它改回来。
	result, err := Ensure(Options{Path: path})
	if err != nil {
		t.Fatalf("Ensure：%v", err)
	}
	if len(ran) != 1 || ran[0] != 1 {
		t.Fatalf("梯子没按顺序跑：%v", ran)
	}
	if !result.Migrated {
		t.Fatalf("跑过梯子却没报 Migrated：%+v", result)
	}

	// 已经是最新版本的文件不该再跑一次梯子。
	ran = nil
	if _, err := Ensure(Options{Path: path}); err != nil {
		t.Fatalf("第二次 Ensure：%v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("版本已是最新却又跑了梯子：%v", ran)
	}
}

// 比本二进制还新的文件要拒绝：宁可让部署脚本停下来，也不要把未来的键按今天的
// 规则重写一遍。
func TestEnsureRefusesNewerSchema(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, []byte(EnvVersion+"=99\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	if _, err := Ensure(Options{Path: path}); err == nil {
		t.Fatal("比本二进制新的 schema 应当报错")
	}
	if content := readFile(t, path); !strings.Contains(content, EnvVersion+"=99") {
		t.Fatalf("报错之后不该动文件：\n%s", content)
	}
}

// 认不出的行是 warning，不是失败：迁移是部署动作，但不该因为一行写得怪就拦住。
// 改做的事照做，warning 交回给调用方。
func TestEnsureWarnsOnUnknownLinesButStillMigrates(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, []byte("这行没有等号\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}

	result, err := Ensure(Options{Path: path})
	// 认不出的行是 warning，不是致命错误：Ensure 照常成功返回，问题走 Warnings。
	if err != nil {
		t.Fatalf("认不出的行不该是致命错误：%v", err)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0].Error(), "不认识的行") {
		t.Fatalf("warning 没从 Result.Warnings 出来：%+v", result.Warnings)
	}
	if len(result.Updated) == 0 {
		t.Fatalf("warning 不该挡住补键：%+v", result)
	}
	if content := readFile(t, path); !strings.Contains(content, EnvVersion+"=1") {
		t.Fatalf("版本键没补上：\n%s", content)
	}
}

func TestInspectReportsSources(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, []byte(
		EnvVersion+"=1\n"+EnvRealRunc+"=/opt/runc/bin/runc\n"), 0o640); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	t.Setenv(EnvHookPhase, "prestart")

	snapshot, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect：%v", err)
	}
	if snapshot.Missing || snapshot.FileVersion != 1 {
		t.Fatalf("快照不对：%+v", snapshot)
	}
	if snapshot.Sources[EnvRealRunc] != "file" {
		t.Fatalf("文件里的键应当报 file：%+v", snapshot.Sources)
	}
	// 环境变量优先于文件 —— 和 Load 的行为保持一致。
	if snapshot.Sources[EnvHookPhase] != "env" ||
		snapshot.Values[EnvHookPhase] != "prestart" {
		t.Fatalf("环境变量没盖过文件：%+v", snapshot)
	}
	if snapshot.Sources[EnvWorkerURL] != "default" ||
		snapshot.Values[EnvWorkerURL] != DefaultWorkerURL {
		t.Fatalf("缺键应当报 default：%+v", snapshot)
	}
}

func TestInspectReportsMissingFile(t *testing.T) {
	cleanEnv(t)
	snapshot, err := Inspect(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("文件不存在不该报错（运行时就是按默认值工作的）：%v", err)
	}
	if !snapshot.Missing || snapshot.FileVersion != 0 {
		t.Fatalf("快照不对：%+v", snapshot)
	}
}
