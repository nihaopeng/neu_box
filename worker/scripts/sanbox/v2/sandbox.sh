#!/bin/bash
# eBPF CGROUP_DEVICE 沙盒 — 设备黑名单 + CPU 限制 + 内存限制
#
# 依赖: clang, bpftool
#
# ===== 用法 =====
#
#   sudo ./sandbox.sh compile
#   sudo ./sandbox.sh load
#   sudo ./sandbox.sh create <name> <cpu> <mem> [major ...]
#   sudo ./sandbox.sh join   <name> <PID>
#   sudo ./sandbox.sh status <name>
#   sudo ./sandbox.sh destroy <name>
#   sudo ./sandbox.sh cleanup
#
#   cpu: CPU 核数, 0 = 不限
#   mem: 内存, 如 512M / 2G / 0 (=不限)
#
# ===== 快速测试 =====
#
#   sudo ./sandbox.sh compile
#   sudo ./sandbox.sh load
#   sudo ./sandbox.sh create test1 2 4G 235 236
#   sudo ./sandbox.sh join test1 $$
#   cat /dev/davinci0        # → Permission denied
#   echo ok > /dev/null      # → 正常
#   sudo ./sandbox.sh destroy test1

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BPF_SRC="${SCRIPT_DIR}/device_block.bpf.c"
BPF_OBJ="${SCRIPT_DIR}/device_block.o"
# BPF map pin 路径
BPF_PIN="/sys/fs/bpf/device_block"
MAP_DEV_PIN="/sys/fs/bpf/device_block_devices"   # HASH: exact major:minor
MAP_MAJ_PIN="/sys/fs/bpf/device_block_majors"     # ARRAY: major:* wildcard

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

# ── 设备号解析: 235:0 → major=235 minor=0; 235:* → major=235 minor=* ─

parse_device() {
    local dev="$1"
    if [[ "$dev" == *:* ]]; then
        echo "${dev%%:*}" "${dev##*:}"
    else
        # 裸数字等同于 major:*
        echo "$dev" "*"
    fi
}

# ── 自动确保 BPF 已编译并加载（create 前自动调用）────────────

_ensure_bpf_ready() {
    # 自动编译（如果 .o 不存在或比 .c 旧）
    if [ ! -f "$BPF_OBJ" ] || [ "$BPF_SRC" -nt "$BPF_OBJ" ]; then
        echo "[auto] 编译 $BPF_SRC ..."
        clang -O2 -g -target bpf -c "$BPF_SRC" -o "$BPF_OBJ"
        echo "[auto] ✓ 编译完成"
    fi

    # 自动加载（如果未 pin）
    if ! bpftool prog show pinned "$BPF_PIN" &>/dev/null; then
        echo "[auto] 加载 BPF 程序到内核 ..."
        bpftool prog load "$BPF_OBJ" "$BPF_PIN"
        echo "[auto] ✓ BPF 程序已加载: $BPF_PIN"
    fi
}

# ── compile (手动) ──────────────────────────────────────────────

cmd_compile() {
    echo "=== 编译 $BPF_SRC ==="
    clang -O2 -g -target bpf -c "$BPF_SRC" -o "$BPF_OBJ"
    echo "✓ 编译完成: $BPF_OBJ"
}

# ── load (手动) ─────────────────────────────────────────────────

cmd_load() {
    [ -f "$BPF_OBJ" ] || die "先执行 compile: $0 compile"
    bpftool prog load "$BPF_OBJ" "$BPF_PIN"
    echo "✓ BPF 程序已加载: $BPF_PIN"
}

# ── create ──────────────────────────────────────────────────────

