# Neu Box OCI runtime

本目录是 `neu_box` 仓库的一部分。发布入口为仓库根的
`deploy/build_release.py`；它在同一次发布中构建 Worker、client 和本目录的 runtime，
由 `neuboxd` RPM 一起安装。

容器接入 neu-box 沙盒的 **runtime 侧**。合包安装两个 runtime 二进制：

| 二进制 | 干什么 |
|---|---|
| `neu-box-runtime` | runc wrapper，占 Docker `default-runtime` 的位置：给带 annotation 的容器注入 hook，其余 argv 原样转发给真 runc |
| `neu-box-hook` | OCI hook：在容器 ENTRYPOINT 之前向 Worker 登记；登记失败或 Worker 不可达时拒绝受管容器启动 |

行为契约、hook phase 验证记录、边界 → [`docs/runtime-hook.md`](docs/runtime-hook.md)
构建与打包细节 → [`docs/packaging.md`](docs/packaging.md)
登记接口契约（字段、状态码）→ [`../../docs/worker-api.md`](../../docs/worker-api.md)

## 安装

```bash
# 单个 RPM 安装 Worker、client、runtime 的程序文件
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudoedit /etc/neu-box/worker.env  # RPM 已安装默认配置；按需修改
sudo /usr/libexec/neu-box/neuboxctl/neuboxctl setup
docker info --format '{{.DefaultRuntime}}'   # 应当输出 neu-box-runtime
```

单个 RPM 用仓库根的 `deploy/build_release.py` 构建。`setup` 会自动合并
`daemon.json`，需要重启时询问 `y/N`；选择 N 后按提示手动重启 Docker 并运行
`sudo /usr/libexec/neu-box/neuboxctl/neuboxctl resume`。详见 [部署手册](../../docs/deployment.md)。
`setup` 会打印实际使用的 Worker URL、hook 路径和 runc 路径；配置文件位于
`/etc/neu-box/runtime.env`。

## 升级

升级同样只装一个包：

```bash
sudo /usr/libexec/neu-box/neuboxctl/neuboxctl pause
sudo dnf install ./neuboxd-<version>-<release>.<arch>.rpm
sudo /usr/libexec/neu-box/neuboxctl/neuboxctl setup
```

Docker 未加载目标配置时，`setup` 会询问是否重启。

## 卸载

```bash
sudo /usr/libexec/neu-box/neuboxctl/neuboxctl pause
sudoedit /etc/docker/daemon.json  # 手动移除 default-runtime 和 runtimes.neu-box-runtime
sudo systemctl restart docker
sudo dnf remove neuboxd
```

移除 Docker 配置时保留文件里的其他键。RPM 不拥有 `runtime.env`，卸载不会清除它。

## 配置

`/etc/neu-box/runtime.env` 由 `neuboxctl setup` 生成和迁移。
除 `NEU_BOX_WORKER_URL` 每次按 Worker 的 `NEU_BOX_PORT` 同步外，手工改过的值会
保留；没有 `NEU_BOX_CONFIG_VERSION` 的文件按配置 schema 版本 0 迁移。

| 键 | 默认值 | 说明 |
|---|---|---|
| `NEU_BOX_WORKER_URL` | `http://127.0.0.1:59075` | Worker 地址，hook 往这里登记 |
| `NEU_BOX_HOOK` | `/usr/libexec/neu-box/neu-box-hook` | 注入进 config.json 的 hook 路径 |
| `NEU_BOX_HOOK_PHASE` | `createRuntime` | 注入到哪个 OCI 阶段（`prestart` 是退路） |
| `NEU_BOX_REAL_RUNC` | `/usr/local/bin/runc` | wrapper 后面真正接的 runtime |
| `NEU_BOX_CAP_GUARD` | `drop` | 全套 capability 容器的守卫策略：`drop`、`deny` 或 `off` |

两个值需要和这台机器对上：

- `NEU_BOX_WORKER_URL` 的端口跟 Worker 的 `NEU_BOX_PORT` 一致，`neuboxctl setup`
  每次同步它。
- `NEU_BOX_REAL_RUNC` 指向本机真正的 runc。`setup` 自动查找；若 Docker
  已安装但找不到 runc，执行 `sudo /usr/libexec/neu-box/neuboxctl/neuboxctl setup --real-runc /实际路径`。

## 安装检查和调试

安装单个 RPM 后运行 `sudo /usr/libexec/neu-box/neuboxctl/neuboxctl setup`。修改 `/etc/neu-box/worker.env` 的
`NEU_BOX_PORT` 后，先用 `neuboxctl pause` 停止运行中的 Worker，再运行 `setup`，
让 runtime 的 Worker 地址保持一致。
容器场景中，`setup` 备份并校验 `/etc/docker/daemon.json`，合并
`default-runtime` 和 `runtimes["neu-box-runtime"]` 两个键；需要重启 Docker 时
会询问。参见[部署手册](../../docs/deployment.md)。

看现状：

| 现象 | 看什么 |
|---|---|
| 这台机器上所有容器都起不来 | 查看 `/etc/neu-box/runtime.env` 的 `NEU_BOX_REAL_RUNC` 是否指向本机真正的 runc |
| 容器起得来，但里面设备全被拒 | 登记没成功。hook 的失败原因在 runc 的 stderr，也就是 `journalctl -u docker` |
| `docker info` 里的 DefaultRuntime 不对 | 查看 `setup` 提示，重启 Docker 后运行 `sudo /usr/libexec/neu-box/neuboxctl/neuboxctl resume` |
| 想退回原生 runc | 手动移除 `daemon.json` 中 Neu Box 的两个键，再手动重启 dockerd |
