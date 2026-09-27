// neu-box-hook：OCI runtime hook，把容器登记到 Worker。
//
// runc 在 hooks.<phase> 阶段拉起它（默认 createRuntime，prestart 可切回，见
// neu-box-runtime 的包注释），从 stdin 喂一份 OCI state。它取出容器 init 的
// 宿主机 PID 和 sandbox_cgroup，POST 给 Worker 的 /container/register。
//
// 唯一允许"登记不上还放行"的情况：Worker 明确回答"没有这个沙盒 / 正在销毁"
// （404 sandbox_not_found / 409 sandbox_not_active）。那时容器起来但零卡（BPF
// 查不到委托）。其余失败一律退非 0 —— 详见 docs/runtime-hook.md。
//
// 它是 runc 拉起来的，不是交互终端：日志一律走 stderr。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neusbox/neu_box/runtime/neubox/internal/config"
)

const (
	// annotationKey 是契约规定的 annotation 键，值是沙盒名。
	annotationKey = "sandbox_cgroup"

	// registerPath 是 Worker 的登记端点。
	//
	// 特意是个常量而不是配置项：路径是契约的一部分，Docker 装机时只有
	// runtime.env 里那四个键，多一个配置项就多一个能配错的地方。真需要可配
	// （比如 Worker 换了前缀）时从这里改成配置项即可，不用满仓库找字符串。
	registerPath = "/container/register"

	// configFileName 是 OCI bundle 里的配置文件名（runtime-spec 规定）。
	configFileName = "config.json"

	// httpTimeout 是 Worker 请求的超时。契约规定 hook 自身 timeout 10s（由
	// neu-box-runtime 写进 OCI hook 记录的 timeout 字段），HTTP 必须**严格小于**
	// 它：超时了还能自己退非 0 并留下原因，被 runc 杀掉就什么都拿不到。
	httpTimeout = 8 * time.Second

	// errorBodyLimit 是出错时从响应里读回来打日志的字节数上限，防止一个
	// 异常大的响应把我们拖住。
	errorBodyLimit = 4 << 10
)

// ociState 是 runc 从 stdin 喂进来的 OCI state（runtime-spec 的 State）。
// 只声明这里用得到的字段。
type ociState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations"`
}

// registerRequest 是 POST /container/register 的请求 body。
//
// 只发契约里必填的三个字段。container_cgroup 和 mount_namespace 是可选的
// "hook 观察值"，Worker 会拿它和自己从 /proc 读到的真值做交叉验证、不一致就
// 409 —— 也就是说报错了会把登记搞失败。而这两个值 Worker 本来就要自己读一遍，
// 我们从同一个 /proc 再读一遍并不能提供额外信息，却多了一个把容器卡死的理由，
// 所以不发。
type registerRequest struct {
	ContainerID   string `json:"container_id"`
	HostPID       int    `json:"host_pid"`
	SandboxCgroup string `json:"sandbox_cgroup"`
}

// registerOutcome 是登记请求结果里"能用来做判断"的那部分。
//
// status 为 0 表示压根没拿到响应（连不上、超时、构造失败…）。code 是 Worker
// 错误体里的业务码 —— 只有它明确说"沙盒不存在 / 正在销毁"时，hook 才允许放行。
type registerOutcome struct {
	status int
	code   string
}

// unregisteredAllowed 判断这次拒绝是不是"授权的否定答案"。
//
// 只认 Worker 给的业务码：代理/网关也可能还个 404，那属于"拿不到答案"，按失败处理。
func (outcome registerOutcome) unregisteredAllowed() bool {
	if outcome.status != http.StatusNotFound &&
		outcome.status != http.StatusConflict {
		return false
	}
	switch outcome.code {
	case "sandbox_not_found", "sandbox_not_active":
		return true
	}
	return false
}

func main() {
	cfg, warn := config.Load("")
	if warn != nil {
		logf(os.Stderr, "%v", warn)
	}
	os.Exit(run(os.Stdin, cfg, os.Stderr))
}

