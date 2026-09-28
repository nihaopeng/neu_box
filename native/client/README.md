# Neu Box CLI (neubox)

Neu Box 的终端沙盒隔离 / 命令任务提交客户端。Go 单文件静态二进制，
**直连 Worker**（不经过 WebUI）。与 Worker 和 OCI runtime 同仓、同版本号，
由同一个 RPM 交付。

## 命令

```
neubox shell [资源选项...]         新开宿主机子 shell，退出自动释放沙盒
neubox acquire [选项...]       为当前终端同步申请沙盒
neubox submit [选项...] -- CMD 异步提交命令任务
neubox submit [选项...] --script FILE|-
                               提交脚本快照（- 从标准输入读取）
neubox release [sandbox_name]  不传名称时释放当前 shell 的沙盒
neubox cancel <id> [--kind task|acquire]
                               取消排队中/运行中的条目（acquire 已拿到卡则就地释放）
neubox docker run DOCKER_ARGS 透传 docker run，自动补沙盒 annotation
neubox docker start 容器 [参数] 启动停止容器并借当前沙盒；-a 等原程序结束
neubox docker restart 容器    停止运行中容器并借当前 shell 沙盒重新启动
neubox docker status 容器     查询 Docker 状态与 Worker 当前授权
neubox docker exec 容器       报错并提示使用原生 docker exec
neubox {list|status|join}      沙盒管理
neubox tasks [--all | --since 4h]
                               任务队列（默认：活跃任务 + 近 2h 结束的）
neubox {result|log} TASK_ID    结果快照 / 完整日志
neubox wait TASK_ID            增量跟踪日志并等待任务结束
neubox check                   检查 worker 可达性与 API 版本兼容性
neubox [--json] version
neubox help [docker]           查看命令用法与容器生命周期说明
```

资源选项可用于 `shell`、`acquire` 和 `submit`：

| 选项 | 说明 |
|---|---|
| `--device 1` | 指定一张卡，可重复 |
| `--devices 1,3` | 指定卡号（与 --device-num 互斥） |
| `--device-num 2` | 自动分配卡数量；不指定任何设备选项时默认 1，`--device-num 0` 表示不申请卡 |
| `--cpu 4` / `--mem 8` | 宿主沙盒资源上限，0 = 不限 |

未指定任何设备选项（`--device`/`--devices`/`--device-num` 及 acquire 位置参数）时，
设备数默认为 1；显式传 `--device-num 0` 表示不申请设备。

`acquire` 专属选项：

| 选项 | 说明 |
|---|---|
| `--pid 12345` | 指定 PID（默认当前 shell 的父进程） |
| （无容器选项） | acquire 只接受宿主机 PID；申请后可用 `neubox docker run` 启动容器 |

`neubox shell --device-num 2` 把 `neubox` 进程和新开的交互子 shell 放进沙盒，
原来的宿主机 shell 不变。退出子 shell 后自动释放沙盒；长期持有或需要把当前
shell 加入沙盒时继续使用 `acquire` / `release`。`shell` 不接受 `--pid`，
默认启动 `$SHELL -i`，未设置 `$SHELL` 时使用 `/bin/sh -i`。

命令任务统一使用 `submit`，其专属选项为：

| 选项 | 说明 |
|---|---|
| `--priority 1` | 队列优先级：0=普通，数值越大越先执行 |
| `--image IMAGE` | 使用 Worker 创建并登记的一次性容器 |
| `--wait` | 提交后直接跟踪日志和退出状态；中断跟踪不取消任务 |
| `--workdir PATH` | 工作目录；Host 默认提交时所在目录 |
| `--container-user USER` | 容器命令用户 |
| `--env KEY[=VALUE]` | 显式传入环境变量；省略值时取提交终端的值（可重复） |
| `--project` | Docker：把当前目录可写挂载到 `/workspace`，并默认在其中运行 |
| `--mount HOST:CONTAINER[:ro|rw]` | Docker：挂载已有宿主路径；默认只读（可重复） |
| `--output HOST[:CONTAINER]` | Docker：创建并挂载可写输出目录；容器内默认 `/outputs` |
| `--command "..."` | 命令字符串；也可把命令及参数放在 `--` 后 |
| `--script FILE|-` | 提交时读取脚本原文并保存快照；`-` 从标准输入读取；与 `--command` / `--` 互斥 |

`--` 后的命令按参数数组提交，保留每个参数的边界。`--script` 保留脚本的换行、
缩进、注释和 heredoc；修改本地脚本文件不会改变已经排队的任务。脚本里引用的
其他文件仍须存在于执行节点，客户端只上传脚本本身。脚本按宿主机任务运行，
由 Bash 解释（首行 shebang 不切换解释器），执行时 stdin 是 `/dev/null`；
可在脚本里使用管道、heredoc 和 `neubox docker run/start`。

默认输出为顶格、标签对齐的两列 `key: value`，没有标题或 `next` 提示；
CPU / 内存无限制显示为 `unlimited`，没有设备显示为 `none`，查询失败则显示
`unknown`，不能把未知当成无卡。自动化调用可将
`--json` 放在子命令前或紧跟子命令；成功结果写入 stdout，失败对象写入
stderr。

## 容器

容器快捷命令使用同仓的 Worker API 和 OCI runtime/hook。部署时要安装
单个 RPM，再运行 `neuboxctl setup` 配置 Docker 默认 runtime；需要重启 Docker 时
按提示选择，步骤见 [部署手册](../../docs/deployment.md)。

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

