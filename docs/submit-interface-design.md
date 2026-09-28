# neubox 终端、容器与 submit 接口设计

日期：2026-09-27

状态：CLI、Worker 与 runtime 按本文实现；第 7 节的 WebUI 交互留待后续实现。
设备授权和 Docker 时序仍需在维护窗口做真机验证。

## 1. 已确定的接口方向

- 保留 `neubox acquire`，用 `neubox shell` 提供退出即释放的终端体验。
- 资源参数继续使用 `--device-num`，不引入 `-n` 替代写法。
- `submit` 接收一条命令或完整脚本。任务以入口命令或脚本退出为结束依据，随后清理资源。
- 需要借卡的容器通过 `neubox docker run/start/restart` 启动。Docker 业务参数继续透传。
- 容器启动后的操作使用原生 `docker exec`，沿用已有授权。
- 不引入任务专用 Docker API 代理或 `DOCKER_HOST` 连接，不通过改写脚本文本自动接管原生 Docker 命令。
- WebUI 提供资源选择和完整脚本编辑器，不要求勾选 Docker，也不要求拆分 Docker 参数。
- WebUI 不设置工作目录输入框；用户通过脚本里的 `cd` 指定目录。

常用容器任务走 `neubox docker run --rm`。复用已有容器的 `start → exec` 是补充路径，必须满足容器原启动程序适合持续运行的前提。

## 2. 沙盒、任务与容器的关系

**沙盒持有设备授权；终端会话或任务决定沙盒的生命周期；容器在启动时取得沙盒委托。**

| 使用方式 | 申请入口 | 执行业务 | 释放时机 |
| --- | --- | --- | --- |
| 终端 + Host | `shell` / `acquire` | 在宿主机运行程序 | 退出子 shell / `release` |
| 终端 + Docker | `shell` / `acquire` | 启动容器，再在其中操作 | 退出子 shell / `release` |
| Submit + Host | `submit` | 执行命令或脚本 | 入口退出后清理 |
| Submit + Docker | `submit` | 命令或脚本调用容器包装命令 | 入口退出后清理 |

一个任务可以包含多个步骤，也可以启动多个受委托容器。它们共享任务申请的设备，不会各自再获得一份独立配额。

任务入口进程用于确定结束点和业务退出码。沙盒及容器登记用于确定资源归属和清理范围。容器进程由 Docker daemon 启动，不能只通过任务入口 PID 的子进程树寻找和清理。

容器授权必须在用户程序首次访问设备之前完成。对于当前 Ascend/CANN 路径，不能指望在 `docker exec` 时补发或更换授权。Docker 的 `run`、停止容器的 `start` 以及停止后重新启动的 `restart` 才是需要管理的启动入口。

## 3. 终端接口

### 3.1 新开一个有资源的 shell

```bash
neubox shell --device-num 2
python train.py
exit
```

`shell` 排队申请资源，在沙盒内启动子 shell。子 shell 退出后清理沙盒和受委托容器，再返回原来的终端。

### 3.2 让当前 shell 获得资源

```bash
neubox acquire --device-num 2
python train.py
neubox release
```

`acquire` 保留现有语义，当前 shell 进入沙盒，不额外打开一层 shell。

| 命令 | 用途 |
| --- | --- |
| `neubox status` | 查询当前 shell 的沙盒与资源 |
| `neubox list` | 列出所有用户的活跃任务、申请和沙盒；`--sandboxes` 只看沙盒 |
| `neubox release` | 释放当前 shell 的沙盒 |
| `neubox release SANDBOX` | 释放指定沙盒 |

### 3.3 在终端中使用容器

新建容器：

```bash
neubox shell --device-num 2
neubox docker run --rm -it -v /data:/data IMAGE bash
```

启动已有停止容器并进入：

```bash
neubox shell --device-num 2
neubox docker start dev
docker exec -it dev bash
```

第二个示例要求 `dev` 是可被 runtime 管理的容器，其原启动程序能够保持运行。

| 命令 | 行为 |
| --- | --- |
| `neubox docker run ...` | 透传 Docker 参数，启动时绑定当前沙盒 |
| `neubox docker start CONTAINER` | 启动停止容器，绑定当前沙盒 |
| `neubox docker restart CONTAINER` | 明确停止运行中的容器，再按当前沙盒重新启动 |
| `neubox docker status CONTAINER` | 查询容器状态、当前沙盒和授权设备 |
| `docker exec ...` | 在运行中的容器执行命令，沿用已有授权 |
| `neubox docker exec ...` | 报错并引导使用原生 exec，说明 exec 不能借卡 |
| `docker stop CONTAINER` | 停止容器，终端持有的沙盒仍然存在 |

