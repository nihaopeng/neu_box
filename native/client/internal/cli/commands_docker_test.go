package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// execCall 记录一次假 exec 的调用，测试靠它断言最终交给 docker 的 argv。
type execCall struct {
	path string
	argv []string
	env  []string
}

// recordExec 装一个假 exec：真的 syscall.Exec 成功后不返回，这里假装成功。
func recordExec(application *app) *execCall {
	call := &execCall{}
	application.execFn = func(path string, argv []string, env []string) error {
		call.path = path
		call.argv = argv
		call.env = env
		return nil
	}
	return call
}

func TestDockerRunInjectsAnnotationForOwnSandbox(t *testing.T) {
	var requestedPID string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/sandbox/status" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		requestedPID = request.URL.Query().Get("pid")
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"sandbox_name": "sbx_yuxd_42.slice",
			"sandbox":      map[string]any{"name": "sbx_yuxd_42.slice"},
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordExec(application)

	code := application.run([]string{"docker", "run", "--rm", "-it", "ubuntu", "bash"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if requestedPID != "222" {
		t.Fatalf("客户端应报自己的 PID (222)，实际报的是 %q", requestedPID)
	}
	if call.path != "/usr/bin/docker" {
		t.Fatalf("exec path=%q", call.path)
	}
	want := []string{
		"/usr/bin/docker",
		"run",
		"--annotation", "sandbox_cgroup=sbx_yuxd_42.slice",
		"--rm", "-it", "ubuntu", "bash",
	}
	if call.argv[0] != call.path {
		t.Fatalf("argv[0] 必须是可执行文件名本身（execve 语义），实际 argv[0]=%q path=%q: %q",
			call.argv[0], call.path, call.argv)
	}
	if !reflect.DeepEqual(call.argv, want) {
		t.Fatalf("got  %q\nwant %q", call.argv, want)
	}
	if len(call.env) == 0 {
		t.Fatal("exec 应该带上环境变量")
	}
}

// docker run 之后的参数逐字出现在 docker argv 里，包括开头的 --。
func TestDockerRunPassesArgumentsThroughVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"sandbox_name": "sbx_yuxd_42.slice",
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordExec(application)

	// 全是 neubox 不认识的参数：连 --sandbox 也一并交给 docker。
	passthrough := []string{"--", "-it", "--rm", "--sandbox", "x", "--annotation", "k=v", "ubuntu", "bash"}
	args := append([]string{"docker", "run"}, passthrough...)
	if code := application.run(args); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	want := append([]string{"/usr/bin/docker", "run", "--annotation", "sandbox_cgroup=sbx_yuxd_42.slice"}, passthrough...)
	if !reflect.DeepEqual(call.argv, want) {
		t.Fatalf("got  %q\nwant %q", call.argv, want)
	}
}

func TestDockerRunWithoutSandboxFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// Worker 对"不在沙盒里"就是 200 + null，不是错误码。
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"sandbox_name": nil,
			"sandbox":      nil,
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordExec(application)

	code := application.run([]string{"docker", "run", "--rm", "-it", "ubuntu", "bash"})
	if code == 0 {
		t.Fatal("没有沙盒时必须报错退出")
	}
	if call.argv != nil {
		t.Fatalf("不能在没定到沙盒时 exec docker: %q", call.argv)
	}
	if !strings.Contains(errOut.String(), "acquire") {
		t.Fatalf("错误信息应提示先 acquire: %s", errOut.String())
	}
}

// 容器里 PID 是 namespace 内视角，反查会撞上宿主机上别的进程，只能报错。
func TestDockerRunRefusesPIDLookupInsideContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("容器内不该拿 namespace 里的 PID 去查沙盒")
		writeJSON(t, writer, http.StatusOK, map[string]any{})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	application.insideContainer = func() bool { return true }
	call := recordExec(application)

	code := application.run([]string{"docker", "run", "--rm", "ubuntu"})
	if code == 0 {
		t.Fatal("容器内反查沙盒必须报错")
	}
	if call.argv != nil {
		t.Fatalf("不该 exec docker: %q", call.argv)
	}
	if !strings.Contains(errOut.String(), "宿主机终端") {
		t.Fatalf("错误信息应指出在宿主机执行: %s", errOut.String())
	}
}

func TestDockerRunReportsMissingDockerBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_a.slice"})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	application.lookPath = func(string) (string, error) {
		return "", errors.New("executable file not found in $PATH")
	}
	call := recordExec(application)

	code := application.run([]string{"docker", "run", "ubuntu"})
	if code == 0 {
		t.Fatal("找不到 docker 必须报错")
	}
	if call.argv != nil {
		t.Fatalf("不该 exec docker: %q", call.argv)
	}
	if !strings.Contains(errOut.String(), "docker") {
		t.Fatalf("错误信息应提到 docker: %s", errOut.String())
	}
}

func TestDockerRejectsUnsupportedSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"docker"},
		{"docker", "ps"},
		{"docker", "compose", "up"},
	} {
		application, _, errOut := testApplication("http://127.0.0.1:1")
		call := recordExec(application)

		if code := application.run(args); code != 2 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, errOut.String())
		}
		if call.argv != nil {
			t.Fatalf("%v: 不该 exec docker", args)
		}
		if !strings.Contains(errOut.String(), "docker help") && !strings.Contains(errOut.String(), "docker run") {
			t.Fatalf("%v: 错误信息应指出 docker help: %s", args, errOut.String())
		}
	}
}

// ── `neubox docker start`：两段式借条 ─────────────────────────────

const testContainerID = "0123456789abcdef0123456789abcdef" +
	"0123456789abcdef0123456789abcdef"

// startWorker 复刻 Worker 的借条端点：POST 存借条、GET 报认领状态。
type startWorker struct {
	Lent          map[string]any
	Queries       []url.Values
	State         string // GET 报的状态；空串当 pending
	PostCode      int    // 非 0 时 POST 直接返回它
	IntentSandbox string
	Queried       chan struct{}
}

func (worker *startWorker) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/container/intent" {
				t.Errorf("unexpected path: %s", request.URL.Path)
				return
			}
			switch request.Method {
			case http.MethodPost:
				decodeRequest(t, request, &worker.Lent)
				if worker.PostCode != 0 {
					writeJSON(t, writer, worker.PostCode, map[string]any{
						"error": "PID 222 不在任何沙盒中",
						"code":  "not_in_sandbox",
					})
					return
				}
				writeJSON(t, writer, http.StatusOK, map[string]any{
					"sandbox_name": "sbx_yuxd_42.slice",
					"state":        "pending",
				})
			case http.MethodGet:
				worker.Queries = append(worker.Queries, request.URL.Query())
				if worker.Queried != nil {
					select {
					case worker.Queried <- struct{}{}:
					default:
					}
				}
				state := worker.State
				if state == "" {
					state = "pending"
				}
				sandbox := worker.IntentSandbox
				if sandbox == "" {
					sandbox = "sbx_yuxd_42.slice"
				}
				writeJSON(t, writer, http.StatusOK, map[string]any{
					"container_id": request.URL.Query().Get("container_id"),
					"sandbox_name": sandbox,
					"state":        state,
				})
			default:
				t.Errorf("unexpected method: %s", request.Method)
			}
		}))
}

// runCall 记录一次 docker 子进程调用和一次 docker inspect。
type runCall struct {
	path    string
	argv    []string
	env     []string
	inspect []string
	stops   []string
}

type fakeStartedCommand struct {
	wait func() (int, error)
	kill func() error
}

func (command fakeStartedCommand) Wait() (int, error) { return command.wait() }
func (command fakeStartedCommand) Kill() error        { return command.kill() }

