// 能力位守卫：让经过本 wrapper 的容器**永远不会被 Ascend 驱动判成 admin**。
//
// ── 驱动侧怎么判 ──────────────────────────────────────────────────
// cann/driver（src/sdk_driver/pbl/uda/uda_access.c）：
//
//	uda_is_admin_task() = cred->user_ns == &init_user_ns
//	                      && ka_system_kernel_cap_compare(effective, privileged)
//
// privileged 来自 ka_system_get_privileged_kernel_cap()
// （src/sdk_driver/kernel_adapt/system/ka_system.c），≥6.3 内核上是：
//
//	((CAP_TO_MASK(CAP_AUDIT_READ + 1) - 1) << 32) | (~0 >> 32)
//
// 内核里 CAP_TO_MASK(x) = 1U << (x & 31)、CAP_AUDIT_READ = 37，所以这个掩码
// = bits 0..37 —— 也就是"从 CAP_CHOWN 到 CAP_AUDIT_READ 的全部能力位"。
// 比较是**超集**判定（ka_system_kernel_cap_compare 里 `(cap1 & cap2) == cap2`），
// 所以容器必须一位不缺地拥有这 38 位才会被判成 admin —— 现实中只有
// `--privileged` / `--cap-add=ALL` 能凑齐，Docker 默认那十几个 cap 差得远。
//
// ── 被判成 admin 的后果 ───────────────────────────────────────────
// 驱动会给这个 mount namespace 建一张**全量** UDA 设备表
// （uda_init_ns_node_dev 走 UDA_MAX_PHY_DEV_NUM 分支，且每个设备都被
// uda_is_admin_ns 放行），表按 mnt ns 缓存。我们 worker 那套 eBPF 只拦
// open("/dev/davinciN")，而驱动这条建表/访问路径根本不看它 —— 容器于是能拿到
// 全部卡，沙盒隔离静默失效（真机已复现：申请 2 张卡的容器里
// torch.npu.device_count() == 8）。
//
// ── 对策：剪掉一位 ────────────────────────────────────────────────
// 掩码是超集判定，少一位就掉出 admin 分支；而**非 admin** 容器走的正是我们
// 期望的路径：
//
//	有 sandbox_cgroup annotation（已登记）→ UDA 表按 eBPF 放行的设备建
//	                                         = 沙盒持有那几张卡；
//	没有 annotation（未登记）           → 一张都没有，fail-closed。
//
// 选 CAP_AUDIT_READ(37)：它只影响读内核审计日志，容器几乎不可能用到；而它是
// 掩码覆盖得到的最后一位，剪它代价最小。注意四个集合（bounding/permitted/
// effective/ambient）都要剪 —— 只剪 effective 会被 permitted∩bounding 在 exec
// 时补回来（驱动的掩码判定最终读的是 cred->cap_effective，任何进程能达到的
// 上限是 permitted ∩ bounding）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

const (
	// capGuardDropCapability 是我们剪掉的那一位能力。
	capGuardDropCapability = "CAP_AUDIT_READ"
	capAuditReadBit        = 37

	// capGuardModeDrop 剪掉一位（默认）；capGuardModeDeny 直接拒绝创建；
	// capGuardModeOff 关闭守卫（排障用，默认不要开）。
	capGuardModeDrop = "drop"
	capGuardModeDeny = "deny"
	capGuardModeOff  = "off"
)

// privilegedMask 复刻驱动的 ka_system_get_privileged_kernel_cap()（≥6.3 分支）。
//
// 写成表达式而不是常数，是为了让"和驱动同一套算法"这件事在代码里看得见：
// CAP_TO_MASK(CAP_AUDIT_READ+1) - 1 = 0x3F → 左移 32 位得 bits 32..37，
// 再或上 (~0 >> 32) 的 bits 0..31，合起来就是 bits 0..37。
var privilegedMask = func() uint64 {
	low := (uint64(1) << uint((capAuditReadBit+1)&31)) - 1
	return (low << 32) | (^uint64(0) >> 32)
}()

