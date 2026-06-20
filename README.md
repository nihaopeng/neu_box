# Neu Box — 轻量多节点资源管理与沙盒隔离

## 架构

```
浏览器 ──→ Master (Flask) ──→ Worker (Flask) ──→ ttyd / bash
              │                      │
              │ 节点发现/请求转发      │ cgroup v2 + eBPF 沙盒
              │ 每15s轮询节点状态       │ 端口池管理
              │                      │ 终端 / 命令队列
           .env + config.json      .env
```

**两种工作模式：**
- **终端模式**：Master 转发请求到 Worker，Worker 启动 ttyd（终端 over HTTP），前端通过 iframe 嵌入。ttyd `-q` 参数确保连接断开后自动退出
- **命令模式**：Master 转发命令到 Worker，Worker 维护 FIFO 任务队列，逐任务在沙盒中执行并返回结果

## 运行

```bash
# Master
cd master
python main.py

# Worker（sudo 启动，自动安装 neu-sbox.sh 到 /etc/profile.d/）
cd worker
sudo python main.py
```

## 配置

### Master — `master/.env`

| 变量 | 默认值 | 含义 |
|---|---|---|
| `listen` | `0.0.0.0` | Master 监听地址 |
| `port` | `25565` | Master 监听端口 |
| `db_dir` | `./db` | SQLite 数据库目录（实验记录） |
| `poll_interval` | `15` | 节点状态轮询间隔（秒） |
| `LOG_LEVEL` | `INFO` | 日志级别：`DEBUG` / `INFO` / `WARNING` / `ERROR` |

节点列表由 `master/config.json` 中的 `nodes_pool` 数组管理，支持前端 UI 动态增删：

| 字段 | 含义 |
|---|---|
| `nodes_pool[].name` | 节点显示名称 |
| `nodes_pool[].host` | Worker IP 地址 |
| `nodes_pool[].port` | Worker 端口 |

### Worker — `worker/.env`

| 变量 | 默认值 | 含义 |
|---|---|---|
| `port` | `59075` | Worker 监听端口 |
| `listen` | `0.0.0.0` | Worker 监听地址 |
| `HOST_IP` | (自动检测) | Worker 对外 IP，终端 URL 中返回 |
| `port_pool_start` | `59081` | ttyd 端口池起始 |
| `port_pool_end` | `59100` | ttyd 端口池结束 |
| `cgroup_version` | `2` | cgroup 版本（1 或 2） |
| `device_major` | `235` | 设备 major 号（235=NPU/Davinci, 195=GPU/NVIDIA） |
| `db_dir` | `./db` | SQLite 数据库目录 |
| `sandbox_reaper_interval` | `30` | 收尸线程扫描间隔（秒）。同时作为终端无连接超时：若端口无 ESTABLISHED 连接则直接清理 |
| `command_timeout` | `300` | 命令执行超时（秒） |
| `command_max_completed` | `200` | 已完成任务保留上限 |
| `task_log_dir` | `./logs` | 日志文件目录 |
| `LOG_LEVEL` | `INFO` | 日志级别：`DEBUG` / `INFO` / `WARNING` / `ERROR` |
| `gpu_info_script_path` | — | GPU 状态采集脚本路径 |
| `npu_info_script_path` | — | NPU 状态采集脚本路径 |
| `sandbox_script_path` | — | 沙盒管理脚本路径（cgroup + eBPF） |

## 数据流

### 终端模式

```
POST /terminal/create {node_id, username, password, cpu, memory, device_num}
  │
  Master ──→ Worker /terminal/create
               │
               ├─ Port_Pool.acquire_port()         → 分配端口
               ├─ Popen(ttyd -p <port> -q ...)      → 启动终端服务
               ├─ SbxManager.allocate_for_terminal() → 创建 cgroup 沙盒
               └─ 返回 {terminal_url, sandbox_id}
  │
浏览器 iframe.src = terminal_url  →  ttyd WebSocket
  │
用户关闭标签页 → ttyd -q 自动退出
  │
Reaper(每30s) → cleanup_orphaned():
  ├─ PID已死 → 销毁沙盒 + 归还端口
  └─ PID存活但端口无ESTABLISHED连接 → SIGTERM → 销毁沙盒 + 归还端口
```

### 命令模式

```
POST /command/run {node_id, user_id, command, cpu, memory, device_num}
  │
  Master ──→ Worker /command/run
               │
               └─ TaskQueue.submit() → 持久化到 SQLite → 返回 {task_id, position}
  │
TaskQueue 后台消费线程:
  ├─ 取队首任务
  ├─ SbxManager.create_sandbox()    → 创建 cgroup 沙盒
  ├─ Popen(preexec_fn: 写cgroup + setuid) → 在沙盒中以目标用户执行 bash -c <cmd>
  ├─ communicate(timeout)           → 收集 stdout/stderr
  ├─ SbxManager.destroy_sandbox()   → 销毁沙盒
  └─ DB 更新结果

GET /command/result/<task_id>?user_id=...&password=...  → 返回日志（需密码校验）
```

### 终端沙盒模式

将当前 shell 加入独占设备的 cgroup 沙盒。Worker 通过 `/proc/<pid>/status` 校验 PID 归属，无需密码。

```bash
# 安装 — Worker 用 sudo 启动时自动安装到 /etc/profile.d/，无需手动操作
# 新终端登录后自动生效，当前终端可手动 source：
source /etc/profile.d/neu-sbox.sh

# 使用
neu-sbox acquire 1              # 申请 1 个 NPU，当前终端立即隔离
neu-sbox acquire 2 4 8          # 申请 2 NPU + 4 核 CPU + 8G 内存
neu-sbox status                 # 查看当前 shell 是否在沙盒中
neu-sbox list                   # 列出我的沙盒
neu-sbox release <name>         # 释放沙盒
```

```bash
# 远程 Worker
export NEU_BOX_URL=http://<worker_ip>:59075
neu-sbox acquire 1
```

```
POST /sandbox/acquire {username, pid, device_num}
  → /proc/<pid>/status 校验 → cgroup 沙盒 + 设备分配 → shell PID 加入
POST /sandbox/release {sandbox_name}
  → destroy_sandbox() → 设备归还
```