func recordRun(application *app, code int) *runCall {
	call := &runCall{}
	application.outputFn = func(path string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "stop" {
			call.stops = append(call.stops, args[1])
			return []byte(args[1] + "\n"), nil
		}
		call.inspect = append([]string{path}, args...)
		return []byte(`{"Id":"` + testContainerID + `","State":{"Running":false,"Status":"exited"},"HostConfig":{"Annotations":{"sandbox_cgroup":"sbx_old.slice"}}}`), nil
	}
	application.runFn = func(path string, argv []string, env []string) (int, error) {
		call.path, call.argv, call.env = path, argv, env
		return code, nil
	}
	application.startFn = func(path string, argv []string, env []string) (StartedCommand, error) {
		call.path, call.argv, call.env = path, argv, env
		return fakeStartedCommand{
			wait: func() (int, error) { return code, nil },
			kill: func() error { return nil },
		}, nil
	}
	return call
}

func TestDockerStartLendsOwnSandboxThenStartsContainer(t *testing.T) {
	worker := &startWorker{State: "consumed"}
	server := worker.start(t)
	defer server.Close()

	application, out, errOut := testApplication(server.URL)
	call := recordRun(application, 0)

	if code := application.run([]string{"docker", "start", "neu-test"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if worker.Lent["username"] != "yuxd" {
		t.Fatalf("借条应报当前用户，实际 %v", worker.Lent)
	}
	// 报的是自己的 PID：Worker 靠它反查沙盒，客户端说不出别人的沙盒名。
	if worker.Lent["pid"] != float64(222) {
		t.Fatalf("借条应报自己的 PID (222)，实际 %v", worker.Lent["pid"])
	}
	if worker.Lent["container_id"] != testContainerID {
		t.Fatalf("借条应按容器 ID 记，实际 %v", worker.Lent["container_id"])
	}
	wantInspect := []string{"/usr/bin/docker", "inspect", "--format",
		"{{json .}}", "neu-test"}
	if !reflect.DeepEqual(call.inspect, wantInspect) {
		t.Fatalf("inspect got %q\nwant %q", call.inspect, wantInspect)
	}
	wantArgv := []string{"/usr/bin/docker", "start", "neu-test"}
	if !reflect.DeepEqual(call.argv, wantArgv) {
		t.Fatalf("docker argv got %q\nwant %q", call.argv, wantArgv)
	}
	if len(call.env) == 0 {
		t.Fatal("子进程应该带上环境变量")
	}
	if !strings.Contains(out.String(), "sbx_yuxd_42.slice") {
		t.Fatalf("成功时应报出借出去的沙盒：%s", out.String())
	}
	query := worker.Queries[0]
	if query.Get("container_id") != testContainerID ||
		query.Get("username") != "yuxd" || query.Get("pid") != "222" {
		t.Fatalf("确认借条时查询串不对：%v", query)
	}
}

// docker 自己的选项跟在容器名后面，原样交给 docker。
func TestDockerStartPassesDockerOptionsThrough(t *testing.T) {
	worker := &startWorker{State: "consumed"}
	server := worker.start(t)
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)

	code := application.run([]string{"docker", "start", "neu-test", "-a", "-i"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	want := []string{"/usr/bin/docker", "start", "neu-test", "-a", "-i"}
	if !reflect.DeepEqual(call.argv, want) {
		t.Fatalf("got  %q\nwant %q", call.argv, want)
	}
}

func TestDockerStartAttachConfirmsBeforeWorkloadExits(t *testing.T) {
	worker := &startWorker{State: "consumed", Queried: make(chan struct{}, 1)}
	server := worker.start(t)
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)
	release := make(chan struct{})
	application.startFn = func(path string, argv []string, env []string) (StartedCommand, error) {
		call.path, call.argv, call.env = path, argv, env
		return fakeStartedCommand{
			wait: func() (int, error) { <-release; return 23, nil },
			kill: func() error { close(release); return nil },
		}, nil
	}
	result := make(chan int, 1)
	go func() { result <- application.run([]string{"docker", "start", "neu-test", "-a"}) }()
	select {
	case <-worker.Queried:
	case <-time.After(time.Second):
		t.Fatal("start -a 应在容器运行期间回查借条")
	}
	select {
	case code := <-result:
		t.Fatalf("容器未退出时 start -a 不应返回：%d", code)
	default:
	}
	close(release)
	select {
	case code := <-result:
		if code != 23 {
			t.Fatalf("exit=%d stderr=%s", code, errOut.String())
		}
	case <-time.After(time.Second):
		t.Fatal("容器退出后应返回 Docker 的退出码")
	}
	if !reflect.DeepEqual(call.argv, []string{"/usr/bin/docker", "start", "neu-test", "-a"}) {
		t.Fatalf("docker start -a 参数被改动: %q", call.argv)
	}
}

func TestDockerStartInteractiveAlsoRequiresConcurrentConfirmation(t *testing.T) {
	for _, option := range []string{"-i", "--interactive", "-i=true", "--interactive=true"} {
		attached, err := dockerStartAttachOption([]string{option})
		if err != nil || !attached {
			t.Fatalf("%s: attached=%t err=%v", option, attached, err)
		}
	}
	for _, option := range []string{"-i=false", "--interactive=false"} {
		attached, err := dockerStartAttachOption([]string{option})
		if err != nil || attached {
			t.Fatalf("%s: attached=%t err=%v", option, attached, err)
		}
	}
}

func TestDockerStartRejectsIntentConsumedByDifferentSandbox(t *testing.T) {
	worker := &startWorker{State: "consumed", IntentSandbox: "sbx_other.slice"}
	server := worker.start(t)
	defer server.Close()
	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)
	if code := application.run([]string{"docker", "start", "neu-test"}); code == 0 {
		t.Fatalf("借条被别的沙盒认领仍返回成功: %s", errOut.String())
	}
	if !reflect.DeepEqual(call.stops, []string{testContainerID}) {
		t.Fatalf("借条串线后应停止本次容器: %v", call.stops)
	}
}

func TestDockerStartAttachCancelsClientAndStopsContainerOnBindingFailure(t *testing.T) {
	worker := &startWorker{State: "consumed", IntentSandbox: "sbx_other.slice"}
	server := worker.start(t)
	defer server.Close()
	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)
	clientStopped := make(chan struct{})
	killed := false
	application.startFn = func(path string, argv []string, env []string) (StartedCommand, error) {
		call.path, call.argv, call.env = path, argv, env
		return fakeStartedCommand{
			wait: func() (int, error) { <-clientStopped; return 137, nil },
			kill: func() error { killed = true; close(clientStopped); return nil },
		}, nil
	}
	if code := application.run([]string{"docker", "start", "neu-test", "-a"}); code == 0 {
		t.Fatalf("错误借条不能让 -a 成功: %s", errOut.String())
	}
	if !killed || !reflect.DeepEqual(call.stops, []string{testContainerID}) {
		t.Fatalf("需先中止 Docker 客户端并停止容器: killed=%v stops=%v", killed, call.stops)
	}
}

