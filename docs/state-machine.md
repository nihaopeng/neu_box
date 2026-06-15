# 系统状态机设计文档

## 概述

neu_box 是一个轻量多节点资源管理系统，采用 Master-Worker 架构。用户通过 Master 申请终端沙盒（1 GPU / 1 CPU / 1G 内存），Master 调度到对应节点的 Worker，Worker 通过自研 cgroup 脚本创建隔离沙盒并运行 ttyd，将 IP:端口返回前端渲染。用户直连 Worker 节点，Master 不做流量中转。

---

## 一、全局系统状态机

```
                                    ┌──────────────────────────────────────────────────────┐
                                    │                    neu_box 全局系统                    │
                                    │                                                      │
  ┌──────┐   POST /terminal   ┌──────────┐   dispatch   ┌──────────┐   create   ┌──────────┐
  │ USER │ ──────────────────►│  MASTER  │ ────────────►│  WORKER  │ ─────────►│ SANDBOX  │
  │      │                    │          │              │          │           │ (cgroup) │
  │      │ ◄───────────────── │          │ ◄──────────── │          │ ◄───────── │          │
  └──────┘   {ip, port, id}   └──────────┘  {status}    └──────────┘  {ready}   └──────────┘
                                        │                                              │
                                        │           直连 ttyd (WebSocket)               │
                                        └──────────────────────────────────────────────┘
```

---

## 二、Master 状态机

Master 是整个系统的调度中枢，负责接收用户请求、选择合适 Worker、转发请求、返回结果。

### 2.1 状态定义

```
                         ┌─────────────┐
                         │             │
               ┌────────►│    IDLE     │◄──────────┐
               │         │   (就绪)     │           │
               │         └──────┬──────┘           │
               │                │                   │
               │         收到用户请求               │ 调度失败 /
               │                │                   │ 超时恢复
               │         ┌──────▼──────┐           │
               │         │             │           │
               │         │ SCHEDULING  │───────────┘
               │         │  (调度中)    │
               │         └──────┬──────┘
               │                │
               │         选中 Worker,
               │         发送请求
               │                │
               │   ┌────────────▼────────────┐
               │   │                         │
               ├───┤  WAITING_WORKER_RESPONSE│
               │   │     (等待Worker响应)      │
               │   └────────────┬────────────┘
               │                │
               │         ┌──────┴──────┐
               │         │             │
               │    Worker 成功返回    Worker 超时/失败
               │         │             │
               │   ┌─────▼──────┐  ┌──▼───────────┐
               │   │ RESPONDING │  │  SCHEDULING   │──► 重试 / 返回错误
               │   │ (响应用户)  │  │ _FAILED       │
               │   └─────┬──────┘  │  (调度失败)     │
               │         │         └───────────────┘
               │   返回 IP:Port
               │   给前端
               │         │
               └─────────┘
```

### 2.2 状态详解

| 状态 | 描述 | 触发条件 | 动作 |
|------|------|----------|------|
| **IDLE** | Master 空闲，等待用户请求 | 服务启动 / 上一次请求处理完毕 | 监听 HTTP 端口 |
| **SCHEDULING** | 正在选择合适的 Worker 节点 | 收到 `POST /api/terminal` 请求 | 查询各 Worker 资源状态，按策略筛选 |
| **WAITING_WORKER_RESPONSE** | 已向 Worker 发送创建请求，等待结果 | 调度成功，HTTP 请求已发往 Worker | 等待 Worker 响应，设置超时计时器（默认 30s） |
| **RESPONDING** | Worker 返回成功，组装响应给前端 | Worker 返回 `{ip, port, sandbox_id}` | 封装 JSON 返回给用户，记录分配信息 |
| **SCHEDULING_FAILED** | 调度或创建过程失败 | Worker 超时 / 无可用节点 / Worker 返回错误 | 重试（最多 3 次）或返回错误给用户 |

### 2.3 调度策略