// capBits 是 Linux uapi 里的能力位编号。
//
// 认不出来的名字直接忽略：runc 自己会拒绝非法名字，我们不必在这一层做校验，
// 而"以后新加的能力位"（≥41）本来就不在驱动掩码里。
var capBits = map[string]uint{
	"CAP_CHOWN":              0,
	"CAP_DAC_OVERRIDE":       1,
	"CAP_DAC_READ_SEARCH":    2,
	"CAP_FOWNER":             3,
	"CAP_FSETID":             4,
	"CAP_KILL":               5,
	"CAP_SETGID":             6,
	"CAP_SETUID":             7,
	"CAP_SETPCAP":            8,
	"CAP_LINUX_IMMUTABLE":    9,
	"CAP_NET_BIND_SERVICE":   10,
	"CAP_NET_BROADCAST":      11,
	"CAP_NET_ADMIN":          12,
	"CAP_NET_RAW":            13,
	"CAP_IPC_LOCK":           14,
	"CAP_IPC_OWNER":          15,
	"CAP_SYS_MODULE":         16,
	"CAP_SYS_RAWIO":          17,
	"CAP_SYS_CHROOT":         18,
	"CAP_SYS_PTRACE":         19,
	"CAP_SYS_PACCT":          20,
	"CAP_SYS_ADMIN":          21,
	"CAP_SYS_BOOT":           22,
	"CAP_SYS_NICE":           23,
	"CAP_SYS_RESOURCE":       24,
	"CAP_SYS_TIME":           25,
	"CAP_SYS_TTY_CONFIG":     26,
	"CAP_MKNOD":              27,
	"CAP_LEASE":              28,
	"CAP_AUDIT_WRITE":        29,
	"CAP_AUDIT_CONTROL":      30,
	"CAP_SETFCAP":            31,
	"CAP_MAC_OVERRIDE":       32,
	"CAP_MAC_ADMIN":          33,
	"CAP_SYSLOG":             34,
	"CAP_WAKE_ALARM":         35,
	"CAP_BLOCK_SUSPEND":      36,
	"CAP_AUDIT_READ":         37,
	"CAP_PERFMON":            38,
	"CAP_BPF":                39,
	"CAP_CHECKPOINT_RESTORE": 40,
}

// ociCapabilities 是 OCI spec 的 process.capabilities。
type ociCapabilities struct {
	Bounding  []string `json:"bounding,omitempty"`
	Effective []string `json:"effective,omitempty"`
	Permitted []string `json:"permitted,omitempty"`
	Ambient   []string `json:"ambient,omitempty"`
}

type ociProcess struct {
	Capabilities *ociCapabilities `json:"capabilities,omitempty"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces,omitempty"`
}

// capabilitiesFromNames 把能力名（或十进制编号）折成位图。
func capabilitiesFromNames(names []string) uint64 {
	var bits uint64
	for _, name := range names {
		key := strings.ToUpper(strings.TrimSpace(name))
		if bit, ok := capBits[key]; ok {
			bits |= uint64(1) << bit
			continue
		}
		if number, err := strconv.Atoi(key); err == nil && number >= 0 && number < 64 {
			bits |= uint64(1) << uint(number)
		}
	}
	return bits
}

// coversPrivilegedMask 判断这组能力位是否覆盖驱动那个 admin 掩码。
func coversPrivilegedMask(bits uint64) bool {
	return bits&privilegedMask == privilegedMask
}

// dropFromCapabilities 从四个集合里去掉某一位能力，返回是否有改动。
func dropFromCapabilities(caps *ociCapabilities, capability string) bool {
	changed := false
	drop := func(list []string) []string {
		kept := make([]string, 0, len(list))
		for _, name := range list {
			if strings.EqualFold(strings.TrimSpace(name), capability) {
				changed = true
				continue
			}
			kept = append(kept, name)
		}
		return kept
	}
	caps.Bounding = drop(caps.Bounding)
	caps.Effective = drop(caps.Effective)
	caps.Permitted = drop(caps.Permitted)
	caps.Ambient = drop(caps.Ambient)
	return changed
}