func TestDockerStartAttachStopsDelayedContainerAfterEarlyStopMiss(t *testing.T) {
	worker := &startWorker{State: "consumed", IntentSandbox: "sbx_other.slice"}
	server := worker.start(t)
	defer server.Close()
	application, _, errOut := testApplication(server.URL)
	inspects, stops := 0, 0
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] == "inspect" {
			inspects++
			return dockerInspectJSON(inspects >= 3, true), nil
		}
		if args[0] == "stop" {
			stops++
			if stops == 1 {
				return nil, errors.New("container is not running yet")
			}
			return []byte(testContainerID), nil
		}
		t.Fatalf("unexpected docker call: %q", args)
		return nil, nil
	}
	clientStopped := make(chan struct{})
	application.startFn = func(string, []string, []string) (StartedCommand, error) {
		return fakeStartedCommand{
			wait: func() (int, error) { <-clientStopped; return 137, nil },
			kill: func() error { close(clientStopped); return nil },
		}, nil
	}
	if code := application.run([]string{"docker", "start", "neu-test", "-a"}); code == 0 {
		t.Fatalf("错误借条不能成功: %s", errOut.String())
	}
	if stops != 2 || inspects < 3 {
		t.Fatalf("需要发现延迟启动后再停止: stops=%d inspects=%d", stops, inspects)
	}
}

