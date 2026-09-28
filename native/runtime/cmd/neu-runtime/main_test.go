package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neusbox/neu_box/native/runtime/internal/config"
)

// childModeEnv 让测试二进制把自己当成 neu-box-runtime 跑起来。
//
// 这个包的 run() 最后要 syscall.Exec 换掉自己，在测试进程里没法调；端到端的
// argv 转发验证只能另起一个进程。TestMain 里认这个环境变量，子进程用它当
// 命令行（os.Args[1:] 就是 runc 的 argv）。
const (
	childModeEnv = "NEU_BOX_RUNTIME_TEST_CHILD"
	argvOutEnv   = "NEU_BOX_RUNTIME_TEST_ARGV_OUT"
	configEnv    = "NEU_BOX_CONFIG"
)

func TestMain(m *testing.M) {
	if os.Getenv(childModeEnv) == "1" {
		// 子进程只吃环境变量：NEU_BOX_CONFIG 由测试指向一个自己的文件，
		// 免得受这台机器上真的 /etc/neu-box/runtime.env 影响。
		cfg, warn := config.Load("")
		if warn != nil {
			fmt.Fprintf(os.Stderr, "neu-box-runtime: %v\n", warn)
		}
		os.Exit(run(os.Args[1:], cfg, os.Stderr))
	}
	os.Exit(m.Run())
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.RealRunc = "/bin/true"
	cfg.HookPath = "/usr/libexec/neu-box/neu-box-hook"
	return cfg
}

// writeBundle 造一个 <bundle>/config.json，返回 bundle 目录。
func writeBundle(t *testing.T, content string) string {
	t.Helper()
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, configFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("写 config.json 失败：%v", err)
	}
	return bundle
}

func readConfig(t *testing.T, bundle string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundle, configFileName))
	if err != nil {
		t.Fatalf("读 config.json 失败：%v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("config.json 不是合法 JSON：%v\n%s", err, raw)
	}
	return parsed
}

// hookPaths 取 hooks.<phase> 里所有 hook 的 path，按出现顺序。
func hookPaths(t *testing.T, parsed map[string]any, phase string) []string {
	t.Helper()
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	entries, ok := hooks[phase].([]any)
	if !ok {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		record, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("hooks.%s 里的元素不是对象：%v", phase, entry)
		}
		path, _ := record["path"].(string)
		paths = append(paths, path)
	}
	return paths
}

func TestPrepareForwardsEveryOtherSubcommand(t *testing.T) {
	// 只有 create 和 run 会注入，别的子命令逐个确认：argv 逐字转发、文件不动。
	for _, sub := range []string{"start", "state", "kill", "delete", "exec", "list", "pause", "spec", "features", "ps", "restore", "update"} {
		args := []string{sub, "abc123"}
		bundle := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
		args = append(args, "--bundle", bundle)
		before, _ := os.ReadFile(filepath.Join(bundle, configFileName))

		argv, err := prepare(args, testConfig(), io.Discard)
		if err != nil {
			t.Fatalf("%s：不该报错：%v", sub, err)
		}
		if !reflect.DeepEqual(argv, args) {
			t.Fatalf("%s：argv 被改了\n got %v\nwant %v", sub, argv, args)
		}
		after, _ := os.ReadFile(filepath.Join(bundle, configFileName))
		if string(before) != string(after) {
			t.Fatalf("%s：不该动 config.json", sub)
		}
	}
}