```
调度输入: {gpu: 1, cpu: 1, memory: "1G"}

策略优先级（可配置）:
  1. 资源最匹配优先 (BestFit)   — 选择剩余资源刚好满足需求的节点
  2. 负载最低优先 (LeastLoaded) — 选择当前负载最小的节点
  3. 轮询 (RoundRobin)          — 依次轮询所有可用节点

调度流程:
  1. 从 src_manager 获取所有注册 Worker 列表
  2. 过滤: worker.status == ONLINE && idle_gpu >= 1 && idle_cpu >= 1 && idle_mem >= 1G
  3. 按策略排序选出目标 Worker
  4. 若无可用 Worker → 进入 SCHEDULING_FAILED
  5. 若有 → 向目标 Worker 发送 POST /api/sandbox/create
```

### 2.4 超时与重试

```
Worker 请求超时: 30s
最大重试次数:   3
重试间隔:       1s (指数退避: 1s, 2s, 4s)
重试策略:       重试时排除上次失败的 Worker，选择下一个候选
全部失败:       返回 HTTP 503 {error: "no_worker_available"}
```

---

## 三、Worker 状态机

Worker 部署在每个计算节点上，负责管理本节点的 cgroup 沙盒生命周期。

### 3.1 状态定义

```
                         ┌─────────────┐
                         │             │
               ┌────────►│    IDLE     │◄──────────────┐
               │         │   (就绪)     │               │
               │         └──────┬──────┘               │
               │                │                       │
               │         收到 Master 请求              │
               │         POST /sandbox/create           │
               │                │                       │
               │         ┌──────▼──────┐               │
               │         │             │               │
               │         │  ALLOCATING │               │
               │         │ (资源分配中) │               │
               │         └──────┬──────┘               │
               │                │                       │
               │         ┌──────┴──────┐               │
               │         │             │               │
               │    cgroup 创建成功   cgroup 创建失败    │
               │         │             │               │
               │   ┌─────▼──────┐  ┌──▼───────────┐   │
               │   │  STARTING  │  │  ALLOCATION   │   │
               │   │  _TTYD     │  │  _FAILED      │───┤→ 返回错误给 Master
               │   │ (启动ttyd) │  └───────────────┘   │
               │   └─────┬──────┘                      │
               │         │                             │
               │   ttyd 启动成功                        │
               │         │                             │
               │   ┌─────▼──────┐                      │
               │   │             │                     │
               ├───┤   RUNNING   │                     │
               │   │  (沙盒运行)  │                     │
               │   └─────┬──────┘                     │
               │         │                             │
               │   用户断开 / Master 发来销毁请求 /     │
               │   超时自动回收                          │
               │         │                             │
               │   ┌─────▼──────┐                      │
               │   │  STOPPING  │                      │
               │   │ (清理回收中)│                      │
               │   └─────┬──────┘                      │
               │         │                             │
               │   清理完成                              │
               │         │                             │
               └─────────┘                             │
```

### 3.2 状态详解

| 状态 | 描述 | 触发条件 | 动作 |
|------|------|----------|------|
| **IDLE** | Worker 空闲，等待 Master 指令 | Worker 启动 / 上一次清理完成 | 向 Master 注册，上报心跳和资源状态 |
| **ALLOCATING** | 正在调用 cgroup 脚本创建沙盒 | 收到 Master 的 `POST /api/sandbox/create` | 执行 `cgroup-sandbox.sh create --gpu 1 --cpu 1 --mem 1G` |
| **STARTING_TTYD** | cgroup 创建完毕，正在启动 ttyd | cgroup 脚本返回成功 | 在沙盒内启动 ttyd，分配端口 |
| **RUNNING** | 沙盒正常运行，ttyd 对外服务 | ttyd 进程启动成功，端口监听中 | 向 Master 返回 `{ip, port, sandbox_id}`，监控资源使用 |
| **ALLOCATION_FAILED** | 沙盒创建失败 | cgroup 脚本执行失败 / 端口冲突 / 资源不足 | 清理残留资源，返回错误给 Master |
| **STOPPING** | 正在销毁沙盒，回收资源 | 用户请求销毁 / 超时自动回收 / Master 指令 | 执行 `cgroup-sandbox.sh destroy <sandbox_id>`，杀 ttyd 进程 |

### 3.3 Worker 心跳与注册

```
注册:  Worker 启动时 POST /api/worker/register 到 Master
      携带: {node_id, ip, total_gpu, total_cpu, total_mem}

心跳:  每 10s PUT /api/worker/heartbeat 到 Master
      携带: {node_id, idle_gpu, idle_cpu, idle_mem, active_sandboxes: [...]}

过期:  Master 超过 30s 未收到心跳 → 标记 Worker 为 OFFLINE
```

