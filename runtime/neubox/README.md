# Neu Box OCI runtime

本目录是 `neu_box` 仓库的一部分。发布入口为仓库根的
`deploy/build_release.py`；它用同一版本构建 Worker、client 和本目录的 runtime。
源码由原 `neu_box_runtime` 仓库的 `dea6365` 提交纳入，后续在本仓库维护。

容器接入 neu-box 沙盒的 **runtime 侧**。三个二进制：

| 二进制 | 干什么 |
|---|---|
| `neu-box-runtime` | runc wrapper，占 Docker `default-runtime` 的位置：给带 annotation 的容器注入 hook，其余 argv 原样转发给真 runc |
| `neu-box-hook` | OCI hook：在容器 ENTRYPOINT 之前向 Worker 登记；登记失败或 Worker 不可达时拒绝受管容器启动 |
| `neu-box-config` | 生成/迁移 `/etc/neu-box/runtime.env` |

行为契约、hook phase 验证记录、边界 → [`docs/runtime-hook.md`](docs/runtime-hook.md)
构建与打包细节 → [`docs/packaging.md`](docs/packaging.md)
登记接口契约（字段、状态码）→ [`../../docs/worker-api.md`](../../docs/worker-api.md)

## 安装

```bash
# 单个 RPM 安装 Worker、client、runtime 的程序文件
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudo neu-box-config init --real-runc "$(command -v runc)" --worker-url http://127.0.0.1:59075
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
sudo neu-box-config init --real-runc "$(command -v runc)" --worker-url http://127.0.0.1:59075
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

`/etc/neu-box/runtime.env`，由 `neu-box-config` 生成和迁移。生成之后照常手改 ——
你改过的值会被保留；从零手写、不带 `NEU_BOX_CONFIG_VERSION` 的文件不参与迁移。

| 键 | 默认值 | 说明 |
|---|---|---|
| `NEU_BOX_WORKER_URL` | `http://127.0.0.1:59075` | Worker 地址，hook 往这里登记 |
| `NEU_BOX_HOOK` | `/usr/local/bin/neu-box-hook` | 注入进 config.json 的 hook 路径 |
| `NEU_BOX_HOOK_PHASE` | `createRuntime` | 注入到哪个 OCI 阶段（`prestart` 是退路） |
| `NEU_BOX_REAL_RUNC` | `/usr/local/bin/runc` | wrapper 后面真正接的 runtime |

```bash
neu-box-config show      # 生效值 + 每个值的来源（file / env / default）
neu-box-config init ...  # 手动生成 / 迁移
neu-box-config version   # 软件版本 + 它支持的配置 schema 版本
```

两个值需要和这台机器对上：

- `NEU_BOX_WORKER_URL` 的端口跟 worker 的 `NEU_BOX_PORT` 一致。执行
  `neu-box-config init --worker-url` 时由用户填写。
- `NEU_BOX_REAL_RUNC` 指向本机真正的 runc。执行
  `neu-box-config init --real-runc "$(command -v runc)"` 时由用户确认路径。

## 手工安装和调试

不用脚本时，按这四步做：

1. 把三个二进制装到 `/usr/local/bin`（`dnf install` 包，或照
   [`docs/packaging.md`](docs/packaging.md) 自己编）。
2. 生成配置：

   ```bash
   sudo neu-box-config init \
       --real-runc "$(command -v runc)" \
       --worker-url "http://127.0.0.1:<worker 的 NEU_BOX_PORT>"
   ```

3. 备份后改 `/etc/docker/daemon.json`：加 `default-runtime` 和
   `runtimes["neu-box-runtime"]` 两个键，内容见
   [`docs/runtime-hook.md`](docs/runtime-hook.md)（以那份为准）。
4. 重启 dockerd。

看现状：

| 现象 | 看什么 |
|---|---|
| 这台机器上所有容器都起不来 | `neu-box-config show` 里的 `NEU_BOX_REAL_RUNC` 是不是本机真正的 runc |
| 容器起得来，但里面设备全被拒 | 登记没成功。hook 的失败原因在 runc 的 stderr，也就是 `journalctl -u docker` |
| `docker info` 里的 DefaultRuntime 不对 | 检查 `/etc/docker/daemon.json`，修改后手动重启 dockerd |
| 想退回原生 runc | 手动移除 `daemon.json` 中 Neu Box 的两个键，再手动重启 dockerd |
