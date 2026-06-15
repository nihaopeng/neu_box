#!/bin/bash
# cgroup v1 沙盒 — 设备黑名单 + CPU 限制 + 内存限制
#
# ===== 用法 =====
#
#   sudo ./sandbox.sh create <name> <cpu> <mem> [major:minor ...]
#   sudo ./sandbox.sh join    <name> <PID>
#   sudo ./sandbox.sh status  <name>
#   sudo ./sandbox.sh destroy <name>
#
#   cpu: CPU 核数, 0 = 不限
#   mem: 内存, 如 512M / 2G / 0 (0 = 不限)
#
# ===== 示例 =====
#
#   # 2 核 CPU, 4G 内存, 阻止所有 NPU
#   sudo ./sandbox.sh create test1 2 4G 235:* 236:*
#
#   # 1 核 CPU, 1G 内存, 不阻止设备
#   sudo ./sandbox.sh create test2 1 1G
#
#   # 不限 CPU/内存, 只阻止 GPU
#   sudo ./sandbox.sh create test3 0 0 195:* 226:*
#
# ===== 常见设备号 =====
#   195:*  NVIDIA GPU      226:*  DRM GPU
#   235:*  Ascend NPU      236:*  Ascend Manager

set -e

# cgroup v1 各控制器挂载点
CGROUP_BASE="/sys/fs/cgroup"
CGROUP_CPU="${CGROUP_BASE}/cpu"
CGROUP_MEM="${CGROUP_BASE}/memory"
CGROUP_DEV="${CGROUP_BASE}/devices"

PREFIX="sandbox_"

die() { echo "错误: $*" >&2; exit 1; }

# ── 内存解析: 512M / 2G → 字节 ──────────────────────────────────

parse_mem() {
    local raw="$1"
    [ "$raw" = "0" ] && { echo "0"; return; }
    [ -z "$raw" ] && { echo "0"; return; }

    local num="${raw//[^0-9]/}"
    local unit="${raw//[0-9]/}"
    case "$unit" in
        K|k) echo $(( num * 1024 )) ;;
        M|m) echo $(( num * 1024 * 1024 )) ;;
        G|g) echo $(( num * 1024 * 1024 * 1024 )) ;;
        *)   die "无法识别的内存单位: $raw (支持 K/M/G)" ;;
    esac
}

# ── create ──────────────────────────────────────────────────────

