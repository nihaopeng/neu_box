#!/bin/bash
# ============================================================
# 脚本: gpu_info.sh
# 用途: 采集 NVIDIA GPU 设备信息
#
# 输入: 无
# 输出 (stdout): JSON
#   {"total": <int>, "idle": <int>}
#
# 退出码: 始终为 0
#
# 依赖: nvidia-smi (NVIDIA 管理工具)
#
# 空闲判定:
#   显存使用率 ≤ 5% → 空闲
#   显存使用率 > 5% → 非空闲
#
# 示例:
#   $ ./gpu_info.sh
#   {"total":4,"idle":3}
#
#   $ ./gpu_info.sh
#   {"total":0,"idle":0}
# ============================================================

if ! command -v nvidia-smi &>/dev/null; then
    echo '{"total":0,"idle":0}'
    exit 0
fi

output=$(nvidia-smi --query-gpu=index,memory.total,memory.used \
    --format=csv,noheader,nounits 2>/dev/null)
if [ $? -ne 0 ] || [ -z "$output" ]; then
    echo '{"total":0,"idle":0}'
    exit 0
fi

total=0
idle=0
while IFS=, read -r idx mem_total mem_used; do
    idx=$(echo "$idx" | xargs)
    mem_total=$(echo "$mem_total" | xargs)
    mem_used=$(echo "$mem_used" | xargs)
    total=$((total + 1))
    # 显存使用率超过 5% 认为非空闲
    if [ "$mem_total" -gt 0 ] 2>/dev/null; then
        used_pct=$(( mem_used * 100 / mem_total ))
        if [ "$used_pct" -le 5 ] 2>/dev/null; then
            idle=$((idle + 1))
        fi
    else
        idle=$((idle + 1))
    fi
done <<< "$output"

echo "{\"total\":$total,\"idle\":$idle}"
