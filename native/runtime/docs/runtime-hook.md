# neu-box-runtime / neu-box-hook：运行时侧行为

这份文档讲 **runtime 这一侧怎么做**：wrapper 怎么注入、hook 怎么登记、装机和
配置怎么写、哪些验过哪些没验过。

相关实现和契约在同一仓库的其他目录：

| 想看什么 | 去哪 |
|---|---|
| `/container/register` 的字段、状态码、错误码、幂等语义；沙盒状态与锁序；为什么必须卡在 ENTRYPOINT 之前 | [`../../../docs/worker-api.md`](../../../docs/worker-api.md)「登记容器归属（runtime hook 专用）」 |
| Worker 这一侧的流程（谁调、什么时候调、失败会怎样） | [`../../../docs/container-registration.md`](../../../docs/container-registration.md) |
| 客户端（`neubox`）怎么用、annotation 谁拼的 | [`../../../native/client/README.md`](../../../native/client/README.md) |

装机步骤（合包 RPM、卸载、升级）在 [`../README.md`](../README.md)；
这份文档只讲**为什么**那些步骤是这个顺序。

## 链路

```
docker run --annotation sandbox_cgroup=<name> ...
        │
        ▼
   dockerd → containerd → neu-box-runtime（runc wrapper）
                              │ create 时读 bundle/config.json，
                              │ 有 annotation 就注入 hook（默认 createRuntime）
                              ▼
                          真 runc：建 namespace/cgroup
                              │
                              ▼
                        neu-box-hook（stdin 读 OCI state）
                              │ 取容器的真实 cgroup / mnt ns
                              ▼
                    POST /container/register
                              │
        ┌─────────────────────┴─────────────────────┐
     2xx / 200                 其它结果（包括沙盒不存在或正在销毁）
        │                                         │
   hook 退 0                               hook 退非 0
   登记完成                         runc create 失败，容器不启动
        │
        ▼
    ENTRYPOINT 执行
```

runtime 和 hook **不碰 BPF、不碰数据库**，只负责把可信的运行时身份转交给 Worker。

## neu-box-runtime（runc wrapper）

一个可执行文件，注册进 `daemon.json` 的 `runtimes`，占 runc 的位置，收到和 runc
完全相同的 argv。

```json
{
  "default-runtime": "neu-box-runtime",
  "runtimes": { "neu-box-runtime": { "path": "/usr/local/bin/neu-box-runtime" } }
}
```

键和值都叫 `neu-box-runtime`，与二进制同名；发布 RPM 名为 `neuboxd`，
同包安装 Worker、client 和 runtime。

**`neu-box-runtime` 是默认 runtime**，用户不需要写 `--runtime`。没带 annotation
的容器照常启动 —— wrapper 什么都不做，直接转发给真 runc；它会不会拿到设备由 BPF
决定（没登记就拒绝）。

代价是 wrapper 从此在**这台机器上所有容器**的启动路径上。所以它必须：

- 对任何解析不出来的输入都**原样转发**，绝不因为自己的问题挡住无关容器；
- 只往 stderr 写日志，不污染 stdout。

### 装机顺序是硬的

`default-runtime` 指向一个不存在或跑不起来的二进制，dockerd 会**起不了任何容器**。
所以：

- 用户先安装 RPM，用 `neuboxctl setup` 确认 runtime、hook 和真正的 runc 可执行，并合并 `daemon.json`；
- 卸载前先手动从 `daemon.json` 移除 Neu Box runtime 配置，**再**卸载 RPM；
- RPM 的 `%preun` 守住同一个不变式：`daemon.json` 还指着 `neu-box-runtime`
  时拒绝 `rpm -e`。

`setup` 会备份旧的 `daemon.json`。`runtimes` / `default-runtime` **不支持热
加载**，配置发生变化后必须重启 dockerd。重启可能中断运行中的容器，
应在维护窗口做。

