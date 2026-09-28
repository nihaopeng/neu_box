# Neu Box OCI runtime

本目录是 `neu_box` 仓库的一部分。发布入口为仓库根的
`deploy/build_release.py`；它用同一版本构建 Worker、client 和本目录的 runtime。
源码由原 `neu_box_runtime` 仓库的 `dea6365` 提交纳入，后续在本仓库维护。

容器接入 neu-box 沙盒的 **runtime 侧**。三个二进制：

| 二进制 | 干什么 |
|---|---|
| `neu-box-runtime` | runc wrapper，占 Docker `default-runtime` 的位置：给带 annotation 的容器注入 hook，其余 argv 原样转发给真 runc |
| `neu-box-hook` | OCI hook：在容器 ENTRYPOINT 之前向 Worker 登记；登记失败或 Worker 不可达时拒绝受管容器启动 |
| `neu-box-config` | 由 `neuboxctl setup` 调用，生成/迁移 `/etc/neu-box/runtime.env`；也可检查生效值 |

行为契约、hook phase 验证记录、边界 → [`docs/runtime-hook.md`](docs/runtime-hook.md)
构建与打包细节 → [`docs/packaging.md`](docs/packaging.md)
登记接口契约（字段、状态码）→ [`../../docs/worker-api.md`](../../docs/worker-api.md)

## 安装

```bash
# 单个 RPM 安装 Worker、client、runtime 的程序文件
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudoedit /etc/neu-box/worker.env  # 如需修改 Worker 监听端口或设备配置
sudo neuboxctl setup
sudo neu-box-config show
sudoedit /etc/docker/daemon.json  # 手动合并 default-runtime 和 runtimes.neu-box-runtime

# 必须重启 dockerd：会杀掉这台机器上当时所有运行中的容器，进维护窗口做
docker ps
sudo systemctl restart docker
docker info --format '{{.DefaultRuntime}}'   # 应当输出 neu-box-runtime
```

单个 RPM 用仓库根的 `deploy/build_release.py` 构建。`daemon.json` 要加入的 JSON
见 [部署手册](../../docs/deployment.md)；RPM 不修改 Docker 配置，也不重启 dockerd。
装完用 `neu-box-config show` 检查 runtime 配置。

## 升级

升级同样只装一个包：

```bash
sudo neuboxctl pause
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudo neuboxctl setup
```

只有 Docker 配置发生变化时才需另行安排 dockerd 重启。

## 卸载

```bash
sudo neuboxctl pause
sudoedit /etc/docker/daemon.json  # 手动移除 default-runtime 和 runtimes.neu-box-runtime
sudo systemctl restart docker
sudo dnf remove neuboxd
```

移除 Docker 配置时保留文件里的其他键。RPM 不拥有 `runtime.env`，卸载不会清除它。

## 配置

`/etc/neu-box/runtime.env` 由 `neuboxctl setup` 调用 `neu-box-config` 生成和迁移。
除 `NEU_BOX_WORKER_URL` 每次按 Worker 的 `NEU_BOX_PORT` 同步外，手工改过的值会
保留；没有 `NEU_BOX_CONFIG_VERSION` 的文件按配置 schema 版本 0 迁移。

| 键 | 默认值 | 说明 |
|---|---|---|
| `NEU_BOX_WORKER_URL` | `http://127.0.0.1:59075` | Worker 地址，hook 往这里登记 |
| `NEU_BOX_HOOK` | `/usr/local/bin/neu-box-hook` | 注入进 config.json 的 hook 路径 |
| `NEU_BOX_HOOK_PHASE` | `createRuntime` | 注入到哪个 OCI 阶段（`prestart` 是退路） |
| `NEU_BOX_REAL_RUNC` | `/usr/local/bin/runc` | wrapper 后面真正接的 runtime |

```bash
neu-box-config show      # 生效值 + 每个值的来源（file / env / default）
neu-box-config version   # 软件版本 + 它支持的配置 schema 版本
```

两个值需要和这台机器对上：

- `NEU_BOX_WORKER_URL` 的端口跟 Worker 的 `NEU_BOX_PORT` 一致，`neuboxctl setup`
  每次同步它。
- `NEU_BOX_REAL_RUNC` 指向本机真正的 runc。`setup` 从 `PATH` 查找；若 Docker
  已安装但找不到 runc，执行 `sudo neuboxctl setup --real-runc /实际路径`。

## 安装检查和调试

安装单个 RPM 后运行 `sudo neuboxctl setup`。修改 `/etc/neu-box/worker.env` 的
`NEU_BOX_PORT` 后，先用 `neuboxctl pause` 停止运行中的 Worker，再运行 `setup`，
让 runtime 的 Worker 地址保持一致。
容器场景中，备份并手动修改 `/etc/docker/daemon.json`，加入 `default-runtime` 和
`runtimes["neu-box-runtime"]` 两个键，内容见[部署手册](../../docs/deployment.md)。
随后在维护窗口手动重启 dockerd。

`neu-box-config init` 保留给需要单独调试 runtime 配置的维护者；正常安装和升级
只需运行 `neuboxctl setup`。

看现状：

| 现象 | 看什么 |
|---|---|
| 这台机器上所有容器都起不来 | `neu-box-config show` 里的 `NEU_BOX_REAL_RUNC` 是不是本机真正的 runc |
| 容器起得来，但里面设备全被拒 | 登记没成功。hook 的失败原因在 runc 的 stderr，也就是 `journalctl -u docker` |
| `docker info` 里的 DefaultRuntime 不对 | 检查 `/etc/docker/daemon.json`，修改后手动重启 dockerd |
| 想退回原生 runc | 手动移除 `daemon.json` 中 Neu Box 的两个键，再手动重启 dockerd |