---

## 四、Sandbox（cgroup 沙盒）状态机

每个 Sandbox 是一个独立的 cgroup 隔离环境，内部运行 ttyd 提供 Web 终端。

### 4.1 状态定义

```
  ┌──────────┐
  │  PENDING │  ◄── Master 已调度，等待 Worker 创建
  └────┬─────┘
       │
       │ Worker 开始创建
       ▼
  ┌──────────┐
  │ CREATING │  ◄── cgroup 脚本正在执行
  └────┬─────┘
       │
       ├── cgroup 创建成功 ──► ┌───────────┐
       │                       │ TTYD_INIT │  ◄── 正在启动 ttyd
       │                       └─────┬─────┘
       │                             │
       │                       ttyd 启动成功
       │                             │
       │                             ▼
       │                       ┌──────────┐
       │                       │ RUNNING  │  ◄── 终端可用，用户已连接或可连接
       │                       └────┬─────┘
       │                            │
       │                      用户主动关闭 /
       │                      超时自动回收 /
       │                      Master 销毁指令
       │                            │
       │                            ▼
       │                       ┌──────────┐
       │                       │ STOPPING │  ◄── kill ttyd, 清理 cgroup
       │                       └────┬─────┘
       │                            │
       │                      清理完成
       │                            │
       │                            ▼
       │                       ┌──────────┐
       │                       │ STOPPED  │  ◄── 资源已释放
       │                       └──────────┘
       │
       └── cgroup 创建失败 ──► ┌──────────┐
                               │  ERROR   │  ◄── 资源不足 / 脚本错误 / 权限问题
                               └──────────┘
```

### 4.2 状态详解

| 状态 | 描述 | 所属组件 | 生命周期事件 |
|------|------|----------|-------------|
| **PENDING** | 沙盒请求已调度，等待 Worker 执行 | Master 记录 | `sandbox.create.requested` |
| **CREATING** | cgroup 脚本正在创建隔离环境 | Worker | `cgroup.create.start` |
| **TTYD_INIT** | cgroup 已就绪，正在启动 ttyd 进程 | Worker | `ttyd.start` |
| **RUNNING** | ttyd 正常运行，终端对外可用 | Worker | `ttyd.listening` |
| **STOPPING** | 正在销毁沙盒，回收资源 | Worker | `sandbox.destroy.start` |
| **STOPPED** | 沙盒已彻底销毁，资源释放 | Worker(最后上报) | `sandbox.destroy.complete` |
| **ERROR** | 创建或运行过程中发生不可恢复错误 | Worker/Master | `sandbox.error` |

### 4.3 Sandbox 生命周期超时

```
默认最大存活时间:  2 小时 (可配置)
空闲超时:         15 分钟无 IO 自动回收
宽限期:          超时前 5 分钟向用户发送警告
强制回收:        超时后立即执行 STOPPING
```

---

## 五、Cgroup 脚本内部流程（资源隔离）

### 5.1 脚本接口

```bash
# 创建沙盒
cgroup-sandbox.sh create \
  --name <sandbox_id> \
  --cpu 1 \
  --memory 1G \
  --gpu 1 \
  --image <container_image> \
  --port <preferred_port>

# 输出 (JSON):
# 成功: {"status":"ok","sandbox_id":"sbx-abc123","port":8765}
# 失败: {"status":"error","message":"reason..."}

# 销毁沙盒
cgroup-sandbox.sh destroy --name <sandbox_id>

# 查询沙盒
cgroup-sandbox.sh status --name <sandbox_id>
```

### 5.2 Cgroup 脚本内部状态流