普通 `start` 不自动重启运行中的容器，也不把已经运行视为本次绑定成功。主动中断并换卡使用 `restart`。

沙盒释放时，其委托容器也进入清理流程。退出 `docker exec` 的交互 shell 本身，不等于释放外层 `neubox shell/acquire` 持有的资源。

## 4. submit 接口与示例

主入口为：

```text
neubox submit [资源选项] -- COMMAND [ARGS...]
neubox submit [资源选项] --script FILE|-
```

`--` 后的参数属于目标程序。`--script FILE` 在提交时读取文件，`--script -` 在提交时读取 stdin，并把内容保存为任务快照。排队以后修改本地文件不改变已提交的脚本。

脚本引用的其他文件和 Docker 挂载路径仍然位于执行节点，不随脚本自动上传。

### 4.1 宿主机命令和多步任务

```bash
neubox submit --device-num 2 -- python train.py --epochs 10
```

```bash
neubox submit --device-num 2 --script - <<'SH'
set -e
python prepare.py
python train.py --epochs 10
SH
```

命令或脚本在宿主机沙盒内运行。脚本结束后清理沙盒中的剩余进程。

### 4.2 新建容器执行任务：主路径

```bash
neubox submit --device-num 2 -- neubox docker run --rm \
  -v /data:/data \
  -e HF_HOME=/data/hf \
  -w /data \
  --shm-size 16g \
  training:latest python train.py --epochs 10
```

用户在原来的 `docker run` 前增加 `neubox`，其余 Docker 参数完整透传。neubox 不要求把镜像、挂载、环境变量等拆成另一套参数。

执行链为：

```text
任务排队并分配资源
  → 启动沙盒内的 neubox docker run
  → 原生 Docker 创建并启动容器
  → runtime 在业务程序启动前完成沙盒委托登记
  → 前台 docker run 等待容器工作负载结束
  → 任务入口退出
  → 清理并释放资源
```

容器中需要多步执行时，可以让容器主程序直接运行脚本。以下示例要求镜像支持按此方式启动 Bash：

```bash
neubox submit --device-num 2 -- neubox docker run --rm \
  -v /data:/data -w /data \
  training:latest \
  bash -c 'set -e; python prepare.py; python train.py'
```

这些步骤本身构成容器的前台工作负载，不需要额外准备一个仅用于保持容器存活的主程序。镜像有专门 ENTRYPOINT 时，按镜像自身的启动约定传参。

### 4.3 已有停止容器：运行原来配置的程序

```bash
neubox submit --device-num 2 -- neubox docker start train-1 -a
```

这里沿用当前包装命令的参数顺序：容器名在前，Docker start 的其他参数在后。

`-a` 附着到容器并等待本次运行结束。容器原来的 ENTRYPOINT/CMD 就是工作负载，不额外替换程序。原程序是训练时等待训练结束；原程序是服务时任务可以持续运行，由用户取消结束。

### 4.4 已有停止容器：启动后执行额外命令

```bash
neubox submit --device-num 2 --script - <<'SH'
set -e
neubox docker start dev
docker exec dev python /workspace/prepare.py
docker exec dev python /workspace/train.py
SH
```

执行顺序：

```text
start 启动并确认授权后返回
  → 第一个 exec 等待 prepare.py 结束
  → 第二个 exec 等待 train.py 结束
  → 脚本退出
  → Worker 停止本任务登记的 dev
  → 清理并释放资源
```

无需在脚本末尾补 `docker stop`，结束清理由 Worker 负责。

**前提：容器原启动程序适合持续运行，而且运行它本身符合用户意图。**

- 如果原程序是另一份训练，`start` 会先启动那份训练，随后 exec 会再运行新的训练。
- 如果原程序很快退出，exec 无法继续依赖该容器运行。
- 如果原程序是服务，容器启动成功不等于服务已经就绪；依赖服务的脚本需要自行检查就绪条件。

这种形式用于复用常驻环境，不承诺任意停止容器都能直接运行一条新的业务命令。

### 4.5 已经运行的容器

