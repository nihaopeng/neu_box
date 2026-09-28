# Neu Box Worker 测试

构建完成、发布前必须执行测试。以下命令假设已执行 `uv sync --frozen --all-groups`。

## 单元测试

```bash
uv run --frozen pytest -q tests/unit
```

## 集成测试

```bash
uv run --frozen pytest -q tests/integration
```

未提供 hook 二进制时，跨仓库用例会 skip；发布前建议显式指定：

```bash
NEU_BOX_HOOK_BIN=/path/to/neu-box-hook uv run --frozen pytest -q tests/integration
```

## native sandbox 单测

```bash
make -C native/sandbox BUILD_DIR="$PWD/build/native-sandbox" test
```

完整构建会执行 native `make all test`、PyInstaller 打包和验收套件 `--self-check`：

```bash
uv run --frozen --group build deploy/build_release.py
```

## 部署后验收

部署后验收套件会通过 Ascend 驱动的
`/proc/uda/namespace_node` 检查容器设备隔离。普通容器测试可使用本机已有、带
`/bin/sh` 的镜像，例如 `alpine:3.20`；UDA 测试另需本机已有、包含 Python、
`torch_npu` 和与宿主驱动兼容的 CANN 用户态的镜像。套件不会拉取镜像，也不会跳过
缺少前置条件的用例。

在维护窗口以 root 执行；把示例中的镜像名换成本机实际安装的镜像：

```bash
docker image inspect alpine:3.20 your-ascend-image:tag
sudo env NEU_BOX_CONTAINER_IMAGE=alpine:3.20 \
  NEU_BOX_DRIVER_PROBE_IMAGE=your-ascend-image:tag /usr/libexec/neu-box/bin/neuboxctl test
```

套件还验证真实 `neubox submit --script`：文件脚本在提交时保存快照、stdin 脚本
保留退出码、脚本中的 `neubox docker run --rm` 在任务结束后释放容器和设备。
完整前置条件和运行说明见[部署与升级手册](deployment.md#api-与实机验收)。
