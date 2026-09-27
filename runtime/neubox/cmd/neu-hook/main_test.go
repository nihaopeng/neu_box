package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neusbox/neu_box/runtime/neubox/internal/config"
)

func testConfig(workerURL string) config.Config {
	cfg := config.Default()
	cfg.WorkerURL = workerURL
	return cfg
}

// recorded 是假 Worker 收到的一次请求。
type recorded struct {
	path        string
	contentType string
	body        map[string]any
	raw         string
}

// fakeWorker 起一个冒充 Worker 的 httptest 服务器，记下收到的请求。
func fakeWorker(t *testing.T, status int, response string) (*httptest.Server, func() *recorded) {
	t.Helper()
	var (
		mu   sync.Mutex
		last *recorded
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		entry := &recorded{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), raw: string(raw)}
		_ = json.Unmarshal(raw, &entry.body)
		mu.Lock()
		last = entry
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	return server, func() *recorded {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func stateJSON(t *testing.T, state map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("序列化 state：%v", err)
	}
	return string(raw)
}

func TestRunRegistersAndExitsZero(t *testing.T) {
	for _, status := range []int{200, 201} {
		server, received := fakeWorker(t, status, `{"status":"registered"}`)
		state := stateJSON(t, map[string]any{
			"ociVersion":  "1.0.2",
			"id":          "0123456789abcdef",
			"pid":         4242,
			"bundle":      t.TempDir(),
			"annotations": map[string]string{"sandbox_cgroup": "sbx_yuxd_123.slice"},
		})
		var stderr strings.Builder
		if code := run(strings.NewReader(state), testConfig(server.URL), &stderr); code != 0 {
			t.Fatalf("HTTP %d 该退 0，实际退 %d：%s", status, code, stderr.String())
		}

		got := received()
		if got == nil {
			t.Fatal("Worker 没收到请求")
		}
		if got.path != "/container/register" {
			t.Fatalf("路径 = %q", got.path)
		}
		if got.contentType != "application/json" {
			t.Fatalf("Content-Type = %q", got.contentType)
		}
		// 契约里的三个必填字段，一个不多一个不少。
		want := map[string]any{
			"container_id":   "0123456789abcdef",
			"host_pid":       float64(4242),
			"sandbox_cgroup": "sbx_yuxd_123.slice",
		}
		if len(got.body) != len(want) {
			t.Fatalf("body 字段数 = %d，想要 %d：%s", len(got.body), len(want), got.raw)
		}
		for key, value := range want {
			if got.body[key] != value {
				t.Fatalf("body[%q] = %v，想要 %v（完整 body：%s）", key, got.body[key], value, got.raw)
			}
		}
	}
}

func TestRunFallsBackToBundleConfig(t *testing.T) {
	bundle := t.TempDir()
	// state 里没有 annotations，只有 bundle —— 契约规定的回退路径。
	if err := os.WriteFile(filepath.Join(bundle, configFileName),
		[]byte(`{"annotations":{"sandbox_cgroup":"sbx_from_bundle"}}`), 0o644); err != nil {
		t.Fatalf("写 config.json：%v", err)
	}
	server, received := fakeWorker(t, 201, `{}`)
	state := stateJSON(t, map[string]any{"id": "abc", "pid": 7, "bundle": bundle})

	var stderr strings.Builder
	if code := run(strings.NewReader(state), testConfig(server.URL), &stderr); code != 0 {
		t.Fatalf("该退 0，实际退 %d：%s", code, stderr.String())
	}
	if got := received(); got == nil || got.body["sandbox_cgroup"] != "sbx_from_bundle" {
		t.Fatalf("没有从 bundle 的 config.json 取到 annotation：%v", got)
	}
}

func TestRunPrefersStateAnnotations(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, configFileName),
		[]byte(`{"annotations":{"sandbox_cgroup":"sbx_from_bundle"}}`), 0o644); err != nil {
		t.Fatalf("写 config.json：%v", err)
	}
	server, received := fakeWorker(t, 201, `{}`)
	state := stateJSON(t, map[string]any{
		"id": "abc", "pid": 7, "bundle": bundle,
		"annotations": map[string]string{"sandbox_cgroup": "sbx_from_state"},
	})

	if code := run(strings.NewReader(state), testConfig(server.URL), io.Discard); code != 0 {
		t.Fatalf("该退 0，实际退 %d", code)
	}
	if got := received(); got == nil || got.body["sandbox_cgroup"] != "sbx_from_state" {
		t.Fatalf("state 里的 annotation 该优先：%v", got)
	}
}

