# Neu Box CLI (neubox)

Neu Box 的终端沙盒隔离 / 命令任务提交客户端。Go 单文件静态二进制，
**直连 worker**（不经过 WebUI）。2026-08-25 拆分独立仓库后，本客户端于
2026-09-24 重新整合回 `neu_box` 主仓库（`client/neubox/`），与 Worker 同仓、
同版本号、同一发布包交付。

## 命令

```
neubox acquire [选项...]       为当前终端同步申请沙盒
neubox submit [选项...] -- CMD 异步提交命令任务
neubox release <sandbox_name>  释放沙盒
neubox cancel <id> [--kind task|acquire]
                               取消排队中/运行中的条目（acquire 已拿到卡则就地释放）
neubox docker run DOCKER_ARGS 透传 docker run，自动补沙盒 annotation
neubox docker start 容器 [参数] 借条式改绑：把当前 shell 的沙盒借给停着的容器
neubox {list|status|join}      沙盒管理
neubox tasks [--all | --since 4h]
                               任务队列（默认：活跃任务 + 近 2h 结束的）
neubox {result|log} TASK_ID    结果快照 / 完整日志
neubox wait TASK_ID            增量跟踪日志并等待任务结束
neubox check                   检查 worker 可达性与 API 版本兼容性
neubox [--json] version
```

资源选项可用于 `acquire` 和 `submit`：

| 选项 | 说明 |
|---|---|
| `--device 1` | 指定一张卡，可重复 |
| `--devices 1,3` | 指定卡号（与 --device-num 互斥） |
| `--device-num 2` | 自动分配卡数量；不指定任何设备选项时默认 1，`--device-num 0` 表示不申请卡 |
| `--cpu 4` / `--mem 8` | 资源上限，0 = 不限 |

未指定任何设备选项（`--device`/`--devices`/`--device-num` 及 acquire 位置参数）时，
设备数默认为 1；显式传 `--device-num 0` 表示不申请设备。

`acquire` 专属选项：

| 选项 | 说明 |
|---|---|
| `--pid 12345` | 指定 PID（默认当前 shell 的父进程） |
| （无容器选项） | acquire 只接受宿主机 PID；容器应使用 Worker 创建的一次性任务 |

命令任务统一使用 `submit`，其专属选项为：

| 选项 | 说明 |
|---|---|
| `--priority 1` | 队列优先级：0=普通，数值越大越先执行 |
| `--image IMAGE` | 使用 Worker 创建并登记的一次性容器 |
| `--workdir PATH` | 容器命令工作目录 |
| `--container-user USER` | 容器命令用户 |
| `--env K=V` | 容器命令环境变量（可重复） |
| `--command "..."` | 命令字符串；也可把命令及参数放在 `--` 后 |

默认输出为适合终端阅读的摘要，不混入 Worker 原始 JSON。自动化调用可将
`--json` 放在子命令前或紧跟子命令；成功结果写入 stdout，失败对象写入
stderr。

## 容器

容器快捷命令使用同仓的 Worker API 和 OCI runtime/hook。部署时要安装
单个 RPM，手动配置 Docker 默认 runtime，并在维护窗口重启
dockerd；步骤见 [部署手册](../../docs/deployment.md)。

容器要拿到设备，**必须带 `sandbox_cgroup` annotation**：Worker 靠它把容器登记到
沙盒名下，没登记的容器即使卡空着也一律拿不到设备（fail-closed）。annotation 是
传输通道不是凭证，真正的校验在 Worker 侧。

`neubox docker run` 只做一件事 —— 把这行 annotation 拼出来，剩下的参数原样
透传给 docker：

```bash
neubox acquire
neubox docker run --rm -it ubuntu bash
```

展开后是：

```bash
docker run --annotation sandbox_cgroup=<沙盒名> --rm -it ubuntu bash
```

沙盒名按本进程 PID 反查（`GET /sandbox/status?pid=<自己>`）；查不到直接报错，
不会退化成"不加 annotation 照样起"。

`neu-box-runtime` 随同一个 `neuboxd` RPM 安装；用户需用 `neu-box-config init`
设置真正的 runc，再手动设置 Docker 的 `default-runtime`。