func TestPrepareForwardsBadInputUntouched(t *testing.T) {
	// 这台机器上所有容器都走 wrapper，所以这些"看不懂"的输入必须原样透传。
	cases := []struct {
		name    string
		args    func(t *testing.T) []string
		phase   string
		explain string
		// wantLog：不注入的子命令（state/kill/delete…）调用很频繁，这类静默放行；
		// 只有认真考虑过注入的 create/run 才需要留一行"为什么没注入"。
		wantLog bool
	}{
		{
			name:    "空 argv",
			args:    func(*testing.T) []string { return nil },
			explain: "runc 没有参数时自己打 help，wrapper 不该插手",
		},
		{
			name:    "未知子命令",
			args:    func(*testing.T) []string { return []string{"frobnicate", "--whatever"} },
			explain: "runc 加新子命令时 wrapper 不认识也必须放行",
		},
		{
			name:    "create 没带 --bundle",
			args:    func(*testing.T) []string { return []string{"create", "abc123"} },
			explain: "找不到 bundle 就判断不了该不该注入",
			wantLog: true,
		},
		{
			name:    "run 没带 --bundle",
			args:    func(*testing.T) []string { return []string{"run", "-i", "abc123"} },
			explain: "run 也注入，但没 bundle 一样判断不了",
			wantLog: true,
		},
		{
			name: "create 的 bundle 里没有 config.json",
			args: func(t *testing.T) []string {
				return []string{"create", "--bundle", t.TempDir(), "abc123"}
			},
			explain: "bundle 目录还不存在或还没写 config.json",
			wantLog: true,
		},
		{
			name: "config.json 是坏 JSON",
			args: func(t *testing.T) []string {
				return []string{"create", "--bundle", writeBundle(t, `{"annotations": {`), "abc123"}
			},
			explain: "半个 JSON 也不能挡住容器",
			wantLog: true,
		},
		{
			name: "config.json 是空文件",
			args: func(t *testing.T) []string {
				return []string{"create", "--bundle", writeBundle(t, ""), "abc123"}
			},
			explain: "空文件解析不出 annotations",
			wantLog: true,
		},
		{
			name: "annotations 不是字符串映射",
			args: func(t *testing.T) []string {
				return []string{"create", "--bundle", writeBundle(t, `{"annotations":["a","b"]}`), "abc123"}
			},
			explain: "别人的 annotation 格式再怪也与我们无关",
			wantLog: true,
		},
		{
			name: "phase 配错也不能挡无关容器",
			args: func(t *testing.T) []string {
				return []string{"create", "--bundle", writeBundle(t, `{"annotations":{"other":"x"}}`), "abc123"}
			},
			phase:   "not-a-phase",
			explain: "配置写错是全局的，但只影响沙盒容器",
			wantLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args(t)
			cfg := testConfig()
			if tc.phase != "" {
				cfg.HookPhase = tc.phase
			}
			var stderr strings.Builder
			argv, err := prepare(args, cfg, &stderr)
			if err != nil {
				t.Fatalf("必须原样转发，不能报错（%s）：%v", tc.explain, err)
			}
			if !reflect.DeepEqual(argv, args) {
				t.Fatalf("argv 被改了（%s）\n got %v\nwant %v", tc.explain, argv, args)
			}
			if tc.wantLog && stderr.Len() == 0 {
				t.Fatalf("应该往 stderr 留一句话说明为什么没注入（%s）", tc.explain)
			}
			if !tc.wantLog && stderr.Len() != 0 {
				t.Fatalf("这类调用该静默放行，却写了日志：%s", stderr.String())
			}
		})
	}
}

func TestPrepareInjectsHookWhenAnnotated(t *testing.T) {
	// create 和 run 都注入（run = create+start，本机直连 runc 验证要用）；
	// 两个 phase 都覆盖 —— prestart 不是遗留垃圾，它是整条 Docker 链路上唯一
	// 验过的那个，createRuntime 在完整链路上出问题时靠切回它兜底，所以两个都得
	// 能跑。
	for _, sub := range []string{"create", "run"} {
		for _, phase := range []string{"prestart", "createRuntime"} {
			t.Run(sub+"/"+phase, func(t *testing.T) {
				bundle := writeBundle(t, `{
  "ociVersion": "1.0.2",
  "annotations": {"sandbox_cgroup": "sbx_yuxd_123.slice"},
  "process": {"args": ["/bin/sleep", "30"]}
}`)
				cfg := testConfig()
				cfg.HookPhase = phase
				args := []string{sub, "--bundle", bundle, "abc123"}

				argv, err := prepare(args, cfg, io.Discard)
				if err != nil {
					t.Fatalf("prepare：%v", err)
				}
				if !reflect.DeepEqual(argv, args) {
					t.Fatalf("argv 必须逐字转发\n got %v\nwant %v", argv, args)
				}

				parsed := readConfig(t, bundle)
				if got := hookPaths(t, parsed, phase); !reflect.DeepEqual(got, []string{cfg.HookPath}) {
					t.Fatalf("hooks.%s 里的 hook 不对：%v", phase, got)
				}
				// 别的 phase 一个都不能多出来。
				hooks := parsed["hooks"].(map[string]any)
				if len(hooks) != 1 {
					t.Fatalf("只该有 %s 一个 phase，实际：%v", phase, hooks)
				}
				record := hooks[phase].([]any)[0].(map[string]any)
				if got := record["timeout"]; got != float64(hookTimeoutSeconds) {
					t.Fatalf("timeout = %v，想要 %d（契约：hook 自身 10s）", got, hookTimeoutSeconds)
				}
				if got, _ := record["args"].([]any); len(got) != 1 || got[0] != "neu-box-hook" {
					t.Fatalf("hook args 不对：%v", record["args"])
				}
				// 其余字段原样保留。
				process := parsed["process"].(map[string]any)
				if got := process["args"].([]any); !reflect.DeepEqual(got, []any{"/bin/sleep", "30"}) {
					t.Fatalf("process.args 被改坏了：%v", got)
				}
				if got := parsed["ociVersion"]; got != "1.0.2" {
					t.Fatalf("ociVersion 丢了：%v", got)
				}
			})
		}
	}
}

