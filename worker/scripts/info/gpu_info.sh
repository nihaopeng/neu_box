#!/bin/bash
# ============================================================
# 脚本: gpu_info.sh
# 用途: 采集 NVIDIA GPU 设备信息
#
# 输入: 无
# 输出 (stdout): JSON
#   {"total": <int>, "idle": <int>, "busy_ids": [<int>, ...]}
#
# 退出码: 始终为 0
#
# 依赖: nvidia-smi (NVIDIA 管理工具)
#
# 空闲判定（两个条件都满足才算空闲）:
#   1. GPU 利用率 = 0%
#   2. 显存使用 ≤ IDLE_MEM_MB（默认 200MB，env 可配）
#   NVIDIA 卡空闲时驱动约占用 4MB，200MB 是一个安全阈值
#
# 示例:
#   $ ./gpu_info.sh
#   {"total":4,"idle":3,"busy_ids":[2]}
#
#   $ IDLE_MEM_MB=500 ./gpu_info.sh
# ============================================================

IDLE_MEM_MB=${IDLE_MEM_MB:-200}

if ! command -v nvidia-smi &>/dev/null; then
    echo '{"total":0,"idle":0,"busy_ids":[]}'
    exit 0
fi

# 查询 index, 显存总量, 显存已用, GPU 利用率
output=$(nvidia-smi --query-gpu=index,memory.total,memory.used,utilization.gpu \
    --format=csv,noheader,nounits 2>/dev/null)
if [ $? -ne 0 ] || [ -z "$output" ]; then
    echo '{"total":0,"idle":0,"busy_ids":[]}'
    exit 0
fi

total=0
idle=0
busy_ids="["
first=1
while IFS=, read -r idx mem_total mem_used gpu_util; do
    idx=$(echo "$idx" | xargs)
    mem_total=$(echo "$mem_total" | xargs)
    mem_used=$(echo "$mem_used" | xargs)
    gpu_util=$(echo "$gpu_util" | xargs)
    total=$((total + 1))
    busy=false

    # 1. GPU 利用率 > 0% → 占用
    if [ "$gpu_util" -gt 0 ] 2>/dev/null; then
        busy=true
    fi

    # 2. 显存使用超过阈值（空闲卡驱动约占用 4MB）
    if [ "$mem_used" -gt "$IDLE_MEM_MB" ] 2>/dev/null; then
        busy=true
    fi

    if $busy; then
        if [ "$first" -eq 1 ]; then first=0; else busy_ids+=","; fi
        busy_ids+="$idx"
    else
        idle=$((idle + 1))
    fi
done <<< "$output"
busy_ids+="]"

echo "{\"total\":$total,\"idle\":$idle,\"busy_ids\":$busy_ids}"