// run 是 main 的全部逻辑，返回进程退出码：0 只在登记确实成功时。
func run(stdin io.Reader, cfg config.Config, stderr io.Writer) int {
	state, err := readState(stdin)
	if err != nil {
		logf(stderr, "读 OCI state 失败：%v", err)
		return 1
	}
	if state.ID == "" || state.Pid <= 0 {
		logf(stderr, "OCI state 不完整（id=%q pid=%d），拒绝登记", state.ID, state.Pid)
		return 1
	}
	sandbox, err := sandboxName(state)
	if err != nil {
		logf(stderr, "%v", err)
		return 1
	}

	body := registerRequest{
		ContainerID:   state.ID,
		HostPID:       state.Pid,
		SandboxCgroup: sandbox,
	}
	outcome, err := register(cfg.WorkerURL, body, httpTimeout)
	if err != nil {
		if outcome.unregisteredAllowed() {
			// 放行但无授权。这行日志是这条路唯一的观测点（只在 dockerd 日志里）。
			logf(stderr,
				"沙盒 %s 已不存在或正在销毁（%s）—— 容器 %s 以**无授权**方式启动："+
					"容器内看不到任何 NPU。要卡请重新 acquire，并重建容器",
				sandbox, outcome.code, state.ID)
			return 0
		}
		logf(stderr, "登记容器 %s（沙盒 %s）失败：%v", state.ID, sandbox, err)
		return 1
	}
	logf(stderr, "已登记容器 %s → 沙盒 %s（host pid %d）", state.ID, sandbox, state.Pid)
	return 0
}

// readState 从 stdin 读 OCI state。
func readState(stdin io.Reader) (ociState, error) {
	var state ociState
	// 这个 decoder 只用来读一个 JSON 对象；runc 写完就关管道，不会挂住。
	if err := json.NewDecoder(stdin).Decode(&state); err != nil {
		return state, err
	}
	return state, nil
}

// sandboxName 取 sandbox_cgroup：OCI state 里的 annotations 优先，取不到再读
// <bundle>/config.json（契约规定的回退路径）。
func sandboxName(state ociState) (string, error) {
	if value := state.Annotations[annotationKey]; value != "" {
		return value, nil
	}
	if state.Bundle == "" {
		return "", fmt.Errorf("OCI state 里没有 %s annotation，也没有 bundle 路径可以回退", annotationKey)
	}
	configPath := filepath.Join(state.Bundle, configFileName)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("OCI state 里没有 %s annotation，读 %s 也失败：%v", annotationKey, configPath, err)
	}
	var parsed struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("OCI state 里没有 %s annotation，%s 也不是合法 JSON：%v", annotationKey, configPath, err)
	}
	value := parsed.Annotations[annotationKey]
	if value == "" {
		return "", fmt.Errorf("OCI state 和 %s 里都没有 %s annotation", configPath, annotationKey)
	}
	return value, nil
}

// register 把登记请求发给 Worker；非 2xx 一律返回 error，同时把状态码和 Worker
// 的业务码带回去给调用方判断（见 registerOutcome）。
// timeout 单独传是为了能测：生产路径永远用 httpTimeout。
func register(workerURL string, body registerRequest,
	timeout time.Duration) (registerOutcome, error) {
	var outcome registerOutcome
	payload, err := json.Marshal(body)
	if err != nil {
		return outcome, fmt.Errorf("序列化请求：%w", err)
	}
	url := strings.TrimSuffix(workerURL, "/") + registerPath
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return outcome, fmt.Errorf("构造请求 %s：%w", url, err)
	}
	request.Header.Set("Content-Type", "application/json")

	// 不重试：hook 的整个预算是 10s，重试只会把超时吃光，而且登记失败重来一次
	// 也还是同一个结果（沙盒不存在、身份冲突这类都是确定性的）。
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return outcome, fmt.Errorf("POST %s：%w", url, err)
	}
	defer response.Body.Close()

	detail, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
	outcome.status = response.StatusCode
	if outcome.status < 200 || outcome.status > 299 {
		// 业务码只在错误体里；解析不出来就按"拿不到答案"处理（不放行）。
		var parsed struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(detail, &parsed) == nil {
			outcome.code = parsed.Code
		}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return outcome, fmt.Errorf("POST %s 返回 %s：%s", url, response.Status, strings.TrimSpace(string(detail)))
	}
	return outcome, nil
}

// logf 写 stderr。hook 的 stdout 归 runc，日志一律走 stderr。
func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "neu-box-hook: "+format+"\n", args...)
}