func TestPrepareBundleFlagForms(t *testing.T) {
	// --bundle DIR / --bundle=DIR / -b DIR（-b 是 runc 自己的短别名，本机
	// `runc run -b <bundle>` 就是这个写法）。
	bundle := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
	cfg := testConfig()
	for _, args := range [][]string{
		{"create", "--bundle", bundle, "abc123"},
		{"create", "--bundle=" + bundle, "abc123"},
		{"run", "--bundle", bundle, "abc123"},
		{"run", "--bundle=" + bundle, "abc123"},
		{"run", "-b", bundle, "abc123"},
		{"run", "-b", bundle},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := prepare(args, cfg, io.Discard); err != nil {
				t.Fatalf("prepare：%v", err)
			}
			if got := hookPaths(t, readConfig(t, bundle), cfg.HookPhase); !reflect.DeepEqual(got, []string{cfg.HookPath}) {
				t.Fatalf("没注入：%v", got)
			}
		})
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	bundle := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
	cfg := testConfig()
	args := []string{"create", "--bundle", bundle, "abc123"}

	for i := 0; i < 3; i++ {
		if _, err := prepare(args, cfg, io.Discard); err != nil {
			t.Fatalf("第 %d 次 prepare：%v", i+1, err)
		}
	}
	if got := hookPaths(t, readConfig(t, bundle), cfg.HookPhase); !reflect.DeepEqual(got, []string{cfg.HookPath}) {
		t.Fatalf("重复执行不该重复注入：%v", got)
	}
}

func TestPrepareKeepsOtherHooksAndPhases(t *testing.T) {
	// 这台机器上还可能有 Ascend Docker Runtime 之类的 hook：同一个 phase 里的
	// 别人的 hook、以及别的 phase，都不能被我们覆盖掉。
	bundle := writeBundle(t, `{
  "annotations": {"sandbox_cgroup": "sbx_a"},
  "hooks": {
    "prestart": [
      {"path": "/usr/local/bin/ascend-docker-hook", "args": ["ascend"], "timeout": 30}
    ],
    "poststop": [{"path": "/usr/local/bin/cleanup-hook"}]
  }
}`)
	cfg := testConfig()
	cfg.HookPhase = "prestart"

	if _, err := prepare([]string{"create", "--bundle", bundle, "abc123"}, cfg, io.Discard); err != nil {
		t.Fatalf("prepare：%v", err)
	}

	parsed := readConfig(t, bundle)
	want := []string{"/usr/local/bin/ascend-docker-hook", cfg.HookPath}
	if got := hookPaths(t, parsed, "prestart"); !reflect.DeepEqual(got, want) {
		t.Fatalf("prestart 里的 hook 不对\n got %v\nwant %v", got, want)
	}
	if got := hookPaths(t, parsed, "poststop"); !reflect.DeepEqual(got, []string{"/usr/local/bin/cleanup-hook"}) {
		t.Fatalf("poststop 被动过：%v", got)
	}
	// 别人的 hook 记录本身要原样保留（timeout 30 别被吃掉）。
	hooks := parsed["hooks"].(map[string]any)
	first := hooks["prestart"].([]any)[0].(map[string]any)
	if first["timeout"] != float64(30) {
		t.Fatalf("别人的 hook 记录被改了：%v", first)
	}
}

func TestPrepareWithholdsContainerWhenInjectionImpossible(t *testing.T) {
	// 已经确认是沙盒容器（annotation 在手里）但注入做不成：宁可创建失败。
	// 放过去的话容器起得来、设备全被拒，现场极难查。
	cfg := testConfig()
	cfg.HookPhase = "not-a-phase"
	bundle := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
	if _, err := prepare([]string{"create", "--bundle", bundle, "abc123"}, cfg, io.Discard); err == nil {
		t.Fatal("phase 配错 + 沙盒容器：必须报错，不能放行")
	}

	// config.json 只读不可写（换个身份跑不了，用只读目录模拟）。
	readonly := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，跳过写失败这条")
	}
	if err := os.Chmod(readonly, 0o500); err != nil {
		t.Fatalf("chmod：%v", err)
	}
	defer os.Chmod(readonly, 0o700)
	if _, err := prepare([]string{"create", "--bundle", readonly, "abc123"}, testConfig(), io.Discard); err == nil {
		t.Fatal("写不回 config.json：必须报错，不能放行")
	}
}