```
  create 命令
       │
       ▼
  ┌──────────────────┐
  │ 1. 参数校验       │  校验 CPU/内存/GPU 参数合法性
  │    VALIDATE_ARGS  │
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 2. 创建 cgroup    │  mkdir /sys/fs/cgroup/<subsystem>/sandbox_<id>
  │    CGROUP_MKDIR   │  cpu, memory, devices 三个 subsystem
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 3. 设置资源限制   │  echo <quota> > cpu.max
  │    SET_LIMITS     │  echo <mem> > memory.max
  │                   │  echo <gpu_dev> > devices.allow
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 4. 创建命名空间   │  unshare 创建隔离的 mount/pid/net/uts 命名空间
  │    NS_CREATE      │  (可选，视隔离级别而定)
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 5. 启动沙盒进程   │  在 cgroup 中启动 bash，挂载 rootfs (若使用容器镜像)
  │    INIT_PROCESS   │  echo $$ > cgroup.procs
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 6. 分配端口       │  从可用端口池分配，检查端口是否被占用
  │    PORT_ALLOC     │
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 7. 启动 ttyd      │  ttyd -p <port> -c user:pass --cgroup <cgroup_path> bash
  │    TTYD_LAUNCH    │  等待 ttyd 监听端口
  └───────┬──────────┘
          │ ✓
          ▼
  ┌──────────────────┐
  │ 8. 验证 & 输出    │  curl localhost:<port> 确认可达
  │    VERIFY         │  输出 JSON 结果到 stdout
  └──────────────────┘

  任何步骤失败 → 执行回滚 (rollback) → 输出 {"status":"error"}
```

### 5.3 Cgroup 资源限制详情

```
CPU:
  /sys/fs/cgroup/cpu/sandbox_<id>/cpu.max
  → "100000 100000" (= 1 核, 周期 100ms 内最多使用 100ms)

Memory:
  /sys/fs/cgroup/memory/sandbox_<id>/memory.max
  → "1073741824" (= 1G)
  /sys/fs/cgroup/memory/sandbox_<id>/memory.swap.max
  → "0" (禁用 swap，更严格隔离)

GPU:
  /sys/fs/cgroup/devices/sandbox_<id>/devices.allow
  → "c 195:* rwm" (NVIDIA GPU 设备号)
  配合 nvidia-container-toolkit 或设置 CUDA_VISIBLE_DEVICES

进程数限制:
  /sys/fs/cgroup/pids/sandbox_<id>/pids.max
  → "64"
```

---

## 六、完整交互时序

### 6.1 正常创建流程

```
  User          Frontend        Master            Worker          Cgroup Script    ttyd
  │                │               │                 │                 │              │
  │  点击"新建终端"  │               │                 │                 │              │
  │───────────────►│               │                 │                 │              │
  │                │  POST         │                 │                 │              │
  │                │  /api/terminal│                 │                 │              │
  │                │──────────────►│                 │                 │              │
  │                │               │                 │                 │              │
  │                │               │ SCHEDULING      │                 │              │
  │                │               │──────┐          │                 │              │
  │                │               │      │ 查询节点资源                │              │
  │                │               │◄─────┘          │                 │              │
  │                │               │                 │                 │              │
  │                │               │ WAITING_WORKER  │                 │              │
  │                │               │────────────────►│                 │              │
  │                │               │ POST /sandbox/  │                 │              │
  │                │               │      create     │                 │              │
  │                │               │                 │                 │              │
  │                │               │                 │ ALLOCATING      │              │
  │                │               │                 │────────────────►│              │
  │                │               │                 │  create         │              │
  │                │               │                 │                 │──┐           │
  │                │               │                 │                 │  │ cgroup    │
  │                │               │                 │                 │  │ mkdir     │
  │                │               │                 │                 │◄─┘           │
  │                │               │                 │                 │──┐           │
  │                │               │                 │                 │  │ set       │
  │                │               │                 │                 │  │ limits    │
  │                │               │                 │                 │◄─┘           │
  │                │               │                 │◄────────────────│              │
  │                │               │                 │  {"status":"ok"}│              │
  │                │               │                 │                 │              │
  │                │               │                 │ STARTING_TTYD   │              │
  │                │               │                 │─────────────────────────────────►
  │                │               │                 │            ttyd -p 8765 bash     │
  │                │               │                 │◄─────────────────────────────────
  │                │               │                 │         listening on :8765       │
  │                │               │                 │                 │              │
  │                │               │                 │ RUNNING         │              │
  │                │               │◄────────────────│                 │              │
  │                │               │ {ip,port,id}    │                 │              │
  │                │               │                 │                 │              │
  │                │               │ RESPONDING      │                 │              │
  │                │◄──────────────│                 │                 │              │
  │                │ {ip,port,id}  │                 │                 │              │
  │                │               │                 │                 │              │
  │◄───────────────│               │                 │                 │              │
  │  显示终端窗口    │               │                 │                 │              │
  │                │               │                 │                 │              │
  │══════════════════════════════════════════════════════════════════►│              │
  │              直连 WebSocket: ws://<worker_ip>:8765                 │              │
  │                (不经过 Master)                                     │              │
```

