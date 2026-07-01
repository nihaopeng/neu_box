#!/bin/bash
# ============================================================
# 脚本: npu_info.sh
# 用途: 采集 Ascend NPU 设备信息
#
# 输入: 无
# 输出 (stdout): JSON
#   {"total": <int>, "idle": <int>, "busy_ids": [<int>, ...]}
#
# 退出码: 始终为 0
#
# 依赖: npu-smi (华为昇腾 Ascend 管理工具)
#
# 空闲判定:
#   npu-smi info 的进程列表中，某芯片下有进程运行 → 非空闲
#   无进程 → 空闲
#
# 示例:
#   $ ./npu_info.sh
#   {"total":8,"idle":8,"busy_ids":[]}
#
#   $ ./npu_info.sh
#   {"total":8,"idle":6,"busy_ids":[0,3]}
# ============================================================

if ! command -v npu-smi &>/dev/null; then
    echo '{"total":0,"idle":0,"busy_ids":[]}'
    exit 0
fi

output=$(npu-smi info 2>/dev/null)
if [ $? -ne 0 ] || [ -z "$output" ]; then
    echo '{"total":0,"idle":0,"busy_ids":[]}'
    exit 0
fi

# ── 统计 NPU 总数 ──
total=0
in_table=0
row_idx=0
while IFS= read -r line; do
    if [[ "$line" =~ ^\+=== ]]; then
        in_table=1; continue
    fi
    [ "$in_table" -eq 0 ] && continue
    [[ "$line" =~ ^\+--- ]] && break
    [[ "$line" =~ ^\| ]] || continue
    [[ "$line" =~ NPU ]] && continue
    [[ "$line" =~ Name ]] && continue
    [[ "$line" =~ Chip ]] && continue
    if [ $((row_idx % 2)) -eq 1 ]; then
        total=$((total + 1))
    fi
    row_idx=$((row_idx + 1))
done <<< "$output"

# ── 收集有进程的 chip ID ──
declare -A busy_chips
in_proc=0
while IFS= read -r line; do
    if [[ "$line" =~ "Process id" ]] && [[ "$line" =~ "Process name" ]]; then
        in_proc=1; continue
    fi
    [ "$in_proc" -eq 0 ] && continue
    [[ "$line" =~ "No running processes" ]] && continue
    [[ "$line" =~ ^\+ ]] && continue
    if [[ "$line" =~ ^\| ]]; then
        col1=$(echo "$line" | cut -d'|' -f2 | xargs)
        chip_id=$(echo "$col1" | awk '{print $NF}')
        [ -n "$chip_id" ] 2>/dev/null && busy_chips["$chip_id"]=1
    fi
done <<< "$output"

# ── 计算 idle ──
idle=0
busy_json="["
first=1
for ((i=0; i<total; i++)); do
    if [ -z "${busy_chips[$i]}" ]; then
        idle=$((idle + 1))
    else
        if [ "$first" -eq 1 ]; then first=0; else busy_json+=","; fi
        busy_json+="$i"
    fi
done
busy_json+="]"

echo "{\"total\":$total,\"idle\":$idle,\"busy_ids\":$busy_json}"