cmd_create() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 create <name> <cpu> <mem> [major ...]"
    local cpu="$2";  [ -n "$cpu"  ] || die "用法: $0 create <name> <cpu> <mem> [major ...]"
    local mem="$3";  [ -n "$mem"  ] || die "用法: $0 create <name> <cpu> <mem> [major ...]"
    shift 3

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
        echo "  CPU: ${cpu} 核"
    else
        echo "  CPU: 不限"
    fi

    # --- 内存限制 ---
    if [ "$mem_bytes" != "0" ] && [ -n "$mem_bytes" ]; then
        echo "$mem_bytes" > "${mempath}/memory.limit_in_bytes"
        echo 0 > "${mempath}/memory.swappiness" 2>/dev/null || true
        echo "  内存: ${mem}"
    else
        echo "  内存: 不限"
    fi

    # --- eBPF 设备控制（自动编译+加载）---
    _ensure_bpf_ready
    bpftool cgroup attach "$devpath" cgroup_device \
        pinned "$BPF_PIN" multi 2>/dev/null \
        || bpftool cgroup attach "$devpath" cgroup/dev \
            pinned "$BPF_PIN" multi 2>/dev/null \
        || die "BPF 附着失败 — 系统可能无 cgroup v2，请用 v1/sandbox.sh"

    # 填充黑名单
    #   235:0 → HASH map (仅 block /dev/davinci0)
    #   235:* → ARRAY map (block 所有 /dev/davinci*)
    if [ $# -gt 0 ]; then
        for dev in "$@"; do
            local major minor
            read -r major minor <<< "$(parse_device "$dev")"
            if [ "$minor" = "*" ]; then
                bpftool map update pinned "$MAP_MAJ_PIN" \
                    key "$major" 0 0 0 value 1 0 0 0 2>/dev/null || true
            else
                # struct dev_key { u32 major; u32 minor } → 8 bytes LE
                local key_hex
                key_hex=$(printf '%02x %02x %02x %02x %02x %02x %02x %02x' \
                    $(( major & 0xFF )) $(( (major >> 8) & 0xFF )) 0 0 \
                    $(( minor & 0xFF )) $(( (minor >> 8) & 0xFF )) 0 0)
                bpftool map update pinned "$MAP_DEV_PIN" \
                    key hex $key_hex value 1 0 0 0 2>/dev/null || true
            fi
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
    [ -d "$cpupath" ] || die "沙盒 '${name}' 不存在"

    echo "$pid" > "${cpupath}/cgroup.procs"
    echo "$pid" > "${CGROUP_MEM}/${PREFIX}${name}/cgroup.procs"
    echo "$pid" > "${CGROUP_DEV}/${PREFIX}${name}/cgroup.procs"
    echo "✓ PID $pid 已加入沙盒 '${name}' (cpu + memory + eBPF devices)"
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
    echo "  usage:  $(cat ${mempath}/memory.usage_in_bytes 2>/dev/null || echo '-') bytes"
    echo ""
    echo "--- eBPF 设备黑名单 (通配 major:*) ---"
    bpftool map dump pinned "$MAP_MAJ_PIN" 2>/dev/null | grep -B1 '"value": 1' \
        | grep '"key"' | sed 's/.*"key": //; s/,//' | while read major; do
        echo "  ${major}:*"
    done
    echo ""
    echo "--- eBPF 设备黑名单 (精确 major:minor) ---"
    bpftool map dump pinned "$MAP_DEV_PIN" 2>/dev/null | grep -B1 '"value": 1' \
        | grep '"key"' | sed 's/.*"key": //' | while read entry; do
        # entry looks like "235, 1" or similar
        local maj min
        maj=$(echo "$entry" | awk -F'[, ]+' '{print $1}')
        min=$(echo "$entry" | awk -F'[, ]+' '{print $2}')
        echo "  ${maj}:${min}"
    done
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

    local devpath="${CGROUP_DEV}/${PREFIX}${name}"

    # 踢出所有进程
    for cg in "$cpupath" "${CGROUP_MEM}/${PREFIX}${name}" "$devpath"; do
        local procs
        procs=$(cat "${cg}/cgroup.procs" 2>/dev/null || true)
        for p in $procs; do
            echo "$p" > "$(dirname "$cg")/cgroup.procs" 2>/dev/null || true
        done
    done

    # detach BPF
    bpftool cgroup detach "$devpath" cgroup_device 2>/dev/null || true
    bpftool cgroup detach "$devpath" cgroup/dev 2>/dev/null || true

    # 删除 cgroup 目录
    rmdir "$cpupath" 2>/dev/null || true
    rmdir "${CGROUP_MEM}/${PREFIX}${name}" 2>/dev/null || true
    rmdir "$devpath" 2>/dev/null || die "无法删除 $devpath（可能仍有进程残留）"
    echo "✓ 沙盒 '${name}' 已销毁"
}

# ── cleanup ─────────────────────────────────────────────────────

cmd_cleanup() {
    echo "=== 完全清理 ==="
    for d in "${CGROUP_CPU}/${PREFIX}"*/; do
        [ -d "$d" ] || continue
        local name="${d##*/${PREFIX}}"
        echo "销毁沙盒: $name"
        cmd_destroy "$name" 2>/dev/null || true
    done
    rm -f "$BPF_PIN" "$MAP_DEV_PIN" "$MAP_MAJ_PIN" 2>/dev/null || true
    echo "✓ 清理完成"
}

# ── main ───────────────────────────────────────────────────────

case "${1:-}" in
    compile) cmd_compile ;;
    load)    cmd_load ;;
    create)
        shift
        cmd_create "$@"
        ;;
    join)    cmd_join    "${2:-}" "${3:-}" ;;
    status)  cmd_status  "${2:-}" ;;
    destroy) cmd_destroy "${2:-}" ;;
    cleanup) cmd_cleanup ;;
    *)
        echo "用法: $0 {compile|load|create|join|status|destroy|cleanup} [args...]"
        echo ""
        echo "  compile                                 编译 BPF 程序（可选，create 自动处理）"
        echo "  load                                    加载 BPF 到内核（可选，create 自动处理）"
        echo "  create <name> <cpu> <mem> [major ...]   创建沙盒"
        echo "          cpu  = CPU 核数 (0=不限)"
        echo "          mem  = 内存 (如 512M, 2G, 0=不限)"
        echo "          major = 要阻止的设备 major 号 (如 235 226)"
        echo "  join    <name> <PID>                    将进程加入沙盒"
        echo "  status  <name>                          查看沙盒状态"
        echo "  destroy <name>                          销毁沙盒"
        echo "  cleanup                                 清理所有沙盒并卸载 BPF"
        echo ""
        echo "  示例:"
        echo "    $0 create npu_box 2 4G 235 236"
        echo "    $0 create gpu_box 1 2G 195 226"
        echo "    $0 create basic   1 1G"
        echo "    $0 create unlimited 0 0 235"
        ;;
esac
