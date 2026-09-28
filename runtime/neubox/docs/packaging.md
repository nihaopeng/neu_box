# 构建、测试与打包

面向改这个仓库的人。装机步骤在 [`../README.md`](../README.md)，运行时行为契约在
[`runtime-hook.md`](runtime-hook.md)。

## 构建

三个二进制都是静态的（`CGO_ENABLED=0`），标准库够用、没有第三方依赖：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o dist/neu-box-runtime ./cmd/neu-runtime
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o dist/neu-box-hook    ./cmd/neu-hook
CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/neu-box-config ./cmd/neu-config
```

### 版本号

正式发布只以仓库根的 `src/neu_box/__init__.py` 为准：顶层
`deploy/build_release.py` 把同一版本传给 Worker、client、runtime 的构建入口。
本目录的 `VERSION` 仅供单独调试 runtime 构建时使用。

- `neu-box-runtime` / `neu-box-hook` **不带**版本号。wrapper 的 argv 是转发给 runc
  的，加 `--version` 会打架；hook 由 runc 拉起、本来就没有命令行。
- `neu-box-config` 带，而且是**链接期注入**的（`-X main.version`）。

它故意把两件事分开印：`neu-box-config version` 同时报软件版本和它支持的**配置
schema 版本**。排障时"这台机器要不要迁移"看的是后者，不是前者。

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

它构建一个 `neuboxd` RPM，其中包含 Worker、client 和本目录三个 runtime 二进制。
下面的旧入口只用于单独调试 runtime 打包，不属于正式发布流程：

```bash
bash deploy/rpm/build_rpm.sh                 # 编译 + 打包，产物在 dist/rpm/
bash deploy/rpm/build_rpm.sh --no-build      # 用 dist/ 里已有的二进制
bash deploy/rpm/build_rpm.sh --source-only   # 只出 tar.gz 和渲染后的 spec
```

编译在 rpmbuild 之前做完，spec 只落文件 —— spec 里没有工具链。

正式发布的 RPM 文件清单见 [`../../../deploy/rpm/neuboxd.spec`](../../../deploy/rpm/neuboxd.spec)；
本目录中 runtime 的主要安装路径为：

| 路径 | 权限 | 说明 |
|---|---|---|
| `/etc/neu-box` | 0750 root:root | `worker.env` 归包；`runtime.env` 由 `neuboxctl setup` 生成 |
| `/usr/local/bin/neu-box-runtime` | 0755 | wrapper |
| `/usr/local/bin/neu-box-hook` | 0755 | OCI hook |
| `/usr/local/bin/neu-box-config` | 0755 | 配置生成/迁移 |

包里**没有** `runtime.env`：执行 `neuboxctl setup` 时生成或迁移它。
键的说明模板安装在 `/usr/share/neu-box/runtime.env.example`。

### 为什么路径写死在 /usr/local/bin

这个路径是 Docker `daemon.json` 里的 path 和二进制内置默认值
（`NEU_BOX_HOOK` / `NEU_BOX_REAL_RUNC`）共同使用的契约。改路径要同时修改
手动配置步骤和这些默认值。

### 为什么不声明 Docker 和 runc 依赖

三个 runtime 二进制都是静态的（`CGO_ENABLED=0`），自身没有动态库依赖。
合并 RPM 仍声明 Worker 需要的 systemd 等依赖。

runc 和 Docker **故意不写**：装 Docker 的机器上 runc 往往是它自带的普通文件
（本机是 `/usr/local/bin/runc`，`rpm -qf` 说不属于任何软件包），`Requires: runc` /
`Requires: docker` 只会让包装不上。用户在手动修改 `daemon.json` 前应先确认
`NEU_BOX_REAL_RUNC` 和 `NEU_BOX_HOOK` 指向的程序确实存在且可执行。

### 包脚本做了什么（很少）

- `%post`：刷新 systemd unit 并打印人工配置步骤；不碰 daemon.json、不生成
  `runtime.env`、不重启 dockerd。
- `%preun`：最终卸载时，如果 daemon.json 还指着 neu-box-runtime 就拒绝 `rpm -e`。
- `%pre`：Worker 服务运行中时拒绝升级，要求先用 `neuboxctl pause`。
