package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fullCapabilityNames 是 0..40 全部能力名（等价 --cap-add=ALL / --privileged）。
func fullCapabilityNames() []string {
	names := make([]string, 0, len(capBits))
	for name := range capBits {
		names = append(names, name)
	}
	return names
}

func defaultDockerCapabilityNames() []string {
	return []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL",
		"CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
		"CAP_NET_RAW", "CAP_SYS_CHROOT", "CAP_MKNOD", "CAP_AUDIT_WRITE",
		"CAP_SETFCAP",
	}
}

// bundleWithCaps 造一个带 capabilities 的 bundle（process 里还带 cwd/args/env 这些
// 同级字段 —— 剪能力位时**必须一个字节都不动它们**，真机上丢过 cwd，runc 直接报
// "Cwd property must not be empty"）。
func bundleWithCaps(t *testing.T, caps []string, extra string) string {
	t.Helper()
	body := map[string]any{
		"process": map[string]any{
			"cwd":      "/root/repos",
			"args":     []string{"sh", "-c", "sleep 3600"},
			"env":      []string{"PATH=/usr/bin", "HOME=/root"},
			"terminal": true,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"capabilities": map[string]any{
				"bounding":  caps,
				"effective": caps,
				"permitted": caps,
				"ambient":   []string{},
			},
		},
	}
	if extra != "" {
		var extraFields map[string]any
		if err := json.Unmarshal([]byte(extra), &extraFields); err != nil {
			t.Fatalf("extra 不是合法 JSON：%v", err)
		}
		for key, value := range extraFields {
			body[key] = value
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化 bundle：%v", err)
	}
	return writeBundle(t, string(encoded))
}

// 剪位之后 process 的其余字段必须逐字保留。
func TestCapGuardPreservesOtherProcessFields(t *testing.T) {
	bundle := bundleWithCaps(t, fullCapabilityNames(), "")
	path := filepath.Join(bundle, configFileName)

	_, err := applyCapGuard(path, capGuardModeDrop, nil)
	if err != nil {
		t.Fatalf("applyCapGuard: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 config.json：%v", err)
	}
	var spec struct {
		Process map[string]json.RawMessage `json:"process"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 config.json：%v", err)
	}
	// 比语义不比字节：写回时顶层用了 MarshalIndent，nested RawMessage 的缩进会变。
	want := map[string]any{
		"cwd":      "/root/repos",
		"args":     []any{"sh", "-c", "sleep 3600"},
		"env":      []any{"PATH=/usr/bin", "HOME=/root"},
		"terminal": true,
		"user":     map[string]any{"uid": float64(0), "gid": float64(0)},
	}
	for field, expected := range want {
		got, ok := spec.Process[field]
		if !ok {
			t.Fatalf("剪位把 process.%s 弄丢了（runc 会报 Cwd property must not be empty 那类错）", field)
		}
		var decoded any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("process.%s 解析失败：%v", field, err)
		}
		if !reflect.DeepEqual(decoded, expected) {
			t.Fatalf("process.%s 被改动：got=%v want=%v", field, decoded, expected)
		}
	}
}

func readCapabilities(t *testing.T, bundle string) ociCapabilities {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundle, configFileName))
	if err != nil {
		t.Fatalf("读 config.json：%v", err)
	}
	var spec struct {
		Process ociProcess `json:"process"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 config.json：%v", err)
	}
	if spec.Process.Capabilities == nil {
		t.Fatalf("config.json 里 capabilities 没了")
	}
	return *spec.Process.Capabilities
}

// 掩码必须和驱动那份逐位一致：bits 0..37。
func TestPrivilegedMaskMatchesDriverExpression(t *testing.T) {
	if privilegedMask != 0x3FFFFFFFFF {
		t.Fatalf("privilegedMask = %#x，期望 0x3FFFFFFFFF（驱动 CAP_AUDIT_READ+1 那个表达式）", privilegedMask)
	}
	if !coversPrivilegedMask(capabilitiesFromNames(fullCapabilityNames())) {
		t.Fatal("全部能力位应当覆盖掩码")
	}
	if coversPrivilegedMask(capabilitiesFromNames(defaultDockerCapabilityNames())) {
		t.Fatal("Docker 默认能力集不该覆盖掩码（否则会误伤正常容器）")
	}
	// 少一位就不覆盖 —— 这正是我们剪位能生效的原因。
	without := capabilitiesFromNames(fullCapabilityNames()) &^ (uint64(1) << capAuditReadBit)
	if coversPrivilegedMask(without) {
		t.Fatal("剪掉 CAP_AUDIT_READ 之后不该再覆盖掩码")
	}
}

func TestCapGuardDropsAuditReadForFullCapabilities(t *testing.T) {
	bundle := bundleWithCaps(t, fullCapabilityNames(), `{"annotations":{"sandbox_cgroup":"sbx_root_1.slice"}}`)

	changed, err := applyCapGuard(filepath.Join(bundle, configFileName), capGuardModeDrop, nil)
	if err != nil {
		t.Fatalf("applyCapGuard: %v", err)
	}
	if !changed {
		t.Fatal("全套能力位应当被剪掉一位")
	}

	caps := readCapabilities(t, bundle)
	for field, list := range map[string][]string{
		"bounding": caps.Bounding, "effective": caps.Effective,
		"permitted": caps.Permitted, "ambient": caps.Ambient,
	} {
		for _, name := range list {
			if strings.EqualFold(name, capGuardDropCapability) {
				t.Fatalf("%s 里还留着 %s", field, capGuardDropCapability)
			}
		}
	}
	reachable := capabilitiesFromNames(caps.Permitted) & capabilitiesFromNames(caps.Bounding)
	if coversPrivilegedMask(reachable) {
		t.Fatal("剪位之后仍然覆盖掩码，等于没剪")
	}
}

func TestCapGuardLeavesDefaultDockerCapabilitiesAlone(t *testing.T) {
	bundle := bundleWithCaps(t, defaultDockerCapabilityNames(), `{"annotations":{"sandbox_cgroup":"sbx_root_1.slice"}}`)
	path := filepath.Join(bundle, configFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 config.json：%v", err)
	}

	changed, err := applyCapGuard(path, capGuardModeDrop, nil)
	if err != nil {
		t.Fatalf("applyCapGuard: %v", err)
	}
	if changed {
		t.Fatal("默认能力集不该被动")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 config.json：%v", err)
	}
	if string(before) != string(after) {
		t.Fatal("不该改文件内容")
	}
}

func TestCapGuardSkipsUserNamespace(t *testing.T) {
	// 有 user namespace → cred->user_ns != init_user_ns → 驱动恒判非 admin。
	bundle := bundleWithCaps(t, fullCapabilityNames(),
		`{"linux":{"namespaces":[{"type":"user"},{"type":"mount"}]}}`)

	changed, err := applyCapGuard(filepath.Join(bundle, configFileName), capGuardModeDrop, nil)
	if err != nil {
		t.Fatalf("applyCapGuard: %v", err)
	}
	if changed {
		t.Fatal("带 user namespace 的容器不该被剪位")
	}
}

func TestCapGuardDenyModeRefuses(t *testing.T) {
	bundle := bundleWithCaps(t, fullCapabilityNames(), "")
	path := filepath.Join(bundle, configFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 config.json：%v", err)
	}

	changed, err := applyCapGuard(path, capGuardModeDeny, nil)
	if err == nil {
		t.Fatal("deny 模式下全套能力位必须被拒绝")
	}
	if changed {
		t.Fatal("拒绝时不该改文件")
	}
	if !strings.Contains(err.Error(), capGuardDropCapability) {
		t.Fatalf("报错里应当给出办法（剪掉 %s）：%v", capGuardDropCapability, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 config.json：%v", err)
	}
	if string(before) != string(after) {
		t.Fatal("拒绝时不该改文件内容")
	}
}

func TestCapGuardOffModeDoesNothing(t *testing.T) {
	bundle := bundleWithCaps(t, fullCapabilityNames(), "")
	changed, err := applyCapGuard(filepath.Join(bundle, configFileName), capGuardModeOff, nil)
	if err != nil || changed {
		t.Fatalf("off 模式应当完全不动：changed=%v err=%v", changed, err)
	}
}

func TestCapGuardForwardsUnreadableBundle(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.json")
	changed, err := applyCapGuard(missing, capGuardModeDrop, nil)
	if err != nil || changed {
		t.Fatalf("读不到 config.json 应当原样转发：changed=%v err=%v", changed, err)
	}
}

// ── docker exec 那条路：能力位在 --process <file> 里 ──────────────────

func writeProcessFile(t *testing.T, caps []string) string {
	t.Helper()
	body := map[string]any{
		"cwd":      "/root/repos",
		"args":     []string{"python", "train.py"},
		"env":      []string{"PATH=/usr/bin"},
		"terminal": false,
		"capabilities": map[string]any{
			"bounding": caps, "effective": caps, "permitted": caps,
			"ambient": []string{},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化 process：%v", err)
	}
	path := filepath.Join(t.TempDir(), "process.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("写 process.json：%v", err)
	}
	return path
}

func TestExecProcessFileFindsBothForms(t *testing.T) {
	for _, args := range [][]string{
		{"--root", "/run/runc", "exec", "--process", "/tmp/p.json", "cid", "true"},
		{"exec", "--process=/tmp/p.json", "cid", "true"},
	} {
		if got := execProcessFile(args); got != "/tmp/p.json" {
			t.Fatalf("%v → %q", args, got)
		}
	}
	if got := execProcessFile([]string{"exec", "cid", "true"}); got != "" {
		t.Fatalf("没有 --process 时应当返回空串，实际 %q", got)
	}
}

func TestCapGuardStripsExecProcessCaps(t *testing.T) {
	path := writeProcessFile(t, fullCapabilityNames())

	changed, err := applyCapGuardToExecProcess(path, capGuardModeDrop, nil)
	if err != nil {
		t.Fatalf("applyCapGuardToExecProcess: %v", err)
	}
	if !changed {
		t.Fatal("exec 的全套能力位应当被剪掉一位（否则 docker exec 起的进程仍是 admin）")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 process.json：%v", err)
	}
	if strings.Contains(string(raw), capGuardDropCapability) {
		t.Fatalf("process.json 里还留着 %s", capGuardDropCapability)
	}
	var process map[string]json.RawMessage
	if err := json.Unmarshal(raw, &process); err != nil {
		t.Fatalf("解析 process.json：%v", err)
	}
	for field, expected := range map[string]any{
		"cwd":  "/root/repos",
		"args": []any{"python", "train.py"},
		"env":  []any{"PATH=/usr/bin"},
	} {
		var decoded any
		if err := json.Unmarshal(process[field], &decoded); err != nil {
			t.Fatalf("解析 %s：%v", field, err)
		}
		if !reflect.DeepEqual(decoded, expected) {
			t.Fatalf("%s 被改动：got=%v want=%v", field, decoded, expected)
		}
	}
}

func TestCapGuardLeavesNarrowExecProcessAlone(t *testing.T) {
	path := writeProcessFile(t, defaultDockerCapabilityNames())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 process.json：%v", err)
	}
	changed, err := applyCapGuardToExecProcess(path, capGuardModeDrop, nil)
	if err != nil || changed {
		t.Fatalf("默认能力集不该被动：changed=%v err=%v", changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 process.json：%v", err)
	}
	if string(before) != string(after) {
		t.Fatal("不该改文件内容")
	}
}

// prepare 会对 exec 子命令做同一件事，并且 argv 原样转发。
func TestPrepareGuardsExecSubcommand(t *testing.T) {
	path := writeProcessFile(t, fullCapabilityNames())
	args := []string{"--root", "/run/runc", "exec", "--process", path, "cid", "true"}

	forwarded, err := prepare(args, testConfig(), io.Discard)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(forwarded) != len(args) {
		t.Fatalf("argv 不该被改动：%v", forwarded)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 process.json：%v", err)
	}
	if strings.Contains(string(raw), capGuardDropCapability) {
		t.Fatal("prepare 没有对 exec 的 process 文件剪位")
	}
}

// 与 annotation 无关：没登记的容器也要过这一关（它的危险不来自 annotation）。
func TestPrepareStripsFullCapabilitiesWithoutAnnotation(t *testing.T) {
	bundle := bundleWithCaps(t, fullCapabilityNames(), "")
	args := []string{"create", "--bundle", bundle, "container-id"}

	forwarded, err := prepare(args, testConfig(), io.Discard)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(forwarded) != len(args) {
		t.Fatalf("argv 不该被改动：%v", forwarded)
	}
	caps := readCapabilities(t, bundle)
	for _, name := range caps.Bounding {
		if strings.EqualFold(name, capGuardDropCapability) {
			t.Fatal("没有 annotation 的容器也应当被剪位")
		}
	}
}
