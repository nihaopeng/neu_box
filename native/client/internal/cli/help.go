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

常用命令:
    neubox shell --device-num 2
    neubox acquire --device-num 2
    neubox status
    neubox release
    neubox submit --device-num 2 -- python train.py
    neubox submit --device-num 2 --script train.sh
    neubox wait <task_id>
    neubox result <task_id>
    neubox tasks

容器命令:
    neubox docker run --rm -it ubuntu bash
    neubox docker start <container> -a
    neubox docker restart <container>
    neubox docker status <container>

详细帮助:
    neubox help verbose
    neubox docker help
`)
}

func (a *app) printVerboseHelp() {
	fmt.Fprint(a.out, `neubox — 完整命令参考

用法:
    neubox shell [资源选项]
    neubox [--json] acquire [资源选项] [--pid PID]
    neubox [--json] release [sandbox_name]
    neubox [--json] status|list|check
    neubox [--json] join <sandbox_name>
    neubox [--json] submit [选项] -- <command> [args...]
    neubox [--json] submit [选项] --script FILE|-
    neubox wait <task_id> [--interval 2s] [--timeout 0]
    neubox [--json] result <task_id>
    neubox [--json] tasks [--all] [--since 4h]
    neubox [--json] cancel <id> [--kind task|acquire]
    neubox [--json] docker run <docker run 参数...>
    neubox [--json] docker start <container> [docker start 参数...]
    neubox [--json] docker restart <container>
    neubox [--json] docker status <container>
    neubox version

资源选项（shell、acquire、submit）:
    --device ID             指定设备编号，可重复
    --devices 1,3           指定多个设备编号
    --device-num 2          自动分配设备数量，默认 1；不能与 --device/--devices 同用
    --cpu 4                 CPU 核数；0 表示不限
    --mem 8                 内存 GB；0 表示不限

submit 选项:
    --priority 1            队列优先级；数值越大越先执行
    --wait                  提交后持续输出日志，直至任务结束
    --script FILE|-         提交脚本内容；- 从标准输入读取
    --command "..."         以字符串指定命令；也可使用 -- <command> [args...]
    --image IMAGE           使用一次性容器执行命令
    --workdir PATH          工作目录
    --container-user USER   容器内的执行用户
    --env KEY[=VALUE]       显式传入环境变量，可重复
    --project               将当前目录挂载到容器内的 /workspace
    --mount SRC:DST[:ro|rw] 挂载节点上的路径，可重复；默认只读
    --output HOST[:DST]     挂载可写输出目录；默认 DST=/outputs

任务与容器:
    shell                   启动临时 shell；退出后自动释放沙盒
    acquire                 为当前终端申请沙盒；使用 release 释放
    submit                  提交异步任务；--wait 可直接等待结果
    --script                提交时保存脚本快照；仅用于宿主机任务
    docker run              在当前沙盒中创建容器；Docker 参数原样传递
    docker start            在当前沙盒中启动已停止的受管容器；-a 等待容器退出
    docker restart          重启运行中的受管容器；现有工作会中断
    docker status           查询容器状态及当前设备授权
    docker exec             进入运行中容器请使用 docker exec；该命令不能更换设备

输出与环境:
    --json                  输出 JSON；可位于子命令前或紧跟子命令
    wait、submit --wait     持续输出日志，不支持 --json
    result --json           任务信息包含 log 字段
    tasks                   默认显示活跃任务及最近 2 小时结束的任务
    NEU_BOX_URL             Worker 地址，默认 http://127.0.0.1:59075
    NEU_BOX_USER            沙盒与任务使用的用户名

示例:
    neubox shell --device-num 2
    neubox acquire --devices 1,3 --cpu 4 --mem 8
    neubox submit --wait --device-num 1 -- python train.py
    neubox submit --device-num 2 --script train.sh
    neubox submit --device-num 2 -- neubox docker run --rm training:latest python train.py
    neubox submit --device-num 2 -- neubox docker start train-1 -a
    neubox docker run --rm -it ubuntu bash
    neubox docker status train-1
    docker exec -it train-1 bash

容器命令详情: neubox docker help verbose
`)
}