func TestPreparePreservesFileMode(t *testing.T) {
	bundle := writeBundle(t, `{"annotations":{"sandbox_cgroup":"sbx_a"}}`)
	path := filepath.Join(bundle, configFileName)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod：%v", err)
	}
	if _, err := prepare([]string{"create", "--bundle", bundle, "abc123"}, testConfig(), io.Discard); err != nil {
		t.Fatalf("prepare：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat：%v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json 权限被改了：%v", info.Mode().Perm())
	}
}

func TestScanBundle(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"有 annotation", `{"annotations":{"sandbox_cgroup":"sbx_a"}}`, "sbx_a", false},
		{"没有任何 annotation", `{"ociVersion":"1.0.2"}`, "", false},
		{"annotations 是空的", `{"annotations":{}}`, "", false},
		{"只有别人的 annotation", `{"annotations":{"other":"x"}}`, "", false},
		{"annotations 是 null", `{"annotations":null}`, "", false},
		{"坏 JSON", `{`, "", true},
		{"顶层是数组", `[]`, "", true},
		{"annotations 不是映射", `{"annotations":["x"]}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanBundle(filepath.Join(writeBundle(t, tc.content), configFileName))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v，想要 wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("annotation = %q，想要 %q", got, tc.want)
			}
		})
	}
	t.Run("文件不存在", func(t *testing.T) {
		if _, err := scanBundle(filepath.Join(t.TempDir(), configFileName)); err == nil {
			t.Fatal("文件不存在必须报错")
		}
	})
}

func TestSubcommandSkipsFlagValues(t *testing.T) {
	// containerd 是把 --root/--log 的值当独立参数传的，不能把它们当子命令。
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "containerd 的真实形状",
			args: []string{"--root", "/run/docker/runtime-runc/moby", "--log", "/run/containerd/io.containerd.runtime.v2.task/moby/x/log.json", "--log-format", "json", "create", "--bundle", "/run/.../bundle", "--pid-file", "/run/.../pid", "abc123"},
			want: "create",
		},
		{"只有子命令", []string{"state", "abc123"}, "state"},
		{
			name: "本机直连 runc 的形状",
			args: []string{"--systemd-cgroup", "--root", "/var/tmp/state", "run", "-b", "/var/tmp/bundle", "neu-box-ok"},
			want: "run",
		},
		{"exec 后面跟着叫 create 的容器", []string{"exec", "create", "/bin/sh"}, "exec"},
		{"认不出来", []string{"--root", "/x", "frobnicate"}, ""},
		{"空", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subcommand(tc.args); got != tc.want {
				t.Fatalf("subcommand = %q，想要 %q", got, tc.want)
			}
		})
	}
}

func TestBundleFlag(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"分开写", []string{"create", "--bundle", "/tmp/b", "cid"}, "/tmp/b", true},
		{"等号", []string{"create", "--bundle=/tmp/b", "cid"}, "/tmp/b", true},
		{"等号空值", []string{"create", "--bundle=", "cid"}, "", true},
		{"短别名", []string{"run", "-b", "/tmp/b", "cid"}, "/tmp/b", true},
		{"没有", []string{"create", "cid"}, "", false},
		{"分开写但没值", []string{"create", "--bundle"}, "", false},
		{"短别名但没值", []string{"run", "-b"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bundleFlag(tc.args)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("bundleFlag = (%q, %v)，想要 (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestGuardSelfExec(t *testing.T) {
	// 指向自己：拒绝（否则每次 exec 都回到自己，容器创建永远不返回）。
	if err := guardSelfExec(os.Args[0]); err == nil {
		t.Fatal("REAL_RUNC 指向 wrapper 自己时必须报错")
	}
	// 指向别人：放行。
	if err := guardSelfExec("/bin/true"); err != nil {
		t.Fatalf("/bin/true 不该被挡：%v", err)
	}
	// 查不到的文件：LookPath 已经拦过一道，这里不重复拦。
	if err := guardSelfExec(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("查不到的文件不该在这里报错：%v", err)
	}
}

func TestExecArgvKeepsArgumentsVerbatim(t *testing.T) {
	got := execArgv("/usr/local/bin/runc", []string{"create", "--bundle", "/tmp/b"})
	want := []string{"/usr/local/bin/runc", "create", "--bundle", "/tmp/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v，想要 %v（argv[0] 是真 runtime 的路径）", got, want)
	}
}

// TestEndToEndForwardsToRealRuntime 真起一个进程跑 run()：验证 syscall.Exec 那条
// 路径、以及改完 config.json 之后交给真 runc 的 argv 还是原来那份。
// 假 runc 是个 shell 脚本，把收到的参数写进文件。
func TestEndToEndForwardsToRealRuntime(t *testing.T) {
	dir := t.TempDir()
	fakeRunc := filepath.Join(dir, "fake-runc.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$" + argvOutEnv + "\"\n"
	if err := os.WriteFile(fakeRunc, []byte(script), 0o755); err != nil {
		t.Fatalf("写假 runc：%v", err)
	}
	argvOut := filepath.Join(dir, "argv.txt")

	// 沙盒容器走两种 argv 形状：containerd 用的 create --bundle，以及本机验证用的
	// run -b。两边都要注入，而且转发给真 runc 的都必须是收到的那一份。
	createArgv := func(bundle string) []string { return []string{"create", "--bundle", bundle, "abc123"} }
	runArgv := func(bundle string) []string { return []string{"run", "-b", bundle, "abc123"} }

	cases := []struct {
		name       string
		config     string
		argv       func(bundle string) []string
		wantInject bool
	}{
		{"沙盒容器 create --bundle", `{"annotations":{"sandbox_cgroup":"sbx_a"},"process":{"args":["/bin/true"]}}`, createArgv, true},
		{"沙盒容器 run -b", `{"annotations":{"sandbox_cgroup":"sbx_a"},"process":{"args":["/bin/true"]}}`, runArgv, true},
		{"普通容器 create --bundle", `{"process":{"args":["/bin/true"]}}`, createArgv, false},
		{"普通容器 run -b", `{"process":{"args":["/bin/true"]}}`, runArgv, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := writeBundle(t, tc.config)
			args := tc.argv(bundle)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(),
				childModeEnv+"=1",
				configEnv+"="+filepath.Join(dir, "absent.env"),
				"NEU_BOX_REAL_RUNC="+fakeRunc,
				"NEU_BOX_HOOK="+filepath.Join(dir, "neu-box-hook"),
				// 故意不设 NEU_BOX_HOOK_PHASE：走的是默认值那条路，
				// 断言也用 config.DefaultHookPhase 跟着默认值走。
				argvOutEnv+"="+argvOut,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper 退出非零：%v\n%s", err, out)
			}
			raw, err := os.ReadFile(argvOut)
			if err != nil {
				t.Fatalf("假 runc 没被调用：%v", err)
			}
			got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if !reflect.DeepEqual(got, args) {
				t.Fatalf("转发给真 runc 的 argv 不对\n got %v\nwant %v", got, args)
			}

			injected := len(hookPaths(t, readConfig(t, bundle), config.DefaultHookPhase)) > 0
			if injected != tc.wantInject {
				t.Fatalf("注入 = %v，想要 %v", injected, tc.wantInject)
			}
		})
	}

	t.Run("真 runtime 不存在时报错退出", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "state", "abc123")
		cmd.Env = append(os.Environ(),
			childModeEnv+"=1",
			configEnv+"="+filepath.Join(dir, "absent.env"),
			"NEU_BOX_REAL_RUNC="+filepath.Join(dir, "no-such-runc"),
		)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("真 runtime 不存在时该退非零\n%s", out)
		}
	})

	t.Run("--help 原样转发给真 runc", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "--help")
		cmd.Env = append(os.Environ(),
			childModeEnv+"=1",
			configEnv+"="+filepath.Join(dir, "absent.env"),
			"NEU_BOX_REAL_RUNC="+fakeRunc,
			argvOutEnv+"="+argvOut,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("wrapper 退出非零：%v\n%s", err, out)
		}
		raw, err := os.ReadFile(argvOut)
		if err != nil {
			t.Fatalf("假 runc 没被调用：%v", err)
		}
		if got := strings.TrimSuffix(string(raw), "\n"); got != "--help" {
			t.Fatalf("转发 = %q，想要 %q", got, "--help")
		}
	})
}
