package cli

import (
	"fmt"
	"io"
)

func (a *app) printHelp() {
	a.printHelpTo(a.out)
}

func (a *app) printHelpTo(writer io.Writer) {
	fmt.Fprint(writer, `neubox — 设备沙盒与任务

终端沙盒:
    neubox shell --device-num 2            启动临时 shell；退出后释放沙盒
    neubox acquire --device-num 2          为当前终端申请沙盒；用 release 释放
    neubox status                          查看当前终端的沙盒与资源
    neubox release [sandbox]               释放当前或指定沙盒

任务示例（--wait 持续跟踪日志和结果）:
    宿主机命令:
        neubox submit --wait --device-num 2 -- python train.py
        neubox submit --wait --device-num 2 --script train.sh

    新容器；Docker 参数直接写在 run 后，任务结束后删除容器:
        neubox submit --wait --device-num 2 -- neubox docker run --rm \
            -v /data:/data -e EPOCHS=10 training:latest python /data/train.py

    已停止的受管容器；运行创建时指定的程序，并等待它结束:
        neubox submit --wait --device-num 2 -- neubox docker start train-1 -a

    已停止的受管容器；启动后依次执行命令，脚本结束后停止并保留容器:
        neubox submit --wait --device-num 2 --script - <<'SH'
set -e
neubox docker start dev
docker exec dev python /workspace/prepare.py
docker exec dev python /workspace/train.py
SH

    已运行容器；查看当前授权，exec 沿用该授权、不能换卡:
        neubox docker status train-1
        docker exec train-1 sh -c 'echo ready'

    已运行容器需要换卡；先申请目标沙盒，再中断并重启原程序:
        neubox acquire --device-num 2
        neubox docker restart train-1

查看与控制:
    neubox list                            查看所有用户的活跃任务、申请和沙盒
    neubox list --history                  加入最近 2 小时的结束记录
    neubox list --sandboxes                只查看沙盒
    neubox tasks                           list 的别名
    neubox check                           检查 Worker 连接与版本
    neubox wait <task_id>                  跟踪任务日志和结束状态
    neubox result <task_id>                查看任务结果与日志
    neubox cancel <id> [--kind task|acquire] 取消任务或申请
    neubox docker restart <container>     在当前沙盒中重启运行中容器；会中断原程序
    neubox docker status <container>      查看容器状态与设备授权

常用选项:
    --device-num N      自动分配 N 张卡；默认 1；0 表示不申请卡
    --device ID         指定卡号，可重复；也可用 --devices 1,3
    --cpu N / --mem N   限制 CPU 核数 / 内存 GB；0 表示不限
    --priority 1        submit 高优先级；已有任务不会被抢占
    --wait              submit 后持续跟踪任务
    --json              输出机器可读 JSON

运行原理与完整选项: neubox help verbose
`)
}

func (a *app) printVerboseHelp() {
	fmt.Fprint(a.out, `neubox — 运行原理与完整选项

沙盒与设备:
    shell 为新开的子 shell 借用设备，退出后自动释放；acquire 为当前终端
    借用设备，需执行 release。status 只查询当前终端；list 查询所有用户。
    neubox docker run 的 Docker 参数原样传递；客户端只补充沙盒标识。
    neubox docker start 仅启动已停止的受管容器，在启动时绑定当前沙盒。
    已运行容器不能通过 docker exec 更换设备授权；原生 docker exec 沿用
    容器已有的授权。neubox docker restart 会先停止容器，再在当前沙盒中
    重新启动创建时配置的程序；现有工作会中断。任务不会自动接管运行中容器。

排队与位置:
    submit 和 acquire 共用同一个队列。position 的 priority 是优先级，
    rank 是该优先级内的排位，从 1 开始。priority 1 高于普通的 0；
    高优先级等待设备时，新普通任务不能占用它所需的设备。已运行的任务
    不被抢占；不需要这些设备的任务仍可运行。
    list 默认显示活跃条目；--history 加入最近 2 小时结束的记录，
    --since 4h 指定时间窗，--all 显示 Worker 返回的全部最近记录。
    list --sandboxes 只显示沙盒。tasks 与 list 相同。

任务结束与日志:
    submit 返回成功表示进入队列，使用 wait 跟踪日志或 result 查询结果。
    宿主机任务以入口命令或脚本退出为结束点，随后清理任务沙盒及其容器。
    --script 在提交时保存原文；脚本由 Bash 执行，其他引用文件仍须存在于
    Worker 节点。脚本中的 neubox docker start 负责借卡；后续 docker exec
    只执行命令。容器创建时配置的主程序也会运行，必须能持续到 exec 结束。
    脚本退出后，本任务启动的容器会停止；带 --rm 的容器由 Docker 删除。
    单独使用 docker start 而不加 -a，或使用 docker run -d，会很快返回，
    不会延长任务寿命。需要等待原程序结束时使用 docker start -a。

submit 选项:
    --command "..."         以字符串指定命令；也可使用 -- CMD...
    --workdir PATH          宿主机任务工作目录
    --env KEY[=VALUE]       显式传入环境变量，可重复
    --image IMAGE           旧的直接 Docker 目标入口；新任务建议使用
                            submit -- neubox docker run <Docker 参数...>
    --container-user USER   直接 Docker 目标内的执行用户
    --project               将当前目录挂载到容器的 /workspace
    --mount SRC:DST[:ro|rw] 挂载 Worker 节点上的路径；默认只读
    --output HOST[:DST]     挂载可写输出目录；默认 DST=/outputs

容器命令:
    neubox docker run <docker run 参数...>      参数原样传给 docker run
    neubox docker start <容器> [start 参数...]  启动已停止的受管容器
    neubox docker restart <容器>              重启运行中的受管容器
    neubox docker status <容器>               查看状态与当前授权

输出与环境:
    --json                  成功响应为 JSON；可位于子命令前或之后
    wait、submit --wait     持续输出日志，不支持 --json
    result --json           任务信息包含 log 字段
    NEU_BOX_URL             Worker 地址，默认 http://127.0.0.1:59075
    NEU_BOX_USER            沙盒与任务使用的用户名

完整示例: neubox help
`)
}
