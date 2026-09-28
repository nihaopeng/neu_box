package cli

import (
	"fmt"
	"net/http"

	"github.com/neusbox/neu_box/native/client/internal/api"
)

// requiredAPIVersion 是本客户端要求的最低 worker API 版本。
// worker 0.4.0 起使用 /tasks 资源接口（API v2）；低于该版本不兼容。
const requiredAPIVersion = 2

// healthResponse 对应 worker GET /healthz。
type healthResponse struct {
	Status     string `json:"status"`
	Role       string `json:"role"`
	APIVersion *int   `json:"api_version"`
	Version    string `json:"version"`
	SchemaVer  any    `json:"schema_version"`
}

// runCheck 检查目标 worker 的可达性与 API 版本兼容性。
func (a *app) runCheck(args []string) int {
	if len(args) != 0 {
		return a.usageError("用法: neubox check")
	}
	status, raw, err := a.worker.Request(http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		return a.requestError(err)
	}
	if status != http.StatusOK {
		a.printError("worker_health_failed", fmt.Sprintf("worker /healthz 返回 HTTP %d", status))
		return 1
	}
	var health healthResponse
	if err := api.DecodeJSON(raw, &health); err != nil {
		return a.internalError("worker_health_invalid", fmt.Errorf("解析 /healthz 失败: %w", err))
	}
	apiVersion := "unknown"
	compatible := false
	if health.APIVersion != nil {
		apiVersion = fmt.Sprint(*health.APIVersion)
		compatible = *health.APIVersion >= requiredAPIVersion
	}
	if a.jsonOutput {
		_ = printJSONValue(a.out, map[string]any{
			"worker": health.Role, "version": health.Version,
			"schema": health.SchemaVer, "api_version": health.APIVersion,
			"compatible": compatible,
		})
	} else {
		printFields(a.out,
			outputField{"worker", health.Role},
			outputField{"version", health.Version},
			outputField{"schema", fmt.Sprint(health.SchemaVer)},
			outputField{"api_version", apiVersion},
			outputField{"compatible", fmt.Sprint(compatible)},
		)
	}
	if health.APIVersion == nil {
		a.printError("api_version_missing", "worker 未上报 api_version，不支持 /tasks；请升级 worker")
		return 1
	}
	if *health.APIVersion < requiredAPIVersion {
		a.printError("api_version_too_old", "worker API 版本过低，请升级 worker 或降级客户端")
		return 1
	}
	return 0
}