// applyCapGuard 读 <bundle>/config.json，必要时剪掉 capGuardDropCapability。
//
// 返回 (changed, error)：changed 表示确实改了文件；error 只在 deny 模式命中时
// 返回（调用方据此拒绝创建）。**读不了 / 解析不了的 bundle 一律返回
// (false, nil)**：这台机器上所有容器都从这条路走，wrapper 自己的问题不能挡住
// 无关容器（和注入 hook 的既定口径一致）—— 看不明白就没有把握判定，原样转发。
func applyCapGuard(configPath, mode string, log func(string, ...any)) (bool, error) {
	if mode == "" {
		mode = capGuardModeDrop
	}
	if mode == capGuardModeOff {
		return false, nil
	}
	if log == nil {
		log = func(string, ...any) {}
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log("读 %s 失败（%v），能力位没检查，原样转发", configPath, err)
		}
		return false, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		log("%s 不是合法 JSON（%v），能力位没检查，原样转发", configPath, err)
		return false, nil
	}

	// 有 user namespace → cred->user_ns != &init_user_ns → 驱动恒判非 admin。
	if rawLinux, ok := top["linux"]; ok {
		var linuxSpec ociLinux
		if err := json.Unmarshal(rawLinux, &linuxSpec); err == nil {
			for _, namespace := range linuxSpec.Namespaces {
				if strings.EqualFold(namespace.Type, "user") {
					return false, nil
				}
			}
		}
	}

	rawProcess, ok := top["process"]
	if !ok {
		return false, nil // 没有 process 就没有 capabilities，空集不可能覆盖掩码
	}
	// process 用 RawMessage 的 map 过一遍：只替换 capabilities 一项，其余同级
	// 字段（args/env/cwd/user/terminal…）原字节保留。曾经在这里把整个 process
	// 反序列化进自己的结构体再写回，结果 cwd/args 全丢，runc 报
	// "Cwd property must not be empty" —— 改这块必须逐个字段地动。
	var processFields map[string]json.RawMessage
	if err := json.Unmarshal(rawProcess, &processFields); err != nil {
		log("%s 的 process 解析失败（%v），能力位没检查，原样转发", configPath, err)
		return false, nil
	}
	changed, err := guardProcessFields(processFields, mode, log)
	if err != nil || !changed {
		return changed, err
	}
	encodedProcess, err := json.Marshal(processFields)
	if err != nil {
		return false, fmt.Errorf("序列化 %s 的 process：%w", configPath, err)
	}
	top["process"] = encodedProcess

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return false, fmt.Errorf("序列化 %s：%w", configPath, err)
	}
	out = append(out, '\n')
	if err := writeFileAtomic(configPath, out); err != nil {
		return false, fmt.Errorf("写 %s：%w", configPath, err)
	}
	return true, nil
}