// 授权的否定答案：Worker 明确说沙盒不存在 / 正在销毁 → 放行，但容器零卡。
func TestRunAllowsStartWithoutAuthorization(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{
			"沙盒不存在", 404,
			`{"error":"sandbox_cgroup 未匹配到 Worker 沙盒","code":"sandbox_not_found"}`,
			"sandbox_not_found",
		},
		{
			"沙盒正在销毁", 409,
			`{"error":"沙盒当前不可用","code":"sandbox_not_active"}`,
			"sandbox_not_active",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, received := fakeWorker(t, tc.status, tc.body)
			state := stateJSON(t, map[string]any{
				"id": "abc", "pid": 7,
				"annotations": map[string]string{"sandbox_cgroup": "sbx_a"},
			})
			var stderr strings.Builder
			if code := run(strings.NewReader(state), testConfig(server.URL), &stderr); code != 0 {
				t.Fatalf("该退 0（无授权放行），实际退 %d：%s", code, stderr.String())
			}
			if received() == nil {
				t.Fatal("请求没发出去")
			}
			// 警告必须点明"没有卡"，这是这条路唯一的观测点。
			if !strings.Contains(stderr.String(), "无授权") {
				t.Fatalf("stderr 没说明容器无授权：%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.code) {
				t.Fatalf("stderr 没带上 Worker 的业务码 %s：%s", tc.code, stderr.String())
			}
		})
	}
}

func TestRunFailsOnWorkerRejection(t *testing.T) {
	// 拿不到答案 / 身份有问题的非 2xx 一律不放行（404/409 那两种见上一条用例）。
	cases := []struct {
		status int
		body   string
	}{
		{400, `{"error":"参数缺失"}`},
		{404, `{"error":"代理回的 404，没有业务码"}`},
		{409, `{"error":"容器登记在别的沙盒里","code":"docker_container_registered_elsewhere"}`},
		{500, `{"error":"炸了"}`},
		{302, ``},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server, received := fakeWorker(t, tc.status, tc.body)
			state := stateJSON(t, map[string]any{
				"id": "abc", "pid": 7,
				"annotations": map[string]string{"sandbox_cgroup": "sbx_a"},
			})
			var stderr strings.Builder
			code := run(strings.NewReader(state), testConfig(server.URL), &stderr)
			if code == 0 {
				t.Fatalf("HTTP %d 必须退非零", tc.status)
			}
			if received() == nil {
				t.Fatal("请求没发出去")
			}
			// 报错要带上 Worker 说的话，否则现场只剩一个退出码。
			if !strings.Contains(stderr.String(), http.StatusText(tc.status)) {
				t.Fatalf("stderr 里没有状态码：%s", stderr.String())
			}
		})
	}
}

func TestRunFailsWhenWorkerUnreachable(t *testing.T) {
	server, _ := fakeWorker(t, 201, `{}`)
	url := server.URL
	server.Close() // 端口关掉，连不上

	state := stateJSON(t, map[string]any{
		"id": "abc", "pid": 7,
		"annotations": map[string]string{"sandbox_cgroup": "sbx_a"},
	})
	var stderr strings.Builder
	if code := run(strings.NewReader(state), testConfig(url), &stderr); code == 0 {
		t.Fatal("Worker 连不上必须退非零")
	}
	if stderr.Len() == 0 {
		t.Fatal("该往 stderr 说明原因")
	}
}

