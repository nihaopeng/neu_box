#!/bin/bash
# eBPF CGROUP_DEVICE 沙盒 — 设备黑名单 + CPU 限制 + 内存限制
# cgroup v2 only
#
# 依赖: clang, bpftool
#
# ===== 用法 =====
#
#   sudo ./sandbox.sh create <name> <cpu> <mem> [major:minor ...]
#   sudo ./sandbox.sh join   <name> <PID>
#   sudo ./sandbox.sh status <name>
#   sudo ./sandbox.sh destroy <name>
#   sudo ./sandbox.sh cleanup
#
#   cpu: 核数 (0=不限)    mem: 如 512M / 2G / 0
#   设备号: major:minor (如 235:0, 235:*, 235)

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BPF_SRC="${SCRIPT_DIR}/device_block.bpf.c"
BPF_OBJ="${SCRIPT_DIR}/device_block.o"
BPF_PIN="/sys/fs/bpf/device_block"
MAP_DIR="/sys/fs/bpf/sandbox_maps"
MAP_DEV_PIN="${MAP_DIR}/blocked_devices"
MAP_MAJ_PIN="${MAP_DIR}/blocked_majors"

CGROUP_ROOT="/sys/fs/cgroup"
PREFIX="sandbox_"

die() { echo "错误: $*" >&2; exit 1; }

# ==================================================================
# cgroup v2 路径函数
# ==================================================================
CGROUP_VER=2

_cg()        { echo "${CGROUP_ROOT}/${PREFIX}${1}"; }
_cg_cpu()    { echo "${CGROUP_ROOT}/${PREFIX}${1}/cpu.max"; }
_cg_mem()    { echo "${CGROUP_ROOT}/${PREFIX}${1}/memory.max"; }
_cg_memcur() { echo "${CGROUP_ROOT}/${PREFIX}${1}/memory.current"; }
_cg_swap()   { echo "${CGROUP_ROOT}/${PREFIX}${1}/memory.swap.max"; }
_cg_procs()  { echo "${CGROUP_ROOT}/${PREFIX}${1}/cgroup.procs"; }
_cg_dev()    { echo "${CGROUP_ROOT}/${PREFIX}${1}"; }
_cg_root_procs() { echo "${CGROUP_ROOT}/cgroup.procs"; }
_cg_enable() {
    echo "+cpu +memory" > "${CGROUP_ROOT}/cgroup.subtree_control" 2>/dev/null || true
}
_cg_mkdir() {
    local cg=$(_cg "$1"); mkdir -p "$cg"
}
_cg_rmdir() {
    local cg=$(_cg "$1"); rmdir "$cg" 2>/dev/null || true
}
_cg_join() {
    local name="$1" pid="$2"
    echo "$pid" > "$(_cg_procs "$name")"
}
_cg_kickall() {
    local name="$1" procs p
    procs=$(cat "$(_cg_procs "$name")" 2>/dev/null || true)
    for p in $procs; do
        echo "$p" > "$(_cg_root_procs)" 2>/dev/null || true
    done
}
_cg_set_cpu() {
    local name="$1" cores="$2"
    [ "$cores" != "0" ] && [ -n "$cores" ] \
        && echo "$(( cores * 100000 )) 100000" > "$(_cg_cpu "$name")" || true
}
_cg_set_mem() {
    local name="$1" bytes="$2"
    [ "$bytes" != "0" ] && [ -n "$bytes" ] \
        && echo "$bytes" > "$(_cg_mem "$name")" \
        && echo 0 > "$(_cg_swap "$name")" 2>/dev/null || true
}

# ==================================================================
# 工具函数
# ==================================================================

parse_mem() {
    local raw="$1"
    [ "$raw" = "0" ] && { echo "0"; return; }
    [ -z "$raw" ] && { echo "0"; return; }
    local num="${raw//[^0-9]/}" unit="${raw//[0-9]/}"
    case "$unit" in
        K|k) echo $(( num * 1024 )) ;;
        M|m) echo $(( num * 1024 * 1024 )) ;;
        G|g) echo $(( num * 1024 * 1024 * 1024 )) ;;
        *)   die "无法识别的内存单位: $raw (支持 K/M/G)" ;;
    esac
}

