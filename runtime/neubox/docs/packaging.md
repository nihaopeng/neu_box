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

```bash
bash deploy/rpm/build_rpm.sh                 # 编译 + 打包，产物在 dist/rpm/
bash deploy/rpm/build_rpm.sh --no-build      # 用 dist/ 里已有的二进制
bash deploy/rpm/build_rpm.sh --source-only   # 只出 tar.gz 和渲染后的 spec
```

编译在 rpmbuild 之前做完，spec 只落文件 —— spec 里没有工具链。

runtime RPM 的主要条目（[`../deploy/rpm/neu-box-runtime.spec`](../deploy/rpm/neu-box-runtime.spec)）：

| 路径 | 权限 | 说明 |
|---|---|---|
| `/etc/neu-box` | 0750 root:root | 配置目录（目录归包，文件不归） |
| `/usr/local/bin/neu-box-runtime` | 0755 | wrapper |
| `/usr/local/bin/neu-box-hook` | 0755 | OCI hook |
| `/usr/local/bin/neu-box-config` | 0755 | 配置生成/迁移 |
| `/usr/libexec/neu-box/runtime/scripts/` | 0755 | 已安装节点的配置与卸载脚本 |

包里**没有** `runtime.env`：那份文件由 `neu-box-config` 在部署时生成。仓库里的
`deploy/config/runtime.env.example` 只是键的文档，不进包。

### 为什么路径写死在 /usr/local/bin

不是惯例，是契约：daemon.json 里的 path、两个二进制自己的内置默认值
（`NEU_BOX_HOOK` / `NEU_BOX_REAL_RUNC`）、以及 `scripts/install.sh` 三方都钉在这个
前缀上。包换一个路径，装上就会有两份运行时 —— daemon.json 指一份、下次升级更新
另一份。

### 为什么没有 Requires

对三个二进制没有：它们都是静态的（`CGO_ENABLED=0`），没有可声明的依赖。rpmbuild
会自动补上脚本对 `/bin/sh` 的依赖，那个每台机器都有。

runc 和 Docker **故意不写**：装 Docker 的机器上 runc 往往是它自带的普通文件
（本机是 `/usr/local/bin/runc`，`rpm -qf` 说不属于任何软件包），`Requires: runc` /
`Requires: docker` 只会让包装不上。该挡的那件事在部署那一步挡：`install.sh` 在改
`daemon.json` **之前**检查 `NEU_BOX_REAL_RUNC` 和 `NEU_BOX_HOOK` 真的存在且可执行。

### 包脚本做了什么（很少）

- `%post`：首次安装打一段"文件装好了但还没生效"的提示，升级则提示配置迁移在部署
  步骤里。别的一律不做 —— 不碰 daemon.json、不碰配置、不重启 dockerd。
- `%preun`：最终卸载时，如果 daemon.json 还指着 neu-box-runtime 就拒绝 `rpm -e`。
- `%pre`：空。在 dockerd 跑着的时候拒绝安装，会让这个包在任何 Docker 机器上都装不了。
