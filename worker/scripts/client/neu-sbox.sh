# neu-sbox — 将当前终端加入独占设备的 cgroup 沙盒
# 安装: sudo cp neu-sbox.sh /etc/profile.d/
# 用法: neu-sbox acquire [设备数] [CPU核数] [内存(GB)]

neu-sbox() {
    local cmd="${1:-}"
    local WORKER_URL="${NEU_BOX_URL:-http://127.0.0.1:59075}"

    case "$cmd" in
        acquire|a)
            local device_num="${2:-0}"
            local cpu="${3:-0}"
            local memory="${4:-0}"
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

        *)
            echo "neu-sbox — 终端沙盒隔离"
            echo ""
            echo "用法: neu-sbox {acquire|release|list|status} [参数]"
            echo ""
            echo "  acquire [设备数] [CPU] [内存]   创建沙盒并加入当前 shell"
            echo "  release <sandbox_name>         释放沙盒"
            echo "  list                           列出我的沙盒"
            echo "  status                         查看当前 shell 沙盒状态"
            echo ""
            echo "示例:"
            echo "  neu-sbox acquire 1             # 申请 1 个 NPU"
            echo "  neu-sbox acquire 2 4 8         # 申请 2 NPU + 4 核 + 8G"
            echo "  neu-sbox release user_pengyt_12345"
            echo ""
            echo "远程 Worker: export NEU_BOX_URL=http://<worker_ip>:59075"
            ;;
    esac
}