parse_device() {
    local dev="$1"
    if [[ "$dev" == *:* ]]; then
        echo "${dev%%:*}" "${dev##*:}"
    else
        echo "$dev" "*"
    fi
}

_ensure_bpf_ready() {
    echo "[auto] 编译 $BPF_SRC ..."
    clang -O2 -g -target bpf -c "$BPF_SRC" -o "$BPF_OBJ" || die "BPF 编译失败"
    echo "[auto] ✓ 编译完成"
    # 程序未加载，或 maps 没 pin 好 → 重新加载
    if ! bpftool prog show pinned "$BPF_PIN" &>/dev/null \
        || [ ! -f "$MAP_DEV_PIN" ] \
        || [ ! -f "$MAP_MAJ_PIN" ]; then
        echo "[auto] 加载 BPF 程序 ..."
        rm -rf "$BPF_PIN" "$MAP_DIR" 2>/dev/null || true
        mkdir -p "$MAP_DIR"
        bpftool prog load "$BPF_OBJ" "$BPF_PIN" pinmaps "$MAP_DIR"
        echo "[auto] ✓ 已加载，maps:"
        ls -l "$MAP_DIR/"
    fi
}

# ==================================================================
# 命令
# ==================================================================

cmd_compile() {
    clang -O2 -g -target bpf -c "$BPF_SRC" -o "$BPF_OBJ"
    echo "✓ 编译完成: $BPF_OBJ"
}

cmd_load() {
    [ -f "$BPF_OBJ" ] || die "先执行 compile"
    rm -rf "$BPF_PIN" "$MAP_DIR" 2>/dev/null || true
    mkdir -p "$MAP_DIR"
    bpftool prog load "$BPF_OBJ" "$BPF_PIN" pinmaps "$MAP_DIR"
    echo "✓ BPF 程序已加载"
    ls -l "$MAP_DIR/"
}