### 6.2 销毁流程

```
  User          Frontend        Master            Worker          Cgroup Script
  │                │               │                 │                 │
  │  关闭终端标签页  │               │                 │                 │
  │───────────────►│               │                 │                 │
  │                │  DELETE       │                 │                 │
  │                │  /api/terminal│                 │                 │
  │                │  /<sandbox_id>│                 │                 │
  │                │──────────────►│                 │                 │
  │                │               │────────────────►│                 │
  │                │               │ POST /sandbox/  │                 │
  │                │               │    destroy      │                 │
  │                │               │                 │ STOPPING        │
  │                │               │                 │────────────────►│
  │                │               │                 │  destroy        │
  │                │               │                 │                 │──┐
  │                │               │                 │                 │  │ kill ttyd
  │                │               │                 │                 │  │ rmdir cgroup
  │                │               │                 │                 │◄─┘
  │                │               │                 │◄────────────────│
  │                │               │◄────────────────│  {"status":"ok"}│
  │                │◄──────────────│                 │                 │
  │                │  {"ok":true}  │                 │                 │
```

### 6.3 异常流程 — Worker 超时

```
  Master                   Worker
  │                          │
  │ POST /sandbox/create     │
  │─────────────────────────►│
  │                          │  (Worker 挂了 / 网络不通)
  │                          X
  │                          │
  │  ... 等待 30s ...         │
  │                          │
  │ SCHEDULING_FAILED        │
  │──────┐                   │
  │      │ 重试下一个 Worker   │
  │◄─────┘                   │
  │                          │
  │ POST /sandbox/create     │
  │────────────────────────────────────►│  Worker-2 (另一个节点)
  │                                     │
  │◄────────────────────────────────────│  {"status":"ok", ...}
  │                                     │
```

---

## 七、API 接口定义

### 7.1 Master 对外 API（用户/Frontend 调用）

| 方法 | 路径 | 描述 | 请求体 | 响应 |
|------|------|------|--------|------|
| `POST` | `/api/terminal` | 创建终端沙盒 | `{gpu:1, cpu:1, memory:"1G", duration:7200}` | `{sandbox_id, ip, port, token, expires_at}` |
| `DELETE` | `/api/terminal/<id>` | 销毁终端沙盒 | — | `{ok: true}` |
| `GET` | `/api/terminal/<id>` | 查询沙盒状态 | — | `{sandbox_id, status, ip, port, created_at, expires_at}` |
| `GET` | `/api/terminals` | 列出用户所有沙盒 | — | `[{sandbox_id, status, ...}]` |
| `GET` | `/api/nodes` | 查看集群节点状态 | — | `[{node_id, status, idle_gpu, idle_cpu, ...}]` |

### 7.2 Master ↔ Worker 内部 API

| 方法 | 路径 | 描述 | 请求体 | 响应 |
|------|------|------|--------|------|
| `POST` | `/api/sandbox/create` | Master 请求 Worker 创建沙盒 | `{sandbox_id, gpu, cpu, memory}` | `{status, ip, port}` |
| `POST` | `/api/sandbox/destroy` | Master 请求 Worker 销毁沙盒 | `{sandbox_id}` | `{ok: true}` |
| `GET` | `/api/sandbox/list` | Master 查询 Worker 上所有沙盒 | — | `[{sandbox_id, status, ...}]` |
| `POST` | `/api/worker/register` | Worker 向 Master 注册 | `{node_id, ip, total_gpu, total_cpu, total_mem}` | `{ok: true}` |
| `PUT` | `/api/worker/heartbeat` | Worker 向 Master 上报心跳 | `{node_id, idle_gpu, idle_cpu, idle_mem, sandboxes}` | `{ok: true}` |

---

## 八、数据模型

### 8.1 Master 侧数据结构

