# Neu Box 节点组件部署与升级手册

## 部署

```bash
# 首次安装：一个 RPM 安装 Worker、client 与 runtime 的程序文件
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudoedit /etc/neu-box/worker.env          # RPM 已装默认配置，按节点需要修改
sudo /usr/libexec/neu-box/bin/neuboxctl setup                     # 配置 runtime 与 Docker、迁移数据库、启动 Worker
docker info --format '{{.DefaultRuntime}}'  # 应为 neu-box-runtime
curl -fsS http://127.0.0.1:59075/healthz
sudo env NEU_BOX_CONTAINER_IMAGE=alpine:3.20 \
  NEU_BOX_DRIVER_PROBE_IMAGE=your-ascend-image:tag /usr/libexec/neu-box/bin/neuboxctl test

# 升级：先 pause，再装同一个包、迁移配置、setup、验收
sudo /usr/libexec/neu-box/bin/neuboxctl pause
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudo /usr/libexec/neu-box/bin/neuboxctl setup
sudo env NEU_BOX_CONTAINER_IMAGE=alpine:3.20 \
  NEU_BOX_DRIVER_PROBE_IMAGE=your-ascend-image:tag /usr/libexec/neu-box/bin/neuboxctl test
```

从 `0.5.0-15` 升级到首次采用新入口的版本时，安装前仍需使用已安装版本的
`sudo /usr/libexec/neu-box/neuboxctl/neuboxctl pause`；安装完成后改用上面的新路径执行 `setup`。

`pause` 是升级前必须执行的停服路径：排空任务和沙盒、备份数据库和配置、清理旧 BPF pins 后停止 Worker；`setup` 是唯一启动路径：迁移旧配置和 SQLite schema，加载新版 BPF，`/healthz` 通过后恢复调度。启动和停止统一走 `neuboxctl setup` / `pause`，只读观察使用 `systemctl status`、`journalctl`；等待超时可用 `--timeout <秒>` 调整。

`pause` 备份数据库时，也会把现有的 `worker.env` 和 `runtime.env` 复制到同一备份目录；
runtime 配置备份以 `.runtime.env` 结尾。

`setup` 从 `worker.env` 的 `NEU_BOX_PORT` 配置 runtime 的 Worker 地址，并生成或迁移
`/etc/neu-box/runtime.env`。它默认自动查找本机 runc；若找不到，
或需要显式选择另一个 runtime，使用
`sudo /usr/libexec/neu-box/bin/neuboxctl setup --real-runc /实际路径`。没有 Docker 的宿主机任务节点可以直接
运行 `setup`。`setup` 每次都会同步 Worker 地址；runtime 的其他手工配置保持不变。
`setup` 会打印实际使用的 Worker URL、hook 和 runc 路径；需要检查文件内容时，
查看 `/etc/neu-box/runtime.env`。RPM 安装脚本不编辑 Docker 配置、不启动 Worker、
不重启 dockerd；这些动作由显式的 `neuboxctl setup` 完成。

`setup` 自动将下面两个键合并进 `/etc/docker/daemon.json`，保留其他键和值。
写入前调用 `dockerd --validate`，并备份旧文件。没有 Docker 的节点跳过这一步。
如果原本配置了其他默认运行时，`setup` 会停止并提示人工核对调用链：

```json
{
  "default-runtime": "neu-box-runtime",
  "runtimes": {
    "neu-box-runtime": {"path": "/usr/libexec/neu-box/neu-box-runtime"}
  }
}
```

若 Docker 已加载该配置，`setup` 不重启。否则交互终端会询问是否重启 Docker：选 Y
则等待 Docker 启动、验证默认运行时，再恢复 Worker 调度；选 N 则退出并保持 Worker
暂停。此时在维护窗口手动运行 `sudo systemctl restart docker`，确认
`docker info --format '{{.DefaultRuntime}}'` 输出 `neu-box-runtime`，最后运行
`sudo /usr/libexec/neu-box/bin/neuboxctl resume`。非交互环境默认选择 N；可用 `--restart-docker` 自动重启。
Docker 重启可能停止当前运行的容器，请先检查 `docker ps`。

## API 与实机验收

`neuboxctl test` 会访问真实 API、运行任务、占用设备、
启动容器，最后的维护用例还会停/起 Worker。必须在维护窗口以 root 执行。
套件不自动拉取镜像；缺少必需镜像或设备时会失败，不会跳过。

普通容器用例使用 `NEU_BOX_CONTAINER_IMAGE` 指定本机已有、带 `/bin/sh` 的镜像，
例如 `alpine:3.20`。Ascend UDA 隔离用例另需
`NEU_BOX_DRIVER_PROBE_IMAGE`：镜像必须已在本机，并包含 Python、`torch_npu` 和
与宿主驱动兼容的 CANN 用户态。Alpine 不能充当驱动探针镜像。

```bash
docker image inspect alpine:3.20 your-ascend-image:tag
sudo env NEU_BOX_CONTAINER_IMAGE=alpine:3.20 \
  NEU_BOX_DRIVER_PROBE_IMAGE=your-ascend-image:tag /usr/libexec/neu-box/bin/neuboxctl test
```

将镜像名换成该节点实际安装的名称。套件还覆盖 CLI 的
`neubox submit --script` 文件快照、stdin 脚本退出码，以及脚本中执行
`neubox docker run --rm` 后的容器和设备清理。

## 部署依赖

目标节点机器要求：