| 情况 | 授权和使用规则 |
| --- | --- |
| 容器由本任务启动并已登记 | 后续原生 exec 沿用本任务授权 |
| 容器在别的沙盒中运行 | 新任务不能通过 exec 把本次申请的卡交给它 |
| 容器运行但没有设备授权 | exec 不能补发授权；需要从停止状态重新受管启动 |

新任务不自动停止或接管正在使用的外部容器。需要主动中断并换卡时，在终端显式使用受管 `restart`。

本方案不代理原生 Docker，因此也不承诺逐条拦截脚本中的原生 `docker exec`。直接对任务外容器执行 exec，只会使用那个容器原来的授权，并受 Docker 本身的访问权限约束；它不因此成为当前任务的受托容器。当前任务取消时，不应误停那个外部容器。

受管任务的约定写法是先通过本任务的 `neubox docker run/start` 完成绑定，再执行 exec。

## 5. 结束点、退出码与清理

### 5.1 统一结束规则

**入口命令或脚本退出，任务开始清理；清理完成后，资源才能重新分配。**

| 任务内容 | 正常结束点 |
| --- | --- |
| `python train.py` | Python 退出 |
| 前台 `neubox docker run ...` | 前台 Docker 命令结束，正常对应容器工作负载结束 |
| `neubox docker start C -a` | 附着的本次容器运行结束 |
| `start → exec → exec` 脚本 | 顺序命令执行完，脚本退出 |
| 只有 `neubox docker start C` | 启动并确认后很快返回 |
| 只有 `neubox docker run -d ...` | 容器启动后很快返回 |

最后两种写法会让任务很快进入清理，刚启动的容器也会被停止。不会静默删除 `-d`，也不会隐式改成等待所有容器停止。

脚本中允许启动后台工作，但必须用前台工作或显式等待表达完成条件。宿主机后台进程同样需要脚本自行等待。

`docker wait` 打印容器退出码。需要让脚本反映业务结果时，要把打印的值转换为脚本退出码，而不能只看 Docker 客户端是否调用成功。

### 5.2 脚本和任务结果

- 脚本遵循解释器的退出规则。示例中的 `set -e` 用于让简单顺序命令失败时停止，不把它当作所有复杂脚本的错误处理保证。
- 原始业务退出码与任务状态分别保存。
- 取消、超时、启动错误和清理错误不能因为业务退出码为零而被显示为成功。
- 脚本的正常 stdin 不承担交互输入。`--script -` 是提交时读取脚本，脚本内部仍可通过管道或 heredoc 给 `docker exec -i` 等命令提供输入。
- 需要人交互的终端任务使用 `shell/acquire`，批任务不提供隐含的交互终端。

### 5.3 取消与容器去向

清理时禁止新增委托，终止沙盒中的剩余宿主进程，并停止登记在本沙盒下的容器。仅杀掉 `docker exec` 客户端不能代表容器内程序已结束。

容器的删除行为遵循用户的 Docker 配置：有 `--rm` / AutoRemove 的按 Docker 规则删除，其余停止并保留。neubox 不因任务结束而默认删除已有容器，也不为修改其启动命令偷偷重建容器。

清理失败需要显示残留占用并继续处理，不能提前把卡交给下一项任务。Worker 重启或异常退出后的资源对账也必须遵守相同归属规则。

## 6. 实现职责与约束

### 6.1 CLI 和 Worker

任务描述包含资源、执行用户、节点、工作目录、环境变量，以及命令参数数组或脚本快照。

- `-- COMMAND ...` 保存参数边界，避免通过重新拼接改变引号含义。
- `--script` 保存原文，保留换行、缩进、注释和 heredoc。
- WebUI 编辑器按脚本提交，不再裁剪每行后用 `&&` 拼接。
- 脚本由交互式 Bash 执行，首行 shebang 当前不会改变解释器。
- Bash 加载目标用户的 `~/.bashrc`，保留 Conda/PATH 等环境初始化需求。
- 脚本和命令入口必须在加入沙盒之后才开始执行业务。

Docker 业务参数交由 Docker 自己解释。现有包装命令负责传递沙盒信息和发起 start intent，不增加一张完整的 Docker run 参数表，不解析脚本来猜测容器类型。

### 6.2 启动授权

`neubox docker start` 从自身 PID 所在的沙盒取得委托，不另行申请一份资源。因此它既能用于终端，也能用于任务脚本。

目标契约要求：