改完拿 `docker info --format '{{.DefaultRuntime}}'` 验：装上之后应当输出
`neu-box-runtime`，卸干净之后不应再是它。

### 行为

- 只对 `create` 和 `run` 注入，其余子命令（`start`/`state`/`kill`/`delete`/…）
  逐字转发。containerd 只用 `create`；带上 `run` 是为了本机直接用
  `runc run` 测时行为和走 Docker 一致。
- 解析 `--bundle DIR` / `--bundle=DIR` / `-b DIR`（runc 的短别名）三种写法，
  读 `<bundle>/config.json`。认 `-b` 是为了让"本机用 `runc run` 验证"这条路的
  现成写法（`runc run -b <bundle>`）走得通；`-b` 在 runc 里也只出现在
  create/run 上，不会认错。
- 若 `annotations.sandbox_cgroup` 存在且非空 → 往 `hooks[NEU_BOX_HOOK_PHASE]`
  （默认 `createRuntime`）**追加**一条本 hook 的记录（已存在同 path 的就不重复加）。
- 若 annotation 不存在 → **不动 config.json**，也不加 hook。容器照常起来，
  然后在 BPF 那里被拒（这是设计好的 fail-closed 行为，不是 bug）。
- 若 annotation 存在、但**注入本身失败**（config.json 读不了/写不回、phase 配错）
  → **拒绝启动**。判据是"这个容器确认属于某个沙盒"：既然它是受管容器，
  放它进去就等于让它在没有授权的情况下初始化 NPU 驱动。看不懂的输入（不是
  create/run、没有 bundle、JSON 坏了）则是另一种情况 —— 一律**原样转发**，
  不能因为自己的问题挡住无关容器。
- 写回 config.json 后 `exec` 真 runc，argv 原样。
- **能力位守卫**（与 annotation 无关，所有 `create`/`run` 容器都过）：容器请求
  了全套 capabilities（`--privileged` / `--cap-add=ALL`）时，Ascend 驱动
  （`cann/driver`，`src/sdk_driver/pbl/uda/uda_access.c` 的 `uda_is_admin_task`）
  会把它判成 **admin**：掩码 `ka_system_get_privileged_kernel_cap()` 在 6.3+
  内核上是 bits 0..37（`CAP_CHOWN`…`CAP_AUDIT_READ`），比较是**超集**判定，
  所以只有全套能力位才命中。命中的后果是驱动给这个 mount namespace 建一张
  **全量** UDA 设备表（按 mnt ns 缓存），而 worker 那套 eBPF 只拦
  `open("/dev/davinciN")`，驱动这条路径不看它 —— 沙盒隔离静默失效（真机实测：
  申请 2 张卡的容器里 `torch.npu.device_count() == 8`）。
  默认 `NEU_BOX_CAP_GUARD=drop`：从 bounding/permitted/effective/ambient 四个
  集合里剪掉 `CAP_AUDIT_READ`（掩码覆盖得到的最后一位，容器几乎不可能用到），
  容器即掉出 admin 超集，其余能力位不动。于是受管容器只看到沙盒那几张卡、
  未登记容器一张都看不到（fail-closed）。`deny` 改成拒绝创建，`off` 关闭
  （排障用）。
- 能力位守卫也管 **`exec`**：`docker exec` 那份 capability 不在 bundle 里，
  而是 containerd 交给 `runc exec --process <file>` 的一份 Process JSON，并且是
  docker 按容器**自己的 HostConfig 现算**的 —— 跟 create 时被我们剪过的那份
  spec 无关，实测 exec 进程的 `CapEff` 仍是全量。所以 `exec` 子命令上单独剪
  同一位。exec 的 Process 里没有 user namespace 信息（那是容器级属性），这里
  不做 userns 判断 —— 多剪一位本来无害。

## neu-box-hook

由 runc 在 **`createRuntime`** 阶段调用（`NEU_BOX_HOOK_PHASE` 可切回 `prestart`）。

