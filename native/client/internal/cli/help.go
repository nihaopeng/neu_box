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

资源与任务:
    neubox shell --device-num 2             启动临时沙盒 shell；退出后自动释放
    neubox acquire --device-num 2           为当前终端申请沙盒；用 release 释放
    neubox status                           查看当前终端的沙盒与资源
    neubox release [sandbox]                释放当前或指定沙盒
    neubox submit --device-num 2 -- CMD...  提交宿主机命令；任务结束后释放资源
    neubox submit --device-num 2 --script FILE|-
                                            提交脚本快照；- 从标准输入读取
    neubox wait <task_id>                   跟踪任务日志和结束状态
    neubox result <task_id>                 查看任务结果与日志
    neubox cancel <id> [--kind task|acquire] 取消任务或申请

查看运行情况:
    neubox list                             查看所有用户的活跃任务、申请和沙盒
    neubox list --history                   加入最近 2 小时的结束记录
    neubox list --sandboxes                 只查看沙盒
    neubox tasks                            list 的别名
    neubox check                            检查 Worker 连接与版本

容器:
    neubox docker run <docker run 参数...>  在当前沙盒中创建并启动容器
    neubox docker start <container> -a     启动已停止容器并等待其主进程结束
    neubox docker restart <container>      在当前沙盒中重启受管容器
    neubox docker status <container>       查看容器状态与设备授权

常用选项:
    --device-num N      自动分配 N 张卡；默认 1；0 表示不申请卡
    --device ID         指定卡号，可重复；也可用 --devices 1,3
    --cpu N / --mem N   限制 CPU 核数 / 内存 GB；0 表示不限
    --priority 1        submit 高优先级；已有任务不会被抢占
    --wait              submit 后持续跟踪任务
    --json              输出机器可读 JSON

更多帮助:
    neubox help verbose                    运行原理与完整选项
    neubox docker help                     容器用法
`)
}

func (a *app) printVerboseHelp() {
	fmt.Fprint(a.out, `neubox — 运行原理与完整选项

沙盒与设备:
    shell 为新开的子 shell 借用设备，退出后自动释放；acquire 为当前终端
    借用设备，需执行 release。status 只查询当前终端；list 查询所有用户。
    容器在创建运行时由 Worker 登记，并继承所借沙盒的设备授权。
    已运行容器的授权不能通过 docker exec 更换；使用 docker restart 会
    先停止容器，再以当前终端的沙盒重新启动。原生 docker exec 只能沿用授权。

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
    Worker 节点。Docker 容器命令应以前台方式运行；docker start 可用 -a
    等待容器主进程，docker run -d 则不会延长任务寿命。

其他 submit 选项:
    --image IMAGE           使用一次性容器执行命令
    --command "..."         以字符串指定命令；也可使用 -- CMD...
    --workdir PATH          宿主机任务工作目录
    --container-user USER   容器内的执行用户
    --env KEY[=VALUE]       显式传入环境变量，可重复
    --project               将当前目录挂载到容器的 /workspace
    --mount SRC:DST[:ro|rw] 挂载 Worker 节点上的路径；默认只读
    --output HOST[:DST]     挂载可写输出目录；默认 DST=/outputs

输出与环境:
    --json                  成功响应为 JSON；可位于子命令前或之后
    wait、submit --wait     持续输出日志，不支持 --json
    result --json           任务信息包含 log 字段
    NEU_BOX_URL             Worker 地址，默认 http://127.0.0.1:59075
    NEU_BOX_USER            沙盒与任务使用的用户名

容器命令详情: neubox docker help verbose
`)
}
