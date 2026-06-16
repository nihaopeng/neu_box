#!/bin/bash
# eBPF CGROUP_DEVICE 沙盒 — 设备独占 + CPU 限制 + 内存限制
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
MAP_RESERVED_PIN="${MAP_DIR}/reserved_devices"
MAP_MAJORS_PIN="${MAP_DIR}/reserved_majors"

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

_cg_id() {
    stat -c %i "$(_cg "$1")" 2>/dev/null
}

# little-endian hex 构建
_u64_hex() {
    local v="$1"
    printf '%02x %02x %02x %02x %02x %02x %02x %02x' \
        $(( v & 0xFF )) $(( (v >> 8) & 0xFF )) \
        $(( (v >> 16) & 0xFF )) $(( (v >> 24) & 0xFF )) \
        $(( (v >> 32) & 0xFF )) $(( (v >> 40) & 0xFF )) \
        $(( (v >> 48) & 0xFF )) $(( (v >> 56) & 0xFF ))
}

_u32_hex() {
    local v="$1"
    printf '%02x %02x %02x %02x' \
        $(( v & 0xFF )) $(( (v >> 8) & 0xFF )) \
        $(( (v >> 16) & 0xFF )) $(( (v >> 24) & 0xFF ))
}

# 从 root cgroup 分离所有 device_reserve BPF 程序
_bpf_detach() {
    echo "[bpf] 分离 root cgroup 上的 BPF 程序 ..."
    bpftool cgroup show "$CGROUP_ROOT" 2>/dev/null | \
        awk '/device_reserve/{print $1}' | \
        while read id; do
            bpftool cgroup detach "$CGROUP_ROOT" cgroup_device id "$id" 2>/dev/null || true
        done
    echo "[bpf] ✓ 已分离"
}

_bpf_unload() {
    echo "[bpf] 卸载 BPF 程序及 maps ..."
    rm -rf "$BPF_PIN" "$MAP_DIR" 2>/dev/null || true
    echo "[bpf] ✓ 已卸载"
}

_bpf_is_loaded() {
    [ -f "$BPF_PIN" ] && [ -f "$MAP_RESERVED_PIN" ] && [ -f "$MAP_MAJORS_PIN" ]
}

_bpf_is_attached() {
    bpftool cgroup show "$CGROUP_ROOT" 2>/dev/null | grep -q "device_reserve"
}

# 确保 BPF 程序已加载并挂载到 root cgroup（仅首次执行，后续 create 跳过）
_ensure_bpf_ready() {
    # 已加载且已挂载 → 直接返回
    if _bpf_is_loaded && _bpf_is_attached; then
        echo "[bpf] BPF 程序已就绪，跳过加载"
        return
    fi

    # 已加载但未挂载 → 仅重新挂载
    if _bpf_is_loaded && ! _bpf_is_attached; then
        echo "[bpf] BPF 程序已加载但未挂载，重新挂载..."
        bpftool cgroup attach "$CGROUP_ROOT" cgroup_device \
            pinned "$BPF_PIN" multi || die "BPF 挂载到 root cgroup 失败"
        echo "[bpf] ✓ 已挂载"
        return
    fi

    # 首次加载：编译 + 加载 + 挂载
    echo "[bpf] 首次加载，编译 $BPF_SRC ..."
    clang -O2 -g -target bpf -c "$BPF_SRC" -o "$BPF_OBJ" || die "BPF 编译失败"
    echo "[bpf] ✓ 编译完成"

    echo "[bpf] 加载 BPF 程序 ..."
    mkdir -p "$MAP_DIR"
    bpftool prog load "$BPF_OBJ" "$BPF_PIN" pinmaps "$MAP_DIR"
    echo "[bpf] ✓ 已加载，maps:"
    ls -l "$MAP_DIR/"

    echo "[bpf] 挂载 BPF 到 root cgroup ..."
    bpftool cgroup attach "$CGROUP_ROOT" cgroup_device \
        pinned "$BPF_PIN" multi || die "BPF 挂载到 root cgroup 失败"
    echo "[bpf] ✓ 已挂载"
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
    bpftool cgroup attach "$CGROUP_ROOT" cgroup_device \
        pinned "$BPF_PIN" multi || die "BPF 挂载失败"
    echo "✓ 已挂载到 root cgroup"
}