`docker run` 以外的 docker 子命令（build / ps / compose / …）不支持，同样直接
写原生 docker，或者继续用 Worker 的 `submit --image`。

`docker start` 面向**已经停着**的容器：annotation 在建容器时就写死了、改不了，
所以 start 走"借条"模型 —— 启动前把当前 shell 所在沙盒借给这个容器
（`POST /container/intent`，10 秒内有效、一次性），容器启动后 Worker 的
create/启动 hook 认领借条完成改绑；客户端随后回查借条状态（`GET
/container/intent`），没认领就明确警告"容器起来了，但里面看不到设备"。跨
shell 换沙盒继续用同一个容器要走 `neubox docker start`；直接敲原生
`docker start` 没有借条，容器起得来但零卡。

## 任务日志

`tasks` 默认只显示活跃任务（queued/running）和最近 2h 内结束的任务，避免每次
调用都刷出 worker 保留的全部历史记录；`--all` 显示 worker 返回的全部条目，
`--since 4h` 可自定义时间窗（如 `30m` / `12h`）。

`result` 返回调用时的状态与完整日志快照。长任务应使用：

```bash
neubox wait TASK_ID
neubox wait TASK_ID --interval 5s --timeout 2h
```

`wait` 使用日志接口的字节 offset 只读取新增部分，同时轮询任务状态；进入终态后
再拉取一次剩余日志。日志写到 stdout，状态变化写到 stderr，任务 `completed`
退出 0，`failed` 退出非 0。中断或本地超时只停止跟踪，不会取消远端任务。

## 环境变量

| 变量 | 说明 |
|---|---|
| `NEU_BOX_URL` | worker 地址，默认 `http://127.0.0.1:59075` |
| `NEU_BOX_USER` | 用户名（默认取 $USER） |

## 与 worker 的兼容

客户端与 Worker 同版本号（构建时由 `deploy/build_release.py` 以 ldflags 注入
`src/neu_box/__init__.py` 的 `__version__`），不再有独立的客户端版本线。

`neubox check` 查询 worker `/healthz`：

- `api_version >= 2` → 兼容 ✓
- 有 `api_version` 但 < 2 → 退出码 1（不兼容）
- 无 `api_version` 字段（旧版 worker）→ 退出码 1（不支持 `/tasks`）

排队 acquire、单条取消、按 PID 查询沙盒、容器登记与 start 借条均在本仓库的
Worker API v2 中实现；接口细节见 [Worker API](../../docs/worker-api.md)。

## 构建与安装

需要 **Go >= 1.18**（用到 `any`/`strings.Cut`/`-buildvcs`）；工具链要求由
`go.mod` 的 `go` 指令保证，过旧的工具链会直接构建失败。

```bash
./scripts/build.sh                 # → neubox（本机架构，版本号取自 Worker __version__）
GOARCH=arm64 ./scripts/build.sh    # 交叉构建
go test ./... && go vet ./...
sudo install -m 0755 neubox /usr/local/bin/neubox
```

正常部署不需要手工构建：仓库根的 `deploy/build_release.py` 会构建静态
`neubox` 并放入 `neuboxd` RPM。安装后是 `/usr/local/bin/neubox`，
`/usr/local/bin/neu-sbox` 是兼容符号链接；与 Worker 一起升级。

| 变量 | 说明 |
|---|---|
| `NEUBOX_GO` | go 不在 PATH 时指定，如 `NEUBOX_GO=$HOME/go/bin/go` |
| `GOARCH` | 目标架构，默认本机 |

## 示例

```bash
# 终端独占 2 张卡
neubox acquire --device-num 2
# 退出前释放
neubox release "$(neubox list | grep current)"

# 提交高优先级任务（4 卡）
neubox submit --device-num 4 --priority 1 -- python train.py

# 队列 / 结果
neubox tasks
neubox tasks --all           # 含较早结束的任务
neubox wait <task_id>
neubox result --json <task_id>

# 容器任务
neubox submit --image ubuntu -- bash
```