func TestRunFailsWithoutSandboxAnnotation(t *testing.T) {
	// annotation 缺失时连请求都不该发：没什么可登记的，直接失败。
	server, received := fakeWorker(t, 201, `{}`)
	state := stateJSON(t, map[string]any{"id": "abc", "pid": 7, "bundle": t.TempDir()})

	var stderr strings.Builder
	if code := run(strings.NewReader(state), testConfig(server.URL), &stderr); code == 0 {
		t.Fatal("没有 annotation 必须退非零")
	}
	if received() != nil {
		t.Fatal("没有 annotation 时不该发请求")
	}
	if !strings.Contains(stderr.String(), annotationKey) {
		t.Fatalf("stderr 里该点名缺了什么：%s", stderr.String())
	}
}

func TestRunFailsOnBadState(t *testing.T) {
	server, received := fakeWorker(t, 201, `{}`)
	cases := []struct {
		name  string
		stdin string
	}{
		{"空输入", ""},
		{"不是 JSON", "not json at all"},
		{"空对象", "{}"},
		{"pid 是 0", `{"id":"abc","pid":0,"annotations":{"sandbox_cgroup":"sbx_a"}}`},
		{"pid 是负数", `{"id":"abc","pid":-1,"annotations":{"sandbox_cgroup":"sbx_a"}}`},
		{"没有 id", `{"pid":7,"annotations":{"sandbox_cgroup":"sbx_a"}}`},
		{"没有 bundle 也没有 annotation", `{"id":"abc","pid":7}`},
		{"bundle 里没有 config.json", `{"id":"abc","pid":7,"bundle":"/nonexistent-bundle"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			if code := run(strings.NewReader(tc.stdin), testConfig(server.URL), &stderr); code == 0 {
				t.Fatalf("必须退非零（stderr：%s）", stderr.String())
			}
			if received() != nil {
				t.Fatal("state 不完整时不该发请求")
			}
		})
	}
}

func TestRunFailsOnTimeout(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(201)
	}))
	t.Cleanup(stall.Close)

	// 直接调 register，把超时压到 50ms —— run() 用的是 8s 常量，真等 8s 太慢。
	body := registerRequest{ContainerID: "abc", HostPID: 7, SandboxCgroup: "sbx_a"}
	start := time.Now()
	_, err := register(stall.URL, body, 50*time.Millisecond)
	if err == nil {
		t.Fatal("超时必须报错")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时没生效，等了 %v", elapsed)
	}
	// 顺带证明这个形状的 URL 拼得对。
	if !strings.HasSuffix(stall.URL+registerPath, "/container/register") {
		t.Fatal("端点拼错了")
	}
}

func TestWorkerURLWithTrailingSlash(t *testing.T) {
	server, received := fakeWorker(t, 201, `{}`)
	state := stateJSON(t, map[string]any{
		"id": "abc", "pid": 7,
		"annotations": map[string]string{"sandbox_cgroup": "sbx_a"},
	})
	if code := run(strings.NewReader(state), testConfig(server.URL+"/"), io.Discard); code != 0 {
		t.Fatalf("该退 0，实际退 %d", code)
	}
	if got := received(); got == nil || got.path != "/container/register" {
		t.Fatalf("多一个斜杠就拼错路径了：%v", got)
	}
}

func TestHTTPTimeoutFitsInsideHookTimeout(t *testing.T) {
	// 契约：hook 自身 timeout 10s（由 neu-box-runtime 写进 OCI hook 记录），
	// HTTP 必须严格小于它。超了的话 runc 会先杀掉 hook，错误都拿不到。
	const ociHookTimeout = 10 * time.Second
	if httpTimeout >= ociHookTimeout {
		t.Fatalf("httpTimeout = %v，必须小于 hook 自己的 %v", httpTimeout, ociHookTimeout)
	}
}