- 借卡失败必须返回非零，不能仅警告后继续执行后续业务。
- 授权失败不应把容器留在“启动成功但没有预期设备”的状态。
- 完整绑定必须发生在容器用户程序首次访问设备之前。
- `start -a` 的授权验证不能等到前台命令退出后，才回查一个已经过期的短时借条。
- 短任务、`--rm`、并发启动、取消与启动交错时，授权和清理仍要对应同一次容器运行。
- 进入清理状态的沙盒拒绝新增委托。

当前 runtime 依赖容器上的 annotation 触发登记。支持没有旧 annotation 的普通停止容器，需要扩展启动授权协议；不能把它当作包装命令天然具备的能力。具体支持范围必须在 help 中写明。

沙盒归属不等同于 Docker 容器的可信用户属主。并发使用和可操作权限沿用实际可验证的身份规则，不凭一个用户可自行填写的 label 承诺容器独占所有权。

### 6.3 资源作用范围

设备通过沙盒委托给容器。容器仍处于 Docker 自己的 cgroup 中，不会自动继承宿主任务 cgroup 的 CPU、内存限制。

- 宿主进程的 CPU、内存限额由宿主沙盒执行。
- 容器限额可使用原生 Docker 参数表达。
- 如果要让任务的 `--cpu/--mem` 表示宿主进程与全部容器的合计限额，需要单独实现统一配额。
- 在合计限制实现前，help 和 WebUI 必须写清楚限额作用范围，不能把同一限额复制给多个容器后称为任务总额。

任意脚本中的镜像拉取通常发生在任务执行阶段，会占用已经申请的时间和资源。此接口不承诺通过分析脚本文本提前完成镜像准备。

## 7. WebUI（后续交互设计，当前未实现）

### 7.1 提交页面

主要输入为执行节点、资源申请、完整命令或多行脚本。无需选择 Host/Docker、已有/新建、运行/停止，也无需拆分镜像、挂载和环境变量。

```text
执行节点       [ worker-01          ]
申请设备       [ 数量 2 / 指定设备  ]

命令 / 脚本
┌───────────────────────────────────────────────────┐
│ neubox docker run --rm \                           │
│   -v /data:/data \                                │
│   --shm-size 16g \                                │
│   training:latest python /data/train.py            │
└───────────────────────────────────────────────────┘

[插入示例]  [查看容器]

脚本结束后，停止本任务绑定的容器并释放资源。
容器是否删除遵循 Docker 的 --rm 配置。

                                             [提交]
```

用户粘贴业务命令，不粘贴外层 `neubox submit`。从 Docker 文档复制命令时，在需要借卡的 `docker run/start` 前增加 `neubox`，业务参数保持原样。

编辑器内容在宿主机执行。只有明确传给 `docker run` 或 `docker exec` 的程序才在容器里运行，后续普通脚本行不会自动进入容器。

### 7.2 工作目录

WebUI 不提供工作目录输入框。脚本默认从执行节点上该用户的 home 目录开始，需要时显式切换：

```bash
set -e
cd /data/project
python prepare.py
python train.py
```

宿主脚本的 `cd` 不改变容器内部目录；容器目录通过 Docker 的 `-w` 等原生能力设置。

CLI 继续默认使用提交时的当前目录，并保留已有 `--workdir`。任务记录应保存实际初始目录，供结果查询和复现使用。

### 7.3 辅助模板与容器查询

“插入示例”可提供宿主机多步任务、新容器任务、已有容器运行原程序、已有常驻容器中执行额外命令四种模板。模板生成可编辑脚本，不引入额外任务类型。

“查看容器”展示容器名、运行状态、当前授权、原启动命令。原启动命令帮助用户判断应使用 `start -a`，还是适合 `start → exec`。

不为外部运行容器提供自动接管操作。页面预检只反映检查当时的状态；实际执行时仍需验证。对于任意脚本，不承诺在入队前完整推断它将操作哪些容器。

### 7.4 任务详情

展示任务状态、清理阶段、申请和实际分配的设备、原始命令或脚本、实际初始目录、日志、业务退出码、失败原因，以及实际登记的关联容器。

主日志来自任务入口的 stdout/stderr。已有容器的主程序日志不一定出现在 exec 的输出中，可在关联容器中提供单独查看入口。

排队时可取消排队，运行时可取消任务。关闭网页只停止查看，不取消任务。

## 8. 查询、取消、help 与输出风格