// 借条存不上时，不能启动一个无卡容器让后续脚本继续执行。
func TestDockerStartRejectsMissingSandboxBeforeStarting(t *testing.T) {
	worker := &startWorker{PostCode: http.StatusConflict}
	server := worker.start(t)
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)

	code := application.run([]string{"docker", "start", "neu-test"})
	if code == 0 {
		t.Fatalf("借不到沙盒应拒绝启动：exit=%d stderr=%s", code, errOut.String())
	}
	if call.argv != nil {
		t.Fatalf("不能启动容器: %q", call.argv)
	}
	if !strings.Contains(errOut.String(), "not_in_sandbox") {
		t.Fatalf("应说明授权失败原因：%s", errOut.String())
	}
	// 没借条就不用回查认领结果。
	if len(worker.Queries) != 0 {
		t.Fatalf("没借条不该去查认领状态：%v", worker.Queries)
	}
}

// 容器起来了但借条没被认领（过期等）：必须停止并返回失败。
func TestDockerStartStopsWhenTheLendIsNotConsumed(t *testing.T) {
	worker := &startWorker{State: "pending"}
	server := worker.start(t)
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	call := recordRun(application, 0)

	code := application.run([]string{"docker", "start", "neu-test"})
	if code == 0 {
		t.Fatalf("借卡未确认必须失败：exit=%d stderr=%s", code, errOut.String())
	}
	if call.argv == nil {
		t.Fatal("确认是 start 之后的事，docker 必须先起过")
	}
	if len(call.stops) != 1 || call.stops[0] != testContainerID {
		t.Fatalf("借卡失败后必须尝试停止原容器: %v", call.stops)
	}
	if !strings.Contains(errOut.String(), "sbx_yuxd_42.slice") {
		t.Fatalf("错误要说清该绑哪个沙盒：%s", errOut.String())
	}
}

func TestDockerStartNeedsTheContainerFirst(t *testing.T) {
	for _, args := range [][]string{
		{"docker", "start"},
		{"docker", "start", "-a", "neu-test"},
		{"docker", "start", "neu-test", "other-container"},
		{"docker", "start", "neu-test", "--checkpoint", "old"},
	} {
		application, _, errOut := testApplication("http://127.0.0.1:1")
		call := recordRun(application, 0)

		if code := application.run(args); code != 2 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, errOut.String())
		}
		if call.argv != nil {
			t.Fatalf("%v: 不该去 start", args)
		}
	}
}

func TestDockerStartRejectsContainerWithoutRuntimeAnnotation(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] != "inspect" {
			t.Fatalf("unmanaged container should not be started: %v", args)
		}
		return dockerInspectJSON(false, false), nil
	}
	application.runFn = func(string, []string, []string) (int, error) {
		t.Fatal("unmanaged container should not start")
		return 0, nil
	}
	if code := application.run([]string{"docker", "start", "neu-test"}); code == 0 ||
		!strings.Contains(errOut.String(), "不受 Neu Box 管理") {
		t.Fatalf("unmanaged start: exit=%d stderr=%s", code, errOut.String())
	}
}

// docker 自己失败（容器不存在等）：退出码原样透传，也不去问借条。
func TestDockerStartPropagatesDockerExitCode(t *testing.T) {
	worker := &startWorker{State: "consumed"}
	server := worker.start(t)
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	recordRun(application, 125)

	if code := application.run([]string{"docker", "start", "neu-test"}); code != 125 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if len(worker.Queries) != 0 {
		t.Fatalf("start 都没成功，不该去查认领状态：%v", worker.Queries)
	}
}

// 容器内 PID 是 namespace 视角，反查会撞上宿主机上别的进程，只能报错。
func TestDockerStartRefusesInsideContainer(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	application.insideContainer = func() bool { return true }
	call := recordRun(application, 0)

	if code := application.run([]string{"docker", "start", "neu-test"}); code == 0 {
		t.Fatal("容器内必须报错")
	}
	if call.argv != nil {
		t.Fatalf("不该去 start：%q", call.argv)
	}
	if !strings.Contains(errOut.String(), "宿主机终端") {
		t.Fatalf("错误信息应指出在宿主机执行：%s", errOut.String())
	}
}
