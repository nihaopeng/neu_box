package cli

import (
	"fmt"
	"io"
)

func (a *app) printHelp() {
	a.printHelpTo(a.out)
}

func (a *app) printHelpTo(writer io.Writer) {
	fmt.Fprint(writer, `neubox — 终端沙盒隔离 / 命令任务提交

用法:
    neubox shell [资源选项...]
    neubox [--json] acquire [选项...]
    neubox [--json] submit [选项...] -- <command> [args...]
    neubox [--json] submit [选项...] --script FILE|-
    neubox [--json] release [sandbox_name]
    neubox [--json] cancel <id> [--kind task|acquire]
    neubox [--json] docker run <docker 参数...>
    neubox [--json] docker start <容器> [docker 参数...]
    neubox [--json] docker restart <容器>
    neubox [--json] docker status <容器>
    neubox [--json] {list|status|check|join|result} [参数]
    neubox [--json] tasks [--all] [--since 4h]
    neubox wait <task_id> [--interval 2s] [--timeout 0]

全局选项:
    --json                 只输出机器可读 JSON；失败信息写入 stderr

输出说明:
    默认模式               顶格两列 key: value，字段冒号对齐；无标题和操作提示
    JSON 模式              --json 可放在子命令前或紧跟子命令
                           成功结果写入 stdout，失败对象写入 stderr
    result --json          在任务元数据中增加 log 字段
    wait                   增量输出任务日志并等待终态；暂不支持 --json
    submit ... -- ...      -- 后面的参数属于目标命令，不再解析为 neubox 选项
    submit --script FILE|- 提交时读取完整脚本；- 从标准输入读取

命令说明:
    shell                  为子 shell 申请沙盒；退出自动释放，当前 shell 不变
    acquire                同步申请终端沙盒；返回成功时当前 shell 已进入沙盒
    submit                 异步提交命令任务；返回成功只表示任务已进入队列，
                           可用 --wait 直接跟踪，或事后使用 wait <task_id>
    wait <task_id>          增量跟踪日志直到 completed/failed
    result <task_id>        查询异步任务的当前状态、执行结果和完整日志
    docker run              透传 docker run，自动补上沙盒 annotation；
                           没有自己的选项，参数一个不改地交给 docker
    docker start            把停着的容器拉起来，并把当前 shell 的沙盒借给它
                           （容器名放最前面，docker 的选项跟在后面）；借卡失败则报错
    docker restart          停止运行中的受管容器，再借当前 shell 的沙盒启动
    docker status           查询容器运行状态和当前授权设备
    docker exec             不支持借卡；请使用原生 docker exec，先查 docker status

shell / acquire 资源选项:
    --device ID            指定一个卡号，可重复
    --devices 1,3          指定卡号，逗号分隔
    --device-num 2         自动分配卡数量；不指定任何设备选项时默认 1；
                           与 --device/--devices 互斥
    --cpu 4                CPU 核数，0 表示不限
    --mem 8                内存 GB，0 表示不限
    --pid 12345            仅 acquire：指定 PID；默认当前 shell
    --pid 只能指定宿主 PID；容器内申请会被拒绝

submit 选项:
    --device ID            指定一个卡号，可重复
    --devices 1,3          指定卡号，逗号分隔
    --device-num 2         自动分配卡数量；不指定任何设备选项时默认 1；
                           与 --device/--devices 互斥
    --cpu 4                CPU 核数，0 表示不限
    --mem 8                内存 GB，0 表示不限
    --priority 1           队列优先级，数值越大越先执行；0 表示普通
    --wait                 提交后直接跟踪日志和退出状态
    --image IMAGE          创建一次性 Docker 目标
    --workdir PATH         工作目录；Host 默认提交时的当前目录
    --container-user USER  Docker 目标内的用户
    --env KEY[=VALUE]      显式传入环境变量，可重复；不写 VALUE 则读取本地值
    --project              Docker：挂载当前目录到 /workspace（可写）并在其中运行
    --mount SRC:DST[:ro|rw] Docker：挂载已有路径，默认只读，可重复
    --output HOST[:DST]    Docker：创建并挂载可写输出目录，默认 DST=/outputs
    --command "..."        命令字符串；也可将命令放在 -- 后
    --script FILE|-        提交脚本原文快照；与 --command 和 -- 互斥

其他命令:
    list                         列出沙盒和资源
    status                       查看当前 shell 所在 sandbox
    check                        检查 Worker 可达性与 API 版本兼容性
    join <sandbox_name>          将 Host 当前 shell 加入已有 sandbox
    tasks                        查看任务队列；默认只显示活跃任务和最近 2h
                                 结束的任务（宁少勿刷屏）
    tasks --all                  显示 worker 返回的全部任务
    tasks --since 4h             自定义时间窗（30m / 6h / 12h 均可）
    result <task_id>             查看任务输出和结果
    wait <task_id>               跟踪增量日志并等待任务结束
    version                      显示客户端版本

示例:
    neubox shell --device-num 2
    neubox acquire --device-num 1
    neubox acquire --devices 1,3 --cpu 4 --mem 8
    neubox submit --device 1 -- npu-smi info
    neubox submit --device-num 1 --priority 1 -- python train.py
    neubox submit --wait --device-num 1 -- python train.py
    neubox submit --device-num 2 -- neubox docker run --rm training:latest python train.py
    neubox submit --device-num 2 -- neubox docker start train-1 -a
    neubox submit --device-num 2 --script train.sh
    neubox wait 7c65d5ac21f4
    neubox tasks --all           # 含较早结束的任务
    neubox release                # 释放当前 shell 的沙盒
    neubox release sbx_yuxd_12345.slice
    neubox cancel 7c65d5ac21f4            # 取消排队中/运行中的任务
    neubox cancel 9f0a1b2c3d4e --kind acquire
    neubox docker run --rm -it ubuntu bash
    neubox docker start my-container
    neubox docker status my-container
    neubox docker restart my-container
    docker exec -it my-container bash

环境变量:
    NEU_BOX_URL            Worker 地址，默认 http://127.0.0.1:59075
    NEU_BOX_USER           sandbox/任务用户名

容器必须带 sandbox_cgroup annotation：Worker 靠它把容器登记到沙盒名下，没登记的
容器即使卡空着也一律拿不到设备（fail-closed）。docker run 子命令会自动补上这行
annotation；直接用原生 docker 的话得自己写。客户端是静态二进制，运行时不依赖
Bash、curl 或 Python。

docker start 拿卡靠的不是 annotation（那是建容器时写死的、改不了），而是启动前
先存的一张借条：把当前 shell 所在沙盒借给这个容器，10 秒内有效、一次性。所以
跨 shell 换沙盒继续用同一个容器要走 neubox docker start；借卡失败会报错并
尝试停止容器。原生 docker start 不会建立新的借条，不能用它换卡。

运行中的容器不能靠 docker exec 改变授权设备；要换卡，先进入目标沙盒，再使用
neubox docker restart。restart 会中断容器工作，只支持创建时已有 annotation 的
受管容器；已停止的容器用 neubox docker start。neubox docker status 显示的是
Worker 当前授权记录，不是驱动健康检测。更多细节见 neubox docker help。

submit 的 Host 目标从提交时的工作目录运行；只有 --env 显式选择的变量会从
提交终端传入。Docker --project/--mount/--output 使用 Worker 节点上的宿主路径，
不上传文件；输出目录是可写 bind mount，容器结束后文件留在宿主机。
--script 在提交时读取原文，排队后修改本地文件不会改变已提交任务。脚本中的
路径仍位于执行节点；脚本由 Bash 执行，首行 shebang 不改变解释器，stdin
是 /dev/null。任务以入口命令或脚本退出为结束点；仅 docker start 或
docker run -d 很快返回，随后任务会清理其容器。需要等待原程序可用 start -a。
submit 的 --cpu/--mem 约束宿主任务沙盒内的进程；脚本里启动的 Docker 容器
不会自动继承，可在 docker run 中使用原生资源限制参数。

cancel 的语义：排队中的条目被摘出队列（任务留痕为 cancelled，记录与日志保留）；
运行中的任务发取消信号；已经拿到卡的 acquire 就地释放，不需要再补一次 release。
acquire 阻塞排队期间按 Ctrl-C 走的就是这条路径（退出码 130）。
`)
}