```python
# Worker 节点记录
class WorkerNode:
    node_id: str          # 唯一标识, 如 "node-gpu-01"
    ip: str               # Worker IP
    status: str           # ONLINE | OFFLINE | DRAINING
    total_gpu: int        # GPU 总量
    total_cpu: int        # CPU 总量
    total_mem: int        # 内存总量 (bytes)
    idle_gpu: int         # 空闲 GPU
    idle_cpu: int         # 空闲 CPU
    idle_mem: int         # 空闲内存 (bytes)
    active_sandboxes: list # 该节点上活跃沙盒 ID 列表
    last_heartbeat: float  # 最后心跳时间戳
    registered_at: float   # 注册时间戳

# Sandbox 记录 (Master 视角)
class SandboxRecord:
    sandbox_id: str       # 唯一标识, 如 "sbx-abc123"
    user_id: str          # 所属用户
    node_id: str          # 所在 Worker 节点
    ip: str               # Worker IP (直连用)
    port: int             # ttyd 端口
    status: str           # PENDING | RUNNING | STOPPING | STOPPED | ERROR
    gpu: int              # 分配 GPU 数
    cpu: int              # 分配 CPU 核数
    memory: int           # 分配内存 (bytes)
    created_at: float     # 创建时间
    expires_at: float     # 过期时间
    token: str            # 连接 token
```

### 8.2 Worker 侧数据结构

```python
class LocalSandbox:
    sandbox_id: str
    cgroup_path: str      # /sys/fs/cgroup/.../sandbox_<id>
    port: int
    ttyd_pid: int         # ttyd 进程 PID
    shell_pid: int        # 沙盒内 shell 进程 PID
    status: str           # CREATING | TTYD_INIT | RUNNING | STOPPING | ERROR
    created_at: float
    last_io_at: float     # 最后 IO 时间 (用于空闲检测)
```

---

## 九、异常处理与恢复

### 9.1 异常场景矩阵

| 异常场景 | 检测方式 | Master 行为 | Worker 行为 | 用户感知 |
|----------|----------|-------------|-------------|----------|
| Worker 无响应 | 心跳超时 30s | 标记 OFFLINE，排除调度 | — | 分配到其他节点 |
| Sandbox 创建超时 | 30s 无响应 | 重试 3 次 → 返回 503 | — | 提示"资源不足，请稍后重试" |
| ttyd 进程崩溃 | Worker 进程监控 | 收到 Worker 通知，标记 ERROR | 尝试重启 ttyd（最多 1 次）→ 通知 Master | 终端断开，提示重连 |
| cgroup 脚本失败 | 脚本返回非 0 | 记录错误，尝试其他节点 | 清理残留 cgroup 目录 | 分配到其他节点 |
| 端口耗尽 | 端口分配失败 | 标记节点端口池满 | 等待已释放端口或返回错误 | 分配到其他节点 |
| Master 重启 | — | 恢复时重新向 Worker 同步状态 | 心跳恢复后上报当前所有沙盒 | 现有连接不受影响（直连 Worker） |
| Worker 重启 | Master 心跳检测 | 该节点所有沙盒标记 STOPPED | 重新注册，上报空沙盒列表 | 该节点终端全部断开 |

### 9.2 孤儿沙盒清理

```
Worker 定期巡检 (每 60s):
  1. 扫描 /sys/fs/cgroup/cpu/sandbox_* 目录
  2. 对比本地记录，发现无记录的 cgroup → 清理
  3. 发现记录中但 ttyd 进程已死的 → 标记 ERROR, 通知 Master

Master 定期巡检 (每 120s):
  1. 查询所有 SandboxRecord
  2. 超过 expires_at 的 → 通知 Worker 销毁
  3. 状态为 STOPPING 超过 60s 的 → 告警，强制清理
```

---

## 十、安全设计

### 10.1 沙盒隔离边界