- **从 stdin 读 OCI state**（`{"ociVersion","id","pid","bundle","annotations"}`），
  不从 argv 读。
- `sandbox_cgroup` 优先从 state 的 `annotations` 取，取不到再读
  `<bundle>/config.json`。
- 向 `<NEU_BOX_WORKER_URL>/container/register` 发请求；body 只有契约里那三个
  必填字段（`container_id` / `host_pid` / `sandbox_cgroup`），字段含义见
  [`../../../docs/worker-api.md`](../../../docs/worker-api.md)。
- **成功 → 退 0。** Worker 确认容器已登记，容器才执行 ENTRYPOINT。
- **任何失败（沙盒不存在、正在销毁、连不上 Worker、超时、5xx、400、身份冲突 409…）→ 退非 0。**
  已声明受管的容器不能在没有预期授权时启动：放行之后容器里会发生什么、为什么补不回来，见
  [`../../../docs/container-registration.md`](../../../docs/container-registration.md)；而"拿不到答案"那一类更危险 ——
  `neuboxctl pause` 的窗口里 BPF 是拆掉的，那时的容器会直接把全部卡拿走。
  对旧容器执行原生 `docker start`，若它的 annotation 指向已释放的沙盒，
  runtime 会拒绝启动。需要先取得新沙盒，再通过 `neubox docker start` 借出本次授权。
- 不从 argv 读、也不额外发 `container_cgroup` / `mount_namespace`：那两个值是
  Worker 自己从 `/proc/<pid>` 读真值，hook 再报一遍不提供额外信息，报错了反而
  把登记搞失败（409）。

超时预算：hook 自身 10s（由 wrapper 写进 OCI hook 记录的 `timeout` 字段），其中
Worker HTTP 请求 8s。**HTTP 超时必须严格小于 hook 的 timeout**（8s < 10s），否则
runc 杀掉 hook 时连错误信息都拿不到。

## 运行配置

`/etc/neu-box/runtime.env`（角色约定：`/etc/neu-box/<role>.env`，dotenv 格式，
键一律 `NEU_BOX_*` 前缀）。这份文件由 `neuboxctl setup`
生成和迁移，**不由 RPM 安装**。仓库里的
`../../../deploy/config/runtime.env.example` 只是键的文档：

| 键 | 默认值 | 说明 |
|---|---|---|
| `NEU_BOX_WORKER_URL` | `http://127.0.0.1:59075` | Worker 地址，hook 往这里登记 |
| `NEU_BOX_HOOK` | `/usr/local/bin/neu-box-hook` | 注入进 config.json 的 hook 路径 |
| `NEU_BOX_HOOK_PHASE` | `createRuntime` | 注入到哪个 OCI hook 阶段（`prestart` 可切回，见下面「phase 验证记录」） |
| `NEU_BOX_REAL_RUNC` | `/usr/local/bin/runc` | wrapper 后面真正接的 runtime |
| `NEU_BOX_CAP_GUARD` | `drop` | 对请求全套 capability 的容器执行 `drop`、`deny` 或 `off`，详见上文「能力位守卫」 |

文件里另有一个 `NEU_BOX_CONFIG_VERSION`，标记配置文件格式，与软件版本分开。
`neuboxctl setup` 接受当前的版本 1，也接受未标版本的文件（视为版本 0），补齐
缺失键后写成版本 1；未知版本会报错。配置维护在部署时执行，不在容器创建路径上跑。
runtime 本身没有常驻进程，读取配置时仍会对缺失键使用内置默认值。

环境变量优先于文件（和 worker 的 `load_dotenv(override=False)` 一致），
`NEU_BOX_CONFIG=<path>` 可以把程序指到另一个配置文件。

配置文件是主要通道，不是可有可无的备选：hook 是被 runc 拉起来的，继承的是
**dockerd 的环境**，`docker run -e` 传不进去 —— 这就是配置必须落文件、不能靠
环境变量的原因。

