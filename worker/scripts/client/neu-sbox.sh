#!/bin/bash
# neu-sbox — 终端沙盒隔离 / 命令任务提交 CLI
# 安装: worker 启动时自动复制到 /usr/local/bin/neu-sbox (chmod 755)

cmd="${1:-}"
WORKER_URL="${NEU_BOX_URL:-http://127.0.0.1:59075}"

case "$cmd" in
    acquire|a)
        device_num="${2:-0}"
        cpu="${3:-0}"
        memory="${4:-0}"
        command="${5:-}"

        # 如果提供了命令参数，走任务提交路径
        if [ -n "$command" ]; then
            echo "[neu-sbox] 提交任务: device=${device_num} cpu=${cpu} mem=${memory}G"
            echo "[neu-sbox] 命令: ${command}"
            echo "[neu-sbox] user=${USER}"

            resp=$(curl -s -X POST "${WORKER_URL}/command/run" \
                -H "Content-Type: application/json" \
                -d "{\"user_id\":\"${USER}\",\"command\":\"${command}\",\"device_num\":${device_num},\"cpu\":${cpu},\"memory\":${memory},\"mem_unit\":\"GB\"}")

            echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"

            if echo "$resp" | grep -q '"task_id"'; then
                task_id=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['task_id'])" 2>/dev/null)
                position=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['position'])" 2>/dev/null)
                echo ""
                echo "✓ 任务已提交，ID=${task_id} 队列位置 #${position}"
                echo "  查看日志: neu-sbox result ${task_id}"
            fi
            exit 0
        fi

        # 无命令 → 沙盒模式（加入当前 shell）
        shell_pid=$PPID

        echo "[neu-sbox] 申请沙盒: device=${device_num} cpu=${cpu} mem=${memory}G"
        echo "[neu-sbox] shell PID=${shell_pid} user=${USER}"

        resp=$(curl -s -X POST "${WORKER_URL}/sandbox/acquire" \
            -H "Content-Type: application/json" \
            -d "{\"username\":\"${USER}\",\"pid\":${shell_pid},\"device_num\":${device_num},\"cpu\":${cpu},\"memory\":${memory},\"mem_unit\":\"GB\"}")

        echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"

        if echo "$resp" | grep -q '"sandbox_name"'; then
            name=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandbox_name'])" 2>/dev/null)
            echo ""
            echo "✓ 沙盒已创建，当前终端独占设备。释放: neu-sbox release $name"
        fi
        ;;

    release|r)
        sandbox_name="${2:-}"
        if [ -z "$sandbox_name" ]; then
            echo "用法: neu-sbox release <sandbox_name>"
            echo "先用 neu-sbox list 查看你的沙盒"
            exit 1
        fi
        echo "[neu-sbox] 释放沙盒: ${sandbox_name}..."
        curl -s -X POST "${WORKER_URL}/sandbox/release" \
            -H "Content-Type: application/json" \
            -d "{\"sandbox_name\":\"${sandbox_name}\"}" \
            | python3 -m json.tool 2>/dev/null
        echo ""
        echo "✓ 沙盒已释放"
        ;;

    list|ls)
        echo "[neu-sbox] ${USER} 的沙盒:"
        python3 -c "
import json, sys
from urllib.request import urlopen

resp = urlopen('${WORKER_URL}/sandbox/list?username=${USER}')
data = json.loads(resp.read())
sbs = data.get('sandboxes', [])

if not sbs:
    print('  (无)')
else:
    for s in sbs:
        name = s.get('name', '')
        cpu = s.get('cpu', 0) or 0
        mem = s.get('mem', '0') or '0'
        devices = s.get('devices', [])
        dev_str = ','.join(str(d) for d in devices) if devices else '—'
        res_parts = []
        if cpu: res_parts.append(f'CPU={cpu}')
        if mem and mem != '0': res_parts.append(f'mem={mem}')
        res_str = ' '.join(res_parts) if res_parts else '资源不限'
        print(f'  {name}')
        print(f'    设备: {dev_str}  |  {res_str}')