cmd_create() {
    local name="$1";  [ -n "$name" ] || die "用法: $0 create <name> <cpu> <mem> [device ...]"
    local cpu="$2";   [ -n "$cpu"  ] || die "用法: $0 create <name> <cpu> <mem> [device ...]"
    local mem="$3";   [ -n "$mem"  ] || die "用法: $0 create <name> <cpu> <mem> [device ...]"
    shift 3  # 剩余参数都是设备号

    local mem_bytes
    mem_bytes=$(parse_mem "$mem")

    # --- 创建 cgroup 目录 ---
    local cpupath="${CGROUP_CPU}/${PREFIX}${name}"
    local mempath="${CGROUP_MEM}/${PREFIX}${name}"
    local devpath="${CGROUP_DEV}/${PREFIX}${name}"

    [ ! -d "$cpupath" ] || die "沙盒 '${name}' 已存在"
    mkdir -p "$cpupath" "$mempath" "$devpath"

    # --- CPU 限制 ---
    if [ "$cpu" != "0" ] && [ -n "$cpu" ]; then
        echo 100000 > "${cpupath}/cpu.cfs_period_us"
        echo $(( cpu * 100000 )) > "${cpupath}/cpu.cfs_quota_us"
        echo "  CPU: ${cpu} 核 (quota=$(( cpu * 100000 ))/period=100000)"
    else
        echo "  CPU: 不限"
    fi

    # --- 内存限制 ---
    if [ "$mem_bytes" != "0" ] && [ -n "$mem_bytes" ]; then
        echo "$mem_bytes" > "${mempath}/memory.limit_in_bytes"
        # 禁用 swap 以严格限制
        echo 0 > "${mempath}/memory.swappiness" 2>/dev/null || true
        echo "  内存: ${mem} (${mem_bytes} bytes)"
    else
        echo "  内存: 不限"
    fi

    # --- 设备黑名单 ---
    if [ $# -gt 0 ]; then
        for dev in "$@"; do
            echo "c ${dev} rwm" > "${devpath}/devices.deny"
        done
        echo "  设备黑名单: $*"
    else
        echo "  设备: 全放行（无黑名单）"
    fi

    echo "✓ 沙盒已创建: ${CGROUP_BASE}/*/${PREFIX}${name}"
    echo "  加入进程: sudo $0 join $name <PID>"
}

# ── join ────────────────────────────────────────────────────────

cmd_join() {
    local name="$1" pid="$2"
    [ -n "$name" ] || die "用法: $0 join <name> <PID>"
    [ -n "$pid"  ] || die "用法: $0 join <name> <PID>"
    [ -d "/proc/$pid" ] || die "PID $pid 不存在"

    local cpupath="${CGROUP_CPU}/${PREFIX}${name}"
    local mempath="${CGROUP_MEM}/${PREFIX}${name}"
    local devpath="${CGROUP_DEV}/${PREFIX}${name}"

    [ -d "$cpupath" ] || die "沙盒 '${name}' 不存在"

    echo "$pid" > "${cpupath}/cgroup.procs"
    echo "$pid" > "${mempath}/cgroup.procs"
    echo "$pid" > "${devpath}/cgroup.procs"
    echo "✓ PID $pid 已加入沙盒 '${name}' (cpu + memory + devices)"
}

# ── status ──────────────────────────────────────────────────────

cmd_status() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 status <name>"
    local cpupath="${CGROUP_CPU}/${PREFIX}${name}"
    [ -d "$cpupath" ] || die "沙盒 '${name}' 不存在"

    echo "=== 沙盒: $name ==="
    echo ""
    echo "--- CPU ---"
    echo "  quota:  $(cat ${cpupath}/cpu.cfs_quota_us 2>/dev/null || echo '-')"
    echo "  period: $(cat ${cpupath}/cpu.cfs_period_us 2>/dev/null || echo '-')"
    echo ""
    echo "--- 内存 ---"
    local mempath="${CGROUP_MEM}/${PREFIX}${name}"
    local limit
    limit=$(cat "${mempath}/memory.limit_in_bytes" 2>/dev/null || echo '-')
    if [ "$limit" = "9223372036854771712" ] || [ "$limit" = "-1" ]; then
        echo "  limit:  不限"
    else
        echo "  limit:  $limit bytes ($(( limit / 1024 / 1024 ))M)"
    fi
    local usage
    usage=$(cat "${mempath}/memory.usage_in_bytes" 2>/dev/null || echo '-')
    echo "  usage:  ${usage} bytes"
    echo ""
    echo "--- 设备黑名单 ---"
    local devpath="${CGROUP_DEV}/${PREFIX}${name}"
    cat "${devpath}/devices.list" 2>/dev/null | grep -v '^a ' || echo "  (空)"
    echo ""
    echo "--- 进程列表 ---"
    local procs
    procs=$(cat "${cpupath}/cgroup.procs" 2>/dev/null)
    if [ -z "$procs" ]; then
        echo "  (空)"
    else
        for p in $procs; do
            if [ -f "/proc/$p/cmdline" ]; then
                printf "  PID %-8s %s\n" "$p" "$(tr '\0' ' ' < "/proc/$p/cmdline")"
            else
                echo "  PID $p  (已退出)"
            fi
        done
    fi
}

# ── destroy ─────────────────────────────────────────────────────

cmd_destroy() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 destroy <name>"
    local cpupath="${CGROUP_CPU}/${PREFIX}${name}"
    [ -d "$cpupath" ] || die "沙盒 '${name}' 不存在"

    # 踢出所有进程
    for cg in "$cpupath" "${CGROUP_MEM}/${PREFIX}${name}" "${CGROUP_DEV}/${PREFIX}${name}"; do
        local procs
        procs=$(cat "${cg}/cgroup.procs" 2>/dev/null || true)
        for p in $procs; do
            echo "$p" > "$(dirname "$cg")/cgroup.procs" 2>/dev/null || true
        done
        rmdir "$cg" 2>/dev/null || true
    done

    echo "✓ 沙盒 '${name}' 已销毁"
}

# ── main ───────────────────────────────────────────────────────

case "${1:-}" in
    create)
        shift
        cmd_create "$@"
        ;;
    join)    cmd_join    "${2:-}" "${3:-}" ;;
    status)  cmd_status  "${2:-}" ;;
    destroy) cmd_destroy "${2:-}" ;;
    *)
        echo "用法: $0 {create|join|status|destroy} <name> <cpu> <mem> [device ...]"
        echo ""
        echo "  create  <name> <cpu> <mem> [major:minor ...]"
        echo "          cpu  = CPU 核数 (0=不限)"
        echo "          mem  = 内存大小 (如 512M, 2G, 0=不限)"
        echo "  join    <name> <PID>"
        echo "  status  <name>"
        echo "  destroy <name>"
        echo ""
        echo "  示例:"
        echo "    $0 create gpu_box  2 4G 195:* 226:*"
        echo "    $0 create npu_box  1 1G 235:* 236:*"
        echo "    $0 create basic    1 512M"
        echo "    $0 create unlimited 0 0 235:*"
        ;;
esac