```
┌─────────────────────────────────────────┐
│         宿主机 (Worker Node)             │
│                                         │
│  ┌───────────────────────────────────┐  │
│  │  cgroup: sandbox_<id>             │  │
│  │  ├─ cpu.max    = 1 核             │  │
│  │  ├─ memory.max = 1G               │  │
│  │  ├─ pids.max   = 64               │  │
│  │  ├─ devices    = GPU 0 only       │  │
│  │  │                                 │  │
│  │  │  ┌───────────────────────────┐ │  │
│  │  │  │  ttyd (-p <port>)         │ │  │
│  │  │  │  └─ bash (login shell)    │ │  │
│  │  │  │     └─ user processes     │ │  │
│  │  │  └───────────────────────────┘ │  │
│  │  └─────────────────────────────────┘  │
│  │                                       │
│  │  网络: 仅暴露 ttyd 端口给用户网段       │
│  │  存储: 每个沙盒独立 tmpfs (可选)       │
│  └───────────────────────────────────────┘
│                                         │
└─────────────────────────────────────────┘
```

### 10.2 安全措施清单

| 措施 | 实现方式 |
|------|----------|
| 资源限制 | cgroup v2: cpu.max, memory.max, pids.max |
| GPU 隔离 | devices cgroup + CUDA_VISIBLE_DEVICES |
| 进程隔离 | pids cgroup 限制 fork bomb |
| 网络隔离 | iptables 限制沙盒出站（可选） |
| 连接认证 | ttyd 的 `-c user:pass` 参数，Master 生成随机凭证 |
| 超时回收 | 2h 硬超时 + 15min 空闲超时 |
| 审计日志 | 记录所有沙盒的创建/销毁/异常事件 |

---

## 十一、配置项参考

```yaml
# master/config.yaml
master:
  host: "0.0.0.0"
  port: 25565
  scheduler_strategy: "BestFit"       # BestFit | LeastLoaded | RoundRobin
  worker_timeout_seconds: 30
  max_retries: 3
  heartbeat_timeout_seconds: 30
  orphan_check_interval_seconds: 120

sandbox:
  default_lifetime_seconds: 7200      # 2 小时
  idle_timeout_seconds: 900           # 15 分钟
  warning_before_expire_seconds: 300  # 提前 5 分钟警告
  max_sandboxes_per_user: 5
  port_range_start: 8000
  port_range_end: 9000

# worker/config.yaml
worker:
  host: "0.0.0.0"
  port: 25566
  master_url: "http://<master_ip>:25565"
  heartbeat_interval_seconds: 10
  cgroup_script_path: "/opt/neu_box/cgroup-sandbox.sh"
  orphan_check_interval_seconds: 60
  ttyd_binary_path: "/usr/local/bin/ttyd"
```

---

## 十二、状态流转汇总表

| 组件 | 状态序列 (正常) | 状态序列 (异常) | 终态 |
|------|----------------|----------------|------|
| **Master (per request)** | IDLE → SCHEDULING → WAITING_WORKER_RESPONSE → RESPONDING → IDLE | IDLE → SCHEDULING → SCHEDULING_FAILED → IDLE | IDLE |
| **Worker (per sandbox)** | IDLE → ALLOCATING → STARTING_TTYD → RUNNING → STOPPING → IDLE | IDLE → ALLOCATING → ALLOCATION_FAILED → IDLE | IDLE |
| **Sandbox** | PENDING → CREATING → TTYD_INIT → RUNNING → STOPPING → STOPPED | PENDING → CREATING → ERROR | STOPPED / ERROR |
| **Cgroup Script** | VALIDATE_ARGS → CGROUP_MKDIR → SET_LIMITS → NS_CREATE → INIT_PROCESS → PORT_ALLOC → TTYD_LAUNCH → VERIFY | 任意步骤失败 → ROLLBACK | 成功退出 / 错误退出 |

---

## 十三、代码模块映射

```
master/entry_point/
├── main.py          # Flask 应用入口, 注册蓝图, 启动服务
├── terminal.py      # Blueprint: /api/terminal (创建/销毁/查询终端)
├── schedular.py     # 调度器: 节点选择策略, 资源匹配
├── src_manager.py   # 节点管理: Worker 注册/心跳/状态跟踪
├── connection.py    # 连接管理: 向 Worker 发送 HTTP 请求
└── status.py        # 系统状态: 全局状态查询, 健康检查

worker/ (待创建)
├── main.py          # Worker 服务入口
├── sandbox.py       # 沙盒生命周期管理
├── cgroup_exec.py   # 调用 cgroup 脚本的封装
└── heartbeat.py     # 心跳上报

scripts/ (待创建)
└── cgroup-sandbox.sh  # cgroup 资源隔离脚本
```