// guardProcessFields 是能力位判定的本体，直接改传进来的 process 对象字段表。
//
// 两个调用方共用它：
//
//	create/run —— bundle 的 config.json 里 process 那一块；
//	exec       —— containerd 给 `runc exec --process <file>` 的那份 **Process**
//	              JSON（docker exec 的 caps 在这里，不在 bundle 里）。
func guardProcessFields(processFields map[string]json.RawMessage, mode string,
	log func(string, ...any)) (bool, error) {
	rawCapabilities, ok := processFields["capabilities"]
	if !ok {
		return false, nil
	}
	var capabilities ociCapabilities
	if err := json.Unmarshal(rawCapabilities, &capabilities); err != nil {
		log("process.capabilities 解析失败（%v），能力位没检查，原样转发", err)
		return false, nil
	}

	// 容器里任何进程能达到的有效能力上限 = permitted ∩ bounding。
	reachable := capabilitiesFromNames(capabilities.Permitted) &
		capabilitiesFromNames(capabilities.Bounding)
	if !coversPrivilegedMask(reachable) {
		return false, nil
	}

	if mode == capGuardModeDeny {
		return false, fmt.Errorf(
			"容器请求了全部能力位（等价 --privileged / --cap-add=ALL）：Ascend 驱动"+
				"会把这类容器判成 admin，给它的 mount namespace 建出全量 UDA 设备表，"+
				"NPU 沙盒隔离会失效。请去掉 --privileged/--cap-add=ALL，只加真正需要的"+
				"能力位；需要保留全量时把 NEU_BOX_CAP_GUARD 设成 %s（我们会移除 %s）",
			capGuardModeDrop, capGuardDropCapability)
	}

	if !dropFromCapabilities(&capabilities, capGuardDropCapability) {
		// 掩码判定通过却找不到这一位：说明我们对 spec 的理解和驱动不一致，
		// 这时候不能装作没事放过去。
		return false, fmt.Errorf(
			"容器能力集覆盖了驱动 admin 掩码，但 %s 不在 spec 里，拒绝创建以免隔离失效",
			capGuardDropCapability)
	}
	encodedCapabilities, err := json.Marshal(capabilities)
	if err != nil {
		return false, fmt.Errorf("序列化 process.capabilities：%w", err)
	}
	processFields["capabilities"] = encodedCapabilities
	return true, nil
}

// execProcessFile 从 `runc exec` 的 argv 里取 `--process <file>` 的路径。
//
// docker exec 的能力位在这份 Process JSON 里：containerd 把 OCI Process 写到文件，
// 再用 `runc exec --process <file>` 交给 runtime。取不到就返回空串，调用方原样转发。
func execProcessFile(args []string) string {
	for index, arg := range args {
		if arg == "--process" {
			if index+1 < len(args) {
				return args[index+1]
			}
			return ""
		}
		if value, ok := strings.CutPrefix(arg, "--process="); ok {
			return value
		}
	}
	return ""
}

// applyCapGuardToExecProcess 对 `docker exec` 那份 Process JSON 做同样的剪位。
//
// 为什么必须管这条：容器 init（entrypoint）的 spec 我们剪了，但 **exec 出来的
// 进程的能力位是 docker/containerd 按容器自己的 HostConfig 现算的**，跟我们改过
// 的那份 spec 无关 —— 实测 `docker exec` 读到的 CapEff 仍是全量。要是这个 exec
// 进程是该 mount namespace 里第一个初始化 NPU 的，驱动照样会建出全量表。
//
// 与 create 的差别：exec 的 Process 里没有 user namespace 信息（那是容器级属性），
// 所以这里不做 userns 判断 —— 反正多剪一位是无害的。
func applyCapGuardToExecProcess(processPath, mode string,
	log func(string, ...any)) (bool, error) {
	if mode == "" {
		mode = capGuardModeDrop
	}
	if mode == capGuardModeOff || processPath == "" {
		return false, nil
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	raw, err := os.ReadFile(processPath)
	if err != nil {
		// containerd 也可能用 stdin 传（`--process -`），读不到就原样转发。
		return false, nil
	}
	var processFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &processFields); err != nil {
		log("解析 %s 失败（%v），能力位没检查，原样转发", processPath, err)
		return false, nil
	}
	changed, err := guardProcessFields(processFields, mode, log)
	if err != nil || !changed {
		return changed, err
	}
	out, err := json.MarshalIndent(processFields, "", "  ")
	if err != nil {
		return false, fmt.Errorf("序列化 %s：%w", processPath, err)
	}
	out = append(out, '\n')
	if err := writeFileAtomic(processPath, out); err != nil {
		return false, fmt.Errorf("写 %s：%w", processPath, err)
	}
	return true, nil
}