cmd_create() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    local cpu="$2";  [ -n "$cpu"  ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    local mem="$3";  [ -n "$mem"  ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    shift 3

    local mem_bytes; mem_bytes=$(parse_mem "$mem")
    [ ! -d "$(_cg "$name")" ] || die "沙盒 '${name}' 已存在"

    echo "cgroup 版本: v${CGROUP_VER}"

    # 创建目录 + 设置资源
    _cg_enable
    _cg_mkdir "$name"
    _cg_set_cpu "$name" "$cpu"
    _cg_set_mem "$name" "$mem_bytes"

    [ "$cpu" != "0" ] && echo "  CPU: ${cpu} 核"     || echo "  CPU: 不限"
    [ "$mem_bytes" != "0" ] && echo "  内存: ${mem}" || echo "  内存: 不限"

    # eBPF 设备控制
    _ensure_bpf_ready
    bpftool cgroup attach "$(_cg_dev "$name")" cgroup_device \
        pinned "$BPF_PIN" multi 2>/dev/null \
        || die "BPF 附着失败"

    # 填充黑名单
    if [ $# -gt 0 ]; then
        for dev in "$@"; do
            local major minor; read -r major minor <<< "$(parse_device "$dev")"
            if [ "$minor" = "*" ]; then
                bpftool map update pinned "$MAP_MAJ_PIN" \
                    key "$major" 0 0 0 value 1 2>/dev/null || true
            else
                local key_hex
                key_hex=$(printf '%02x %02x %02x %02x %02x %02x %02x %02x' \
                    $(( major & 0xFF )) $(( (major >> 8) & 0xFF )) 0 0 \
                    $(( minor & 0xFF )) $(( (minor >> 8) & 0xFF )) 0 0)
                bpftool map update pinned "$MAP_DEV_PIN" \
                    key hex $key_hex value 1 2>/dev/null || true
            fi
        done
        echo "  设备黑名单: $*"
    else
        echo "  设备: 全放行"
    fi

    echo "✓ 沙盒已创建"
    echo "  加入进程: sudo $0 join $name <PID>"
}

cmd_join() {
    local name="$1" pid="$2"
    [ -n "$name" ] || die "用法: $0 join <name> <PID>"
    [ -n "$pid"  ] || die "用法: $0 join <name> <PID>"
    [ -d "/proc/$pid" ] || die "PID $pid 不存在"
    [ -d "$(_cg "$name")" ] || die "沙盒 '${name}' 不存在"

    _cg_join "$name" "$pid"
    echo "✓ PID $pid 已加入沙盒 '${name}'"
}

cmd_status() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 status <name>"
    [ -d "$(_cg "$name")" ] || die "沙盒 '${name}' 不存在"

    echo "=== 沙盒: $name ==="
    echo "cgroup: v${CGROUP_VER}"
    echo ""

    echo "--- CPU ---"
    cat "$(_cg_cpu "$name")" 2>/dev/null || echo '-'
    echo ""

    echo "--- 内存 ---"
    local limit; limit=$(cat "$(_cg_mem "$name")" 2>/dev/null || echo '-')
    case "$limit" in
        9223372036854771712|-1|max) echo "  limit:  不限" ;;
        *) echo "  limit:  $limit bytes ($(( limit / 1024 / 1024 ))M)" ;;
    esac
    echo "  usage:  $(cat "$(_cg_memcur "$name")" 2>/dev/null || echo '-') bytes"
    echo ""

    echo "--- eBPF 设备黑名单 ---"
    echo "  [通配 major:*]"
    bpftool map dump pinned "$MAP_MAJ_PIN" 2>/dev/null \
        | grep -B1 '"value": 1' | grep '"key"' \
        | sed 's/.*"key": //; s/,//' \
        | while read major; do echo "    ${major}:*"; done
    echo "  [精确 major:minor]"
    bpftool map dump pinned "$MAP_DEV_PIN" 2>/dev/null \
        | grep -B1 '"value": 1' | grep '"key"' \
        | sed 's/.*"key": //' \
        | while read entry; do
            echo "    $(echo "$entry" | awk -F'[, ]+' '{print $1}'):$(echo "$entry" | awk -F'[, ]+' '{print $2}')"
        done
    echo ""

    echo "--- 进程列表 ---"
    local procs; procs=$(cat "$(_cg_procs "$name")" 2>/dev/null)
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

cmd_destroy() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 destroy <name>"
    [ -d "$(_cg "$name")" ] || die "沙盒 '${name}' 不存在"

    _cg_kickall "$name"
    bpftool cgroup detach "$(_cg_dev "$name")" cgroup_device 2>/dev/null || true
    _cg_rmdir "$name"

    echo "✓ 沙盒 '${name}' 已销毁"
}

cmd_cleanup() {
    echo "=== 完全清理 ==="
    for d in "${CGROUP_ROOT}"/${PREFIX}*/; do
        [ -d "$d" ] || continue
        local name="${d##*/${PREFIX}}"
        name="${name%/}"
        echo "销毁沙盒: $name"
        cmd_destroy "$name" 2>/dev/null || true
    done
    rm -rf "$BPF_PIN" "$MAP_DIR" 2>/dev/null || true
    echo "✓ 清理完成"
}

# ==================================================================
# main
# ==================================================================

case "${1:-}" in
    compile) cmd_compile ;;
    load)    cmd_load ;;
    create)  shift; cmd_create "$@" ;;
    join)    cmd_join    "${2:-}" "${3:-}" ;;
    status)  cmd_status  "${2:-}" ;;
    destroy) cmd_destroy "${2:-}" ;;
    cleanup) cmd_cleanup ;;
    *)
        echo "cgroup 版本: v${CGROUP_VER}"
        echo ""
        echo "用法: $0 {create|join|status|destroy|cleanup} [args...]"
        echo ""
        echo "  create <name> <cpu> <mem> [major:minor ...]"
        echo "          cpu  = CPU 核数 (0=不限)"
        echo "          mem  = 内存 (512M / 2G / 0=不限)"
        echo "          device = 设备号 (如 235:0 / 235:* / 235)"
        echo "  join    <name> <PID>"
        echo "  status  <name>"
        echo "  destroy <name>"
        echo "  cleanup"
        ;;
esac
