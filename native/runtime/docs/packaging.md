# 构建、测试与打包

面向改这个仓库的人。装机步骤在 [`../README.md`](../README.md)，运行时行为契约在
[`runtime-hook.md`](runtime-hook.md)。

正式发布只使用仓库根目录的 `deploy/build_release.py`，产物是包含 Worker、client、
runtime 的单个 `neuboxd` RPM。

## 构建

两个 runtime 二进制都是静态的（`CGO_ENABLED=0`），标准库够用、没有第三方依赖：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o dist/neu-box-runtime ./cmd/neu-runtime
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o dist/neu-box-hook    ./cmd/neu-hook
```

### 版本号

正式发布的 RPM 版本以仓库根的 `src/neu_box/__init__.py` 为准。顶层
`deploy/build_release.py` 在同一次发布中构建 Worker、client 和 runtime。
`neu-box-runtime` / `neu-box-hook` 不提供独立的 `--version`：wrapper 的 argv 要
原样转发给 runc，hook 由 runc 调用，不面向用户命令行。

## 测试

```bash
go test ./...     # 不需要 root、不需要 Docker、不需要 runc
go vet ./...
gofmt -l .
```

怎么做到不碰真东西：

- **wrapper**：临时 bundle + 临时 config.json；端到端那条用假 runc（一个 shell
  脚本，把收到的 argv 写进文件），验证 `syscall.Exec` 之后真 runc 拿到的 argv
  逐字不变；`TestMain` 里有个子进程模式，让测试二进制把自己当成 `neu-box-runtime`
  重新拉起来（`run()` 最后会 exec 换掉自己，没法在测试进程里直接调）。
- **hook**：`httptest` 冒充 Worker，断言请求体只有契约里那三个必填字段、2xx 退 0、
  4xx 和连不上退非零。
- **config**：临时 env 文件 + `t.Setenv`，不读这台机器上的真配置。

## 打包

正式发布从仓库根运行：

```bash
uv run --frozen --group build deploy/build_release.py
```

它构建一个 `neuboxd` RPM，其中包含 Worker、client 和本目录的两个 runtime
二进制。

正式发布的 RPM 文件清单见 [`../../../deploy/rpm/neuboxd.spec`](../../../deploy/rpm/neuboxd.spec)；
本目录中 runtime 的主要安装路径为：

| 路径 | 权限 | 说明 |
|---|---|---|
| `/etc/neu-box` | 0750 root:root | `worker.env` 归包；`runtime.env` 由 `neuboxctl setup` 生成 |
| `/usr/libexec/neu-box/neu-box-runtime` | 0755 | wrapper |
| `/usr/libexec/neu-box/neu-box-hook` | 0755 | OCI hook |

包里**没有** `runtime.env`：执行 `neuboxctl setup` 时生成或迁移它。
键的说明模板安装在 `/usr/share/neu-box/runtime.env.example`。

### 私有路径与 Docker 配置

Docker `daemon.json` 的 path 和 `NEU_BOX_HOOK` 都由 `neuboxctl setup` 指向
RPM 内的私有二进制。升级旧包时，`setup` 同步更新曾由包安装的旧 hook 路径与
Docker runtime 路径；手工指定的 hook 路径保留。`NEU_BOX_REAL_RUNC` 指向宿主机
实际安装的 runc，不是 Neu Box 私有程序。

### 为什么不声明 Docker 和 runc 依赖

两个 runtime 二进制都是静态的（`CGO_ENABLED=0`），自身没有动态库依赖。
合并 RPM 仍声明 Worker 需要的 systemd 等依赖。

runc 和 Docker **故意不写**：装 Docker 的机器上 runc 往往是它自带的普通文件
（本机是 `/usr/local/bin/runc`，`rpm -qf` 说不属于任何软件包），`Requires: runc` /
`Requires: docker` 只会让包装不上。`setup` 在修改 `daemon.json` 前会确认
`NEU_BOX_REAL_RUNC` 和 `NEU_BOX_HOOK` 指向的程序确实存在且可执行。

### 包脚本做了什么（很少）

- `%post`：刷新 systemd unit；`%posttrans` 在事务末尾提示运行私有路径下的 `neuboxctl setup`。脚本不碰 daemon.json、
  不生成 `runtime.env`、不重启 dockerd。
- `%preun`：最终卸载时，如果 daemon.json 还指着 neu-box-runtime 就拒绝 `rpm -e`。
- `%pre`：Worker 服务运行中时拒绝升级，要求先用 `neuboxctl pause`。
