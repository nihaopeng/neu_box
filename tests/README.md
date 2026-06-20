# Tests

测试针对已部署的 neu_box Master 发起真实 HTTP 请求。所有测试均通过 `common.py` 共享模块调用 API。

## 运行

```bash
# 全部测试（逐个文件手动跑）
python3 tests/test_nodes.py
python3 tests/test_queue.py
python3 tests/test_command.py
python3 tests/test_experiments.py

# 快速模式（缩短 sleep 等待时间）
python3 tests/test_queue.py --quick

# 只跑匹配的测试
python3 tests/test_queue.py --test=并发
python3 tests/test_command.py --test=提交

# 指定 Master 地址（默认 http://202.199.13.164:25565）
NEU_BOX_MASTER=http://127.0.0.1:25565 python3 tests/test_nodes.py
```

## 文件说明

| 文件 | 内容 |
|---|---|
| `common.py` | HTTP 请求 (`get`/`post`/`put`/`delete`)、断言 (`assert_ok`/`assert_eq`/`assert_gt`/`assert_in`)、`run_tests()` 框架 |
| `test_nodes.py` | 所有节点在线、单节点 `/status` 字段完整、`/config` 列表可读 |
| `test_queue.py` | GPU 并发排队（不超过可用数）、FIFO 顺序、批量删除 |
| `test_command.py` | 必填字段校验、`stdout`/`stderr` 内容验证、多种资源配置 |
| `test_experiments.py` | 实验 CRUD、按标题/标签/创建者搜索、空实验 |

## 要求

- 测试机器能访问 Master
- 如果走代理连不上，先执行 `proxy_off` 关掉代理
- Worker 节点上存在 `pengyt` 和 `lipz` 两个系统用户（用于提交任务）
- 不会修改 `config.json` 中的节点配置（只读）