`neu-box-runtime` 随同一个 `neuboxd` RPM 安装；`neuboxctl setup` 配置 Worker
地址，在 Docker 节点查找真正的 runc，并设置 Docker 的 `default-runtime`。

`run` / `start` / `restart` / `status` 以外的 docker 子命令（build / ps / compose / …）不支持，
这些操作直接使用原生 Docker 命令。提交容器任务时，推荐在 `submit` 的命令或
脚本中调用 `neubox docker run/start`；Docker 业务参数照原样透传。

`docker start` 面向**已经停着**的容器：annotation 在建容器时就写死了、改不了，
所以 start 走"借条"模型 —— 启动前把当前 shell 所在沙盒借给这个容器
（`POST /container/intent`，10 秒内有效、一次性），容器启动后 Worker 的
create/启动 hook 登记成功后确认借条；客户端随后回查状态（`GET
/container/intent`）。借卡或确认失败时，`neubox docker start` 返回非零，并在
已经启动容器时尝试停止它。跨 shell 换沙盒继续使用同一个受管容器要走
`neubox docker start`。原生 `docker start` 不建立新的借条，不能用它换卡。

`neubox docker start CONTAINER -a` 会附着并等待原来的 ENTRYPOINT/CMD 结束。
客户端在等待期间确认授权，随后把 Docker 的业务退出码返回给调用者。容器必须
在创建时带有受管 annotation；正在运行的容器不能通过 `start` 重新借卡。
`start --checkpoint` 恢复不经过当前的设备登记路径，因此受管启动会拒绝。

`neubox docker status CONTAINER` 把 Docker 的运行状态和 Worker 的当前登记合并显示。
已停止容器没有当前授权；运行中但未登记的容器也没有 NeuBox 授权。
Worker 查询失败显示 `unknown`。设备列表是授权记录，不代表驱动健康状况。

运行中的受管容器要换卡，先在持有目标沙盒的 shell 中执行
`neubox docker restart CONTAINER`。它先检查容器创建时的 annotation 和 Worker
可达性，然后 `docker stop`，等旧登记撤销，再存借条并 `docker start`，最后确认
借条被认领。重启会中断容器里的工作；没有 annotation 的普通容器无法靠重启
获得设备，需用 `neubox docker run` 新建。已停止容器使用 `neubox docker start`。

运行中的容器执行新命令，直接用 `docker exec -it CONTAINER bash`。这不会改变
容器的设备授权。`neubox docker exec` 只报错并指向原生命令和
`neubox docker status`，不会转发 Docker 参数。

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
退出 0，`failed` 退出 1，`cancelled` 退出 130。中断或本地超时只停止跟踪，
不会取消远端任务。

`submit` 默认返回任务 ID；`submit --wait` 会在提交后直接执行同样的增量跟踪。
Host 任务会从提交时的工作目录启动，不会自动复制提交终端的所有环境变量；
需要的变量用 `--env KEY` 或 `--env KEY=VALUE` 明确传入。

容器任务的常用写法是把已有的 `docker run` 命令前加上 `neubox`：

```bash
neubox submit --device-num 2 -- neubox docker run --rm \
  -v /data:/data -w /data training:latest python train.py
```

已有停止容器原来的启动命令就是业务时：

```bash
neubox submit --device-num 2 -- neubox docker start train-1 -a
```

需要在已有常驻容器中顺序运行额外命令时：

```bash
neubox submit --device-num 2 --script - <<'SH'
set -e
neubox docker start dev
docker exec dev python /workspace/prepare.py
docker exec dev python /workspace/train.py
SH
```

这个脚本要求 `dev` 原来的主程序保持运行，并且运行它本身符合用户意图。
`docker exec` 沿用容器启动时得到的授权，不能给已经运行的容器换卡。
任务以入口命令或脚本退出为结束点；结束后 Worker 停止本任务登记的容器并
清理沙盒。单独的 `neubox docker start C` 或 `neubox docker run -d ...` 会很快
返回，随后任务开始清理。`--rm` 由 Docker 负责删除容器；没有 `--rm` 的容器
停止后保留。

`submit --cpu/--mem` 约束宿主任务沙盒内的进程。脚本启动的 Docker 容器有
自己的 cgroup；需要限制其 CPU、内存时，在 `docker run` 中写 Docker 原生参数。

旧的 `--image` 形式仍可使用项目和持久化输出：

```bash
neubox submit --image training:v1 --project --output ./runs/exp1 \
  --mount /datasets:/datasets:ro --wait -- python train.py
```

`--project` 映射当前目录到 `/workspace` 且可写；`--output` 映射创建好的宿主目录
到容器内 `/outputs`。这些路径必须位于 Worker 节点或共享文件系统上，客户端不会
上传文件。容器默认用户如果是 root，写回宿主的文件也可能由 root 拥有；可用
`--container-user "$(id -u):$(id -g)"` 指定用户。任务结束时一次性容器会删除，
bind mount 里的输出文件会保留。

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
neubox release

# 临时宿主机终端，退出自动释放
neubox shell --device-num 2

# 提交高优先级任务（4 卡）
neubox submit --device-num 4 --priority 1 -- python train.py

# 队列 / 结果
neubox tasks
neubox tasks --all           # 含较早结束的任务
neubox wait <task_id>
neubox result --json <task_id>

# 容器任务
neubox submit --device-num 2 -- neubox docker run --rm ubuntu echo done

# 运行中的受管容器：查看授权，进入容器，或换卡后重启
neubox docker status my-container
docker exec -it my-container bash
neubox docker restart my-container
```