配置读不动（文件缺失、语法错）不致命：用默认值接着干活，问题打一行 stderr。
在容器创建路径上因为配置文件打不开就拒绝启动，代价比配错了还大。

**为什么不塞进 `worker.env`**：两种配置的读取时机不同。`worker.env` 由
RPM 提供初始文件，`runtime.env` 由 `setup` 生成，供 Docker 启动的 runtime 和 hook 读取。

`NEU_BOX_WORKER_URL` 的端口由 `worker.env` 的 `NEU_BOX_PORT` 决定。每次运行
`neuboxctl setup` 都会同步这个值；修改 Worker 端口后重新运行 `setup`。
`setup` 默认自动查找 runc；只有找不到或需要显式覆盖时才使用
`--real-runc /实际路径`。

`NEU_BOX_REAL_RUNC` 必须可配：以后和 Ascend Docker Runtime 串接时，wrapper
后面接的就不是 runc 了。

## phase 验证记录（别把"验过"的范围说过头）

- **`prestart`**：整条 Docker 链路验过 —— Docker 28.5.2 → containerd 1.7.28 →
  runc 1.3.3，探针实测 hook 里能读到容器 host PID、容器 mnt ns、
  `/system.slice/docker-<id>.scope` 及其 inode；hook 退非 0 时容器创建失败、
  payload 不执行。
- **`createRuntime`（当前默认）**：**直连 runc 验过**（`runc run -b`，不经过
  dockerd）—— hook 被调用、能读到容器 mnt ns（`mnt:[4026549739]` ≠ hook 自己的
  `mnt:[4026531841]`）和容器 cgroup scope 及其 inode、退非 0 时 payload 不执行。
  phase 是 runc 自己的行为，Docker/containerd 只负责挑 runtime 二进制，
  所以这一层验过就够。
- **两个 phase 都没验过的**：整条 Docker 链路上跑 `createRuntime`。真机装机时
  第一次跑就是它，出问题就把 `NEU_BOX_HOOK_PHASE` 切回 `prestart`（这就是
  这个开关留着的原因，也是 `prestart` 不能当遗留垃圾删掉的原因）。

选 `createRuntime` 当默认是因为 `prestart` 在 OCI 规范里已废弃，迟早会被
runc 摘掉。

> **顺带一条实测教训**：两次跑出来的容器 `mnt ns inum` 是同一个数
> （`mnt:[4026549739]`）—— 第一个容器退出后内核把 inum 回收给了第二个。
> **光有 inum 分不清"同一个容器"和"回收后重用的号"**，所以 Worker 侧的
> `ContainerIdentity` 必须带 `init_start_time`（`/proc/<pid>/stat` 第 22 字段，
> 见 [`../../../docs/container-registration.md`](../../../docs/container-registration.md)）。

## 边界与本轮不做

- **wrapper 在这台机器所有容器的启动路径上**（它是 default-runtime）。它对看不懂
  的输入一律原样转发，只有"已经确认是沙盒容器（annotation 就在 config.json 里）
  但注入没做成"才拒绝启动。日志只写 stderr，不碰 stdout。
- **注入的 phase 默认 `createRuntime`**，`prestart` 可以切回去。别把 `prestart`
  删掉 —— 它是退路，理由见上面「phase 验证记录」。
- **hook 只发契约里的三个必填字段**，理由见「neu-box-hook」一节。
- **与 Ascend Docker Runtime 并存本轮不做。** 它和我们是同一个机制（runc wrapper
  + 自己的 prestart hook）。它到底是 append 还是 assign `hooks.prestart` 决定能
  不能简单串接，未验证。`NEU_BOX_REAL_RUNC` 留着就是为了将来能接。
- 鉴权、Kubernetes 不在本轮范围，见
  [`../../../docs/container-registration.md`](../../../docs/container-registration.md)「这轮明确不做」；
  升级流程见 [`../../../docs/deployment.md`](../../../docs/deployment.md)。