| 项目 | 要求 | 必选/可选 |
|---|---|---|
| 操作系统 | Linux | 必选 |
| 架构 | `x86_64` / `aarch64` | 必选 |
| init | systemd | 必选 |
| cgroup | cgroup v2 | 必选 |
| 内核 | 5.11+，启用 cgroup device BPF | 必选 |
| BTF | `/sys/kernel/btf/vmlinux` 存在（`CONFIG_DEBUG_INFO_BTF=y`） | 必选 |
| 权限 | root 或可用的 sudo | 必选 |
| 必需工具 | `/bin/bash` | 必选 |
| 设备工具 | 对应厂商的驱动和状态工具，例如 `npu-smi` 或 `nvidia-smi` | 必选 |
| 容器场景 | Docker、`neu-box-runtime` 与 `neu-box-hook` | 可选 |

检查系统依赖：

```bash
uname -m
systemctl --version
test -f /sys/fs/cgroup/cgroup.controllers   # cgroup v2
test -f /sys/kernel/btf/vmlinux             # BPF CO-RE 必需
ldconfig -p | grep libbpf                   # native sandbox 动态链接依赖，由 dnf 安装

# 容器场景（可选）
command -v docker
test -x /usr/libexec/neu-box/neu-box-hook
docker info --format '{{.DefaultRuntime}}'   # 期望 neu-box-runtime
```

`/sys/kernel/btf/vmlinux` 缺失时 BPF CO-RE 无法加载，Worker `setup` 健康检查会失败。

## 构建打包

构建机需要 Linux，架构通常与目标机一致。依赖如下：

| 依赖 | 要求 | 用途 |
|---|---|---|
| `uv` | - | 管理 Python 依赖和构建环境 |
| Go | 1.21.4+ | 构建 client 与 OCI runtime/hook |
| PyInstaller | - | 打包 Worker 与验收套件 |
| GNU Make | - | 构建 native sandbox |
| C++17 编译器 | - | 编译 native sandbox |
| Clang | 支持 BPF target | 编译 `device_block.o` |
| `pkg-config` | - | 发现并链接 libbpf |
| libbpf | 1.0+ 开发包 | native sandbox 链接 |
| `rpmbuild` | - | 生成 RPM |
| binutils | 提供 `readelf` | 解析 ELF 依赖 |

检查构建依赖：

```bash
uname -m
uv --version
uv run --frozen --group build pyinstaller --version
command -v make
command -v c++
command -v clang
command -v pkg-config
pkg-config --modversion libbpf
command -v rpmbuild
command -v readelf
go version
```

构建前如果 shell 里已经激活了别的虚拟环境，先清掉：

```bash
unset VIRTUAL_ENV
uv sync --frozen --all-groups
```

构建命令：

```bash
uv run --frozen --group build deploy/build_release.py
```

开发后、构建发布前需确保测试通过，详见[测试文档](testing.md)。

构建完成后检查产物：

```bash
ls -l dist/rpm/neuboxd-*.rpm

rpm -qpi dist/rpm/neuboxd-*.rpm
rpm -qpl dist/rpm/neuboxd-*.rpm | grep -E 'neuboxctl|neubox|neu-box-(runtime|hook|deployment-tests)'
rpm -qpR dist/rpm/neuboxd-*.rpm
```

构建产物本地自检：

```bash
build/release/pyinstaller-dist/neu-box-deployment-tests/neu-box-deployment-tests --self-check
```

## 程序的运行时组织

升级时按类别处理：程序文件由 RPM 管理，配置和数据库由 `setup` 迁移，BPF 和运行时状态由 `pause` 清理、`setup` 重建。路径如下：

```shell
# 程序文件：RPM 安装和管理，不迁移
/usr/libexec/neu-box/neuboxd/                   neuboxd daemon bundle
/usr/libexec/neu-box/ctl/                       neuboxctl 管理 CLI bundle
/usr/libexec/neu-box/bin/neuboxctl             私有管理命令入口
/usr/libexec/neu-box/device_block.o             预编译 BPF object
/usr/libexec/neu-box/neu-box-sandbox            native sandbox
/usr/local/bin/neubox                            Go client，唯一公开命令
/usr/libexec/neu-box/neu-box-runtime             OCI runtime wrapper
/usr/libexec/neu-box/neu-box-hook                OCI hook
/usr/lib/systemd/system/neuboxd.service         systemd unit

# 配置：setup 迁移旧键；runtime 的 Worker URL 跟随 NEU_BOX_PORT
/etc/neu-box/worker.env                         Worker 配置
/etc/neu-box/runtime.env                        OCI runtime 配置，由 setup 生成或迁移

# 数据库：setup 迁移 schema，pause 备份，pending 任务保留
/var/lib/neu-box/worker/neu_box.db              SQLite 数据库
/var/lib/neu-box/worker/neu_box.db.paused       暂停标记

# 数据与日志：不迁移、不删除；pause 备份数据库和配置
/var/lib/neu-box/worker/task-logs/              任务日志
/var/log/neu-box/                               Worker 日志
/var/backups/neu-box/                           数据库和配置备份

# 运行时状态：pause 清理，setup 重建
/run/neu-box/sandbox-state/                     沙盒运行时状态
```

`neuboxd` 只暴露 `serve` 守护进程入口；`setup`、`pause`、`resume`、`db`、`sandbox`、`test` 都由独立的 `neuboxctl` 提供。`neuboxctl` 的实现源码在 `src/neu_box/ctl.py`，由 PyInstaller 打成独立 onedir；配置变量见 [配置文档](configuration.md)。