cmd_create() {
    local name="$1"; [ -n "$name" ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    local cpu="$2";  [ -n "$cpu"  ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    local mem="$3";  [ -n "$mem"  ] || die "用法: $0 create <name> <cpu> <mem> [major:minor ...]"
    shift 3

    local mem_bytes; mem_bytes=$(parse_mem "$mem")
    [ ! -d "$(_cg "$name")" ] || die "沙盒 '${name}' 已存在"

    echo "cgroup 版本: v${CGROUP_VER}"

    # 创建 cgroup + 设置资源
    _cg_enable
    _cg_mkdir "$name"
    _cg_set_cpu "$name" "$cpu"
    _cg_set_mem "$name" "$mem_bytes"

    # 获取 cgroup ID（内核级唯一标识，与 bpf_get_current_cgroup_id 对应）
    local cgid; cgid=$(_cg_id "$name")
    [ -n "$cgid" ] || die "无法获取 cgroup ID"

    [ "$cpu" != "0" ] && echo "  CPU: ${cpu} 核"     || echo "  CPU: 不限"
    [ "$mem_bytes" != "0" ] && echo "  内存: ${mem}" || echo "  内存: 不限"
    echo "  cgroup ID: $cgid"

    # eBPF 设备预留（挂在 root cgroup，全局生效）
    _ensure_bpf_ready

    local device_file="/tmp/neu_box_devices_${name}"
    rm -f "$device_file"

    if [ $# -gt 0 ]; then
        for dev in "$@"; do
            local major minor; read -r major minor <<< "$(parse_device "$dev")"
            local minor_num
            if [ "$minor" = "*" ]; then
                minor_num=4294967295  # 0xFFFFFFFF
            else
                minor_num=$minor
            fi
            
            # 使用标准的 _u32_hex 函数，自动处理好 4字节 Little-Endian
            local key_hex val_hex
            key_hex="$(_u32_hex "$major") $(_u32_hex "$minor_num")"
            val_hex=$(_u64_hex "$cgid")
            
            bpftool map update pinned "$MAP_RESERVED_PIN" \
                key hex $key_hex value hex $val_hex \
                || die "设备预留失败: $dev"
            echo "$dev" >> "$device_file"
        done
        echo "  独占设备: $*"
        # 标记该 cgroup 在对应 major 有预留
        # 去重: 只记录每个 major 一次
        local seen_majors=""
        for dev in "$@"; do
            local mj mn; read -r mj mn <<< "$(parse_device "$dev")"
            if [[ " $seen_majors " != *" $mj "* ]]; then
                bpftool map update pinned "$MAP_MAJORS_PIN" \
                    key hex $(_u64_hex "$cgid") $(_u32_hex "$mj") 00 00 00 00 value hex 01 \
                    || die "标记 reserved_majors 失败 (major=$mj)"
                seen_majors="$seen_majors $mj"
            fi
        done
    else
        echo "  设备: 全共享（未预留任何设备）"
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

    echo "--- 设备预留 ---"
    echo "  [reserved_devices]"
    bpftool map dump pinned "$MAP_RESERVED_PIN" 2>/dev/null || echo "    (无)"
    echo "  [reserving_cgroups]"
    bpftool map dump pinned "$MAP_MAJORS_PIN" 2>/dev/null || echo "    (无)"
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

    # 从 reserved_devices / reserved_majors 删除该沙盒预留的所有设备
    local cgid; cgid=$(_cg_id "$name")
    local device_file="/tmp/neu_box_devices_${name}"
    if [ -f "$device_file" ] && [ -n "$cgid" ]; then
        local seen_majors=""
        while read dev; do
            [ -z "$dev" ] && continue
            local major minor; read -r major minor <<< "$(parse_device "$dev")"
            local minor_num
            if [ "$minor" = "*" ]; then
                minor_num=4294967295
            else
                minor_num=$minor
            fi
            # 删除精确设备条目
            local key_hex
            key_hex=$(printf '%02x %02x %02x %02x %02x %02x %02x %02x' \
                $(( major & 0xFF )) $(( (major >> 8) & 0xFF )) 0 0 \
                $(( minor_num & 0xFF )) $(( (minor_num >> 8) & 0xFF )) 0 0)
            bpftool map delete pinned "$MAP_RESERVED_PIN" \
                key hex $key_hex 2>/dev/null || true
            # 去重后删除 major 条目
            if [[ " $seen_majors " != *" $major "* ]]; then
                bpftool map delete pinned "$MAP_MAJORS_PIN" \
                    key hex $(_u64_hex "$cgid") $(_u32_hex "$major") 00 00 00 00 2>/dev/null || true
                seen_majors="$seen_majors $major"
            fi
        done < "$device_file"
        rm -f "$device_file"
    fi

    _cg_rmdir "$name"

    # BPF 程序保持在 root cgroup 上，不卸载（其他沙盒可能仍在使用）
    echo "✓ 沙盒 '${name}' 已销毁"
}

cmd_cleanup() {
    echo "=== 完全清理 ==="
    for d in "${CGROUP_ROOT}"/${PREFIX}*/; do
        [ -d "$d" ] || continue
        local name="${d##*/${PREFIX}}"
        name="${name%/}"
        echo "销毁沙盒: $name"
        # 直接清理 cgroup 和 map 条目，不走 cmd_destroy 避免重复 detach
        _cg_kickall "$name" 2>/dev/null || true
        _cg_rmdir "$name" 2>/dev/null || true
    done

    # 分离 BPF 程序
    _bpf_detach

    # 卸载 BPF 程序及 maps
    _bpf_unload

    rm -f /tmp/neu_box_devices_* 2>/dev/null || true
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