```bash
neubox tasks
neubox result TASK_ID
neubox wait TASK_ID
neubox cancel TASK_ID
neubox submit --wait --device-num 2 -- python train.py
neubox help
neubox docker help
```

| 操作 | 含义 |
| --- | --- |
| `submit` 返回成功 | 已受理入队，不代表业务成功 |
| `wait` / `submit --wait` | 查看日志并等待任务结果 |
| 停止等待或关闭查看终端 | 不自动取消已提交的任务 |
| `cancel` | 取消任务并触发清理 |
| `result` | 查询状态、业务退出码及失败原因 |

neubox 自身元数据采用顶格、对齐的两列输出，不加标题或 `next`。业务日志和原生 Docker 输出保留原样。`status` 与 `acquire` 使用一致的资源字段，例如：

```text
sandbox : sbx_yuxd_199460.slice
devices : 0, 1
cpu     : unlimited
memory  : unlimited
```

容器状态必须区分运行状态与当前授权。历史 annotation 不代表当前仍然有卡，Worker 不可达时也不能把未知授权显示成已绑定。

help 必须说明入口进程决定任务结束、start 会执行原启动程序、exec 不会重新借卡、后台命令需要显式等待，以及任务结束时容器的处理方式。

## 9. 后续能力边界

以下事项没有在本次接口讨论中定为完整实现规格：

- 无 annotation 的已有容器是否纳入首版支持，以及相应 runtime 协议。
- CPU、内存合计配额的实现范围。
- 特权、设备、runtime 和自动重启配置的准入规则。参数透传不能绕过实际授权，也不意味着自动具备服务托管能力。
- 取消宽限期、超时配置与 `wait` 的退出码映射；业务退出码和任务状态仍需分开保存。
- `docker start` 借条目前只在 Worker 内存中；Worker 在借条与 hook 登记之间重启时，
  旧 annotation 若仍指向活沙盒，容器可能短暂沿用旧授权。彻底消除需要持久化
  借条或在 Docker 启动链路传递可核验的启动实例标识。

旧的 `submit --image` 入口暂时保留，新的容器任务建议使用 Host 入口调用
`neubox docker run/start`。第 7 节的页面交互需要在 WebUI 仓库落实；CLI 和
Worker 的完成不代表页面已经具备这些能力。

## 10. 验收场景

| 场景 | 预期 |
| --- | --- |
| Host 多行、注释、续行、heredoc | 原文按脚本解释，不被拼接破坏 |
| 前台 `neubox docker run --rm` | 启动前授权，业务结果可见，结束后无资源残留 |
| 已有容器 `start -a` | 等待原程序结束，长任务不因借条过期误报绑定失败 |
| `start → exec → exec` | 前台步骤顺序等待，脚本结束后停止本任务容器 |
| 容器原程序提前退出 | exec 失败可见，不伪装成正常完成 |
| 借卡失败 | 返回非零，简单 `set -e` 脚本不继续执行后续业务 |
| 只有后台启动命令 | 入口返回后触发清理，不隐式等待容器自然结束 |
| 取消宿主或容器任务 | 入口、剩余宿主进程和受托容器都按归属收尾 |
| 任务外运行容器 | 不通过 exec 改绑，不被当前任务清理误停 |
| 启动与取消竞争、短任务、`--rm` | 不漏登记、不串绑，不把失效授权用于新一次启动 |
| 清理失败或 Worker 重启 | 明确反映占用并对账，不提前重复分配设备 |

涉及设备授权和 Docker/runtime 时序的场景需要真机验证，不能仅用客户端单元测试替代。

## 11. 相关实现与参考

- [CLI 帮助与现有接口](../native/client/internal/cli/help.go)
- [Host 执行器](../src/neu_box/execution/host.py)
- [任务调度与结束清理](../src/neu_box/scheduling/queue.py)
- [容器归属登记](container-registration.md)
- [隔离模型](isolation.md)
- [Runtime hook](../native/runtime/docs/runtime-hook.md)
- [Docker exec：运行条件与主进程约束](https://docs.docker.com/reference/cli/docker/container/exec/)
- [Docker start](https://docs.docker.com/reference/cli/docker/container/start/)
- [Docker start 前台等待与退出码实现](https://github.com/docker/cli/blob/master/cli/command/container/start.go)
- [Docker wait：输出容器退出码](https://docs.docker.com/reference/cli/docker/container/wait/)
