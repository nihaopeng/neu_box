# Neu Box OCI runtime

本目录是 `neu_box` 仓库的一部分。发布入口为仓库根的
`deploy/build_release.py`；它用同一版本构建 Worker、client 和本目录的 runtime。
源码由原 `neu_box_runtime` 仓库的 `dea6365` 提交纳入，后续在本仓库维护。

容器接入 neu-box 沙盒的 **runtime 侧**。三个二进制：

| 二进制 | 干什么 |
|---|---|
| `neu-box-runtime` | runc wrapper，占 Docker `default-runtime` 的位置：给带 annotation 的容器注入 hook，其余 argv 原样转发给真 runc |
| `neu-box-hook` | OCI hook：在容器 ENTRYPOINT 之前把它登记到 Worker。**登记不上容器就起不来**（fail-closed） |
| `neu-box-config` | 生成/迁移 `/etc/neu-box/runtime.env` |

行为契约、hook phase 验证记录、边界 → [`docs/runtime-hook.md`](docs/runtime-hook.md)
构建与打包细节 → [`docs/packaging.md`](docs/packaging.md)
登记接口契约（字段、状态码）→ [`../../docs/worker-api.md`](../../docs/worker-api.md)

## 安装

```bash
# 装二进制 + 生成配置 + 注册成 Docker 默认 runtime
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm ./neu-box-runtime-<version>-<release>.<arch>.rpm
sudo bash /usr/libexec/neu-box/runtime/scripts/install.sh --configure-only

# 必须重启 dockerd：会杀掉这台机器上当时所有运行中的容器，进维护窗口做
docker ps
sudo systemctl restart docker
docker info --format '{{.DefaultRuntime}}'   # 应当输出 neu-box-runtime
```

两个 RPM 用仓库根的 `deploy/build_release.py` 一起构建。装完确认配置：
`neu-box-config show`。配置脚本不会替你重启 dockerd。

## 升级

升级跟安装是同一条命令，换个包就行：

```bash
sudo neuboxctl pause
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm ./neu-box-runtime-<version>-<release>.<arch>.rpm
sudo bash /usr/libexec/neu-box/runtime/scripts/install.sh --configure-only
sudo neuboxctl setup
```

换二进制、迁移配置、确认 daemon.json 都由它一次做完，重复跑也没事。

## 卸载

```bash
sudo bash /usr/libexec/neu-box/runtime/scripts/uninstall.sh  # 优先还原 daemon.json
sudo systemctl restart docker
# RPM 装的机器：重启之后再删包（顺序反了 dockerd 起不了容器）
sudo dnf remove neu-box-runtime
```

`uninstall.sh --purge` 连 `/etc/neu-box/runtime.env` 一起删；不加就留着。

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
neu-box-config init ...  # 重新生成 / 迁移（部署脚本替你调）
neu-box-config version   # 软件版本 + 它支持的配置 schema 版本
```

两个值需要和这台机器对上：

- `NEU_BOX_WORKER_URL` 的端口跟 worker 的 `NEU_BOX_PORT` 一致。这两个值住在两个
  文件里，`install.sh` 会从 `worker.env` 读出来交给配置工具。
- `NEU_BOX_REAL_RUNC` 指向本机真正的 runc。`install.sh` 会用 `command -v runc` 的
  结果填它；你自己手写的那份配置不会被覆盖。

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
| `docker info` 里的 DefaultRuntime 不对 | `sudo bash scripts/uninstall.sh` 之后重跑安装 |
| 想退回原生 runc | `sudo bash /usr/libexec/neu-box/runtime/scripts/uninstall.sh && sudo systemctl restart docker` |
