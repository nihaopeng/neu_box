# neu-sbox — 将当前终端加入独占设备的 cgroup 沙盒 / 提交命令任务
# 安装: sudo cp neu-sbox.sh /etc/profile.d/
# 用法:
#   neu-sbox acquire [设备数] [CPU核数] [内存(GB)] [命令]
#   neu-sbox tasks

neu-sbox() {
    local cmd="${1:-}"
    local WORKER_URL="${NEU_BOX_URL:-http://127.0.0.1:59075}"

    case "$cmd" in
        acquire|a)
            local device_num="${2:-0}"
            local cpu="${3:-0}"
            local memory="${4:-0}"
            local command="${5:-}"

            # 如果提供了命令参数，走任务提交路径
            if [ -n "$command" ]; then
                echo "[neu-sbox] 提交任务: device=${device_num} cpu=${cpu} mem=${memory}G"
                echo "[neu-sbox] 命令: ${command}"
                echo "[neu-sbox] user=${USER}"

                local resp
                resp=$(curl -s -X POST "${WORKER_URL}/command/run" \
                    -H "Content-Type: application/json" \
                    -d "{\"user_id\":\"${USER}\",\"command\":\"${command}\",\"device_num\":${device_num},\"cpu\":${cpu},\"memory\":${memory},\"mem_unit\":\"GB\"}")

                echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"

                if echo "$resp" | grep -q '"task_id"'; then
                    local task_id position
                    task_id=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['task_id'])" 2>/dev/null)
                    position=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['position'])" 2>/dev/null)
                    echo ""
                    echo "✓ 任务已提交，ID=${task_id} 队列位置 #${position}"
                    echo "  查看日志: neu-sbox result ${task_id}"
                fi
                return
            fi

            # 无命令 → 沙盒模式（加入当前 shell）
            local shell_pid=$$

            echo "[neu-sbox] 申请沙盒: device=${device_num} cpu=${cpu} mem=${memory}G"
            echo "[neu-sbox] shell PID=${shell_pid} user=${USER}"

            local resp
            resp=$(curl -s -X POST "${WORKER_URL}/sandbox/acquire" \
                -H "Content-Type: application/json" \
                -d "{\"username\":\"${USER}\",\"pid\":${shell_pid},\"device_num\":${device_num},\"cpu\":${cpu},\"memory\":${memory},\"mem_unit\":\"GB\"}")

            echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"

            if echo "$resp" | grep -q '"sandbox_name"'; then
                local name
                name=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandbox_name'])" 2>/dev/null)
                echo ""
                echo "✓ 沙盒已创建，当前终端独占设备。释放: neu-sbox release $name"
            fi
            ;;

        release|r)
            local sandbox_name="${2:-}"
            if [ -z "$sandbox_name" ]; then
                echo "用法: neu-sbox release <sandbox_name>"
                echo "先用 neu-sbox list 查看你的沙盒"
                return 1
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
            curl -s "${WORKER_URL}/sandbox/list?username=${USER}" \
                | python3 -m json.tool 2>/dev/null
            ;;

        status|st)
            local shell_pid=$PPID
            local cgroup_path="/proc/${shell_pid}/cgroup"
            echo "[neu-sbox] Shell PID=${shell_pid}"
            if [ -r "$cgroup_path" ]; then
                grep -E "sandbox_" "$cgroup_path" 2>/dev/null || echo "  未在任何沙盒中"
            else
                echo "  无法读取 cgroup 信息"
            fi
            ;;

        tasks|t)
            echo "[neu-sbox] 任务队列:"
            local resp
            resp=$(curl -s "${WORKER_URL}/command/queue")
            echo "$resp" | python3 -m json.tool 2>/dev/null || echo "$resp"
            ;;

        result|res|log|l)
            local task_id="${2:-}"
            if [ -z "$task_id" ]; then
                echo "用法: neu-sbox result <task_id>"
                return 1
            fi

            local meta log_text
            meta=$(curl -s "${WORKER_URL}/command/result/${task_id}")
            log_text=$(curl -s "${WORKER_URL}/command/result/${task_id}/log?raw=1")

            # 解析元数据
            local status cmd user cpu mem devices created retcode timed_out
            status=$(echo "$meta"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('status','?'))" 2>/dev/null)
            cmd=$(echo "$meta"      | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('command','?'))" 2>/dev/null)
            user=$(echo "$meta"     | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('user_id','?'))" 2>/dev/null)
            cpu=$(echo "$meta"      | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('cpu',0))" 2>/dev/null)
            mem=$(echo "$meta"      | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('mem','0'))" 2>/dev/null)
            devices=$(echo "$meta"  | python3 -c "import sys,json; d=json.load(sys.stdin); print(','.join(d.get('devices',[])) or '-')" 2>/dev/null)
            created=$(echo "$meta"  | python3 -c "import sys,json; d=json.load(sys.stdin); import datetime; ts=d.get('created_at'); print(datetime.datetime.fromtimestamp(ts).strftime('%Y-%m-%d %H:%M:%S') if ts else '?')" 2>/dev/null)
            retcode=$(echo "$meta"  | python3 -c "import sys,json; d=json.load(sys.stdin); r=d.get('result',{}); print(r.get('returncode','?'))" 2>/dev/null)
            timed_out=$(echo "$meta" | python3 -c "import sys,json; d=json.load(sys.stdin); r=d.get('result',{}); print('是' if r.get('timed_out') else '否')" 2>/dev/null)

            # 状态着色
            local status_label
            case "$status" in
                completed) status_label="✓ 已完成" ;;
                failed)    status_label="✗ 失败"   ;;
                running)   status_label="▶ 执行中" ;;
                queued)    status_label="○ 排队中" ;;
                *)         status_label="$status"   ;;
            esac

            if [ -n "$log_text" ]; then
                echo "$log_text"
            else
                echo "(无输出)"
            fi
            echo ""
            echo "══════════════════════════════════════════════"
            echo "  任务: ${task_id}"
            echo "══════════════════════════════════════════════"
            echo "  状态:     ${status_label}"
            echo "  用户:     ${user}"
            echo "  命令:     ${cmd}"
            echo "  资源:     CPU=${cpu}  内存=${mem}  设备=${devices}"
            echo "  提交时间: ${created}"
            echo "  返回码:   ${retcode}"
            echo "  超时:     ${timed_out}"
            echo "══════════════════════════════════════════════"
            ;;

        *)
            echo "neu-sbox — 终端沙盒隔离 / 命令任务提交"
            echo ""
            echo "用法: neu-sbox {acquire|release|list|status|tasks|result} [参数]"
            echo ""
            echo "  acquire [设备数] [CPU] [内存]            创建沙盒并加入当前 shell"
            echo "  acquire [设备数] [CPU] [内存] <命令>     提交命令任务"
            echo "  release <sandbox_name>                   释放沙盒"
            echo "  list                                     列出我的沙盒"
            echo "  status                                   查看当前 shell 沙盒状态"
            echo "  tasks                                    查看任务队列"
            echo "  result <task_id>                         查看任务结果和日志"
            echo ""
            echo "示例:"
            echo "  neu-sbox acquire 1                       # 申请 1 个 NPU 沙盒"
            echo "  neu-sbox acquire 2 4 8                   # 申请 2 NPU + 4 核 + 8G 沙盒"
            echo "  neu-sbox acquire 1 2 4 nvidia-smi        # 提交任务: 1 NPU 2核 4G 执行 nvidia-smi"
            echo "  neu-sbox tasks                           # 查看队列"
            echo "  neu-sbox result abc123                   # 查看任务 abc123 结果"
            echo "  neu-sbox release user_pengyt_12345"
            echo ""
            echo "远程 Worker: export NEU_BOX_URL=http://<worker_ip>:59075"
            ;;
    esac
}