" 2>/dev/null || curl -s "${WORKER_URL}/sandbox/list?username=${USER}"
        ;;

    status|st)
        shell_pid=$PPID
        cgroup_path="/proc/${shell_pid}/cgroup"
        echo "[neu-sbox] Shell PID=${shell_pid}"
        if [ -r "$cgroup_path" ]; then
            grep -E "sandbox_" "$cgroup_path" 2>/dev/null || echo "  未在任何沙盒中"
        else
            echo "  无法读取 cgroup 信息"
        fi
        ;;

    tasks|t)
        echo "[neu-sbox] 任务队列:"
        resp=$(curl -s "${WORKER_URL}/command/queue")
        echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"
        ;;

    result|res|log|l)
        task_id="${2:-}"
        if [ -z "$task_id" ]; then
            echo "用法: neu-sbox result <task_id>"
            exit 1
        fi
        python3 -c "
import json, sys, time
from urllib.request import urlopen

url = '${WORKER_URL}'
tid = '${task_id}'

try:
    meta_resp = urlopen(f'{url}/command/result/{tid}')
    meta = json.loads(meta_resp.read())
except Exception as e:
    print(f'查询失败: {e}')
    sys.exit(1)

try:
    log_resp = urlopen(f'{url}/command/result/{tid}/log?raw=1')
    log_text = log_resp.read().decode('utf-8', errors='replace')
except Exception:
    log_text = ''

# ── 日志内容 ──
if log_text:
    print(log_text.rstrip())
else:
    print('(无输出)')

# ── 状态摘要 ──
r = meta.get('result', {}) or {}
rc = r.get('returncode', '')
status = meta.get('status', '?')
status_icon = {'completed': '✓', 'failed': '✗', 'running': '▶', 'queued': '○'}.get(status, '?')

line = f'[{status_icon} {status}]'
if rc is not None and rc != '':
    line += f'  rc={rc}'
    if r.get('timed_out'):
        line += ' (超时)'
line += f'  |  {meta.get(\"user_id\", \"?\")}  |  {meta.get(\"command\", \"?\")}'

cpu = meta.get('cpu', 0) or 0
mem = meta.get('mem', '0') or '0'
parts = []
if cpu: parts.append(f'CPU={cpu}')
if mem and mem != '0': parts.append(f'mem={mem}')
res_info = '  '.join(parts) if parts else '资源不限'
if res_info != '资源不限':
    line += f'  |  {res_info}'

dev_n = meta.get('device_num', 0) or 0
devices = meta.get('devices', [])
dev_str = ','.join(str(d) for d in devices) if devices else ''
if dev_n:
    line += f'  |  设备={dev_n}'
    if dev_str:
        line += f' ({dev_str})'

ts = meta.get('finished_at') or meta.get('created_at')
if ts:
    line += f'  |  {time.strftime(\"%m-%d %H:%M\", time.localtime(ts))}'

print()
print(line)
" 2>/dev/null || {
            echo "[neu-sbox] 查询失败，请检查 task_id 和网络连接"
        }
        ;;

    *)
        echo "neu-sbox — 终端沙盒隔离 / 命令任务提交"
        echo ""
        echo "用法: neu-sbox {acquire|release|list|status|tasks|result} [参数]"
        echo ""
        echo "  acquire [设备数] [CPU] [内存]            创建沙盒并加入当前 shell"
        echo "  acquire [设备数] [CPU] [内存] <命令>     提交命令任务"
        echo "  release <sandbox_name>                   释放沙盒"
        echo "  list                                     列出我的沙盒（含资源详情）"
        echo "  status                                   查看当前 shell 沙盒状态"
        echo "  tasks                                    查看任务队列"
        echo "  result <task_id>                         查看任务结果和日志"
        echo ""
        echo "示例:"
        echo "  neu-sbox acquire 1                       # 申请 1 个 NPU 沙盒"
        echo "  neu-sbox acquire 2 4 8                   # 申请 2 NPU + 4 核 + 8G 沙盒"
        echo "  neu-sbox acquire 1 2 4 nvidia-smi        # 提交: 1 NPU 2核 4G 跑 nvidia-smi"
        echo "  neu-sbox list                            # 列出沙盒（显示设备/资源）"
        echo "  neu-sbox tasks                           # 查看队列"
        echo "  neu-sbox result abc123                   # 查看任务 abc123 结果和日志"
        echo "  neu-sbox release user_pengyt_12345"
        echo ""
        echo "已在沙盒中再次 acquire → 自动释放旧沙盒，覆盖为新资源"
        echo "远程 Worker: export NEU_BOX_URL=http://<worker_ip>:59075"
        ;;
esac
