package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func rejectCreateWithoutInstance() error {
	return sandboxerr.WrapKind("create", sandboxerr.KindInvalid,
		fmt.Errorf("%w: create requires -instance; use legacy-create for raw containers", sandboxerr.ErrInvalid))
}

func formatInstanceHuman(inst sandbox.Instance) string {
	ports := joinIntsHuman(inst.HostPorts)
	line := fmt.Sprintf("id=%s status=%s image=%s hostports=%s", inst.ID, inst.Status, inst.Image, ports)
	if len(inst.HostPorts) > 0 {
		line += fmt.Sprintf(" url=http://127.0.0.1:%d", inst.HostPorts[0])
	}
	return line
}

func joinIntsHuman(ports []int) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, ",")
}

func formatWaitHeartbeat(elapsedMS int64, attempt int, detail string) string {
	elapsed := time.Duration(elapsedMS) * time.Millisecond
	return fmt.Sprintf("wait: elapsed=%s attempt=%d last=%s\n", elapsed.Truncate(time.Second), attempt, detail)
}

type psResult struct {
	Instances []sandbox.Instance    `json:"instances"`
	PortPool  dockerx.PortPoolStats `json:"port_pool"`
}

func formatHuman(out any) string {
	switch v := out.(type) {
	case sandbox.Instance:
		return formatInstanceHuman(v) + "\n"
	case []sandbox.Instance:
		if len(v) == 0 {
			return "instances: none\n"
		}
		var b strings.Builder
		for _, inst := range v {
			b.WriteString(formatInstanceHuman(inst))
			b.WriteByte('\n')
		}
		return b.String()
	case psResult:
		return formatHuman(v.Instances) + fmt.Sprintf("ports in_use=%d size=%d warn=%v restrict=%v\n",
			v.PortPool.InUse, v.PortPool.Size, v.PortPool.Warn, v.PortPool.RestrictMultiPort)
	case map[string]string:
		var b strings.Builder
		for k, val := range v {
			fmt.Fprintf(&b, "%s=%s\n", k, val)
		}
		return b.String()
	default:
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Sprintf("%v\n", v)
		}
		return string(raw) + "\n"
	}
}

func writeOutput(jsonMode bool, out any) error {
	if out == nil {
		return nil
	}
	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	_, err := io.WriteString(os.Stdout, formatHuman(out))
	return err
}

type cliObserver struct {
	jsonMode bool
	human    io.Writer
	inner    sandbox.LogObserver
}

func (o cliObserver) Observe(ctx context.Context, event sandbox.OperationEvent) {
	if !o.jsonMode && event.Stage == "wait.retry" && o.human != nil {
		_, _ = io.WriteString(o.human, formatWaitHeartbeat(event.Duration.Milliseconds(), event.Attempt, event.Detail))
	}
	o.inner.Observe(ctx, event)
}

func usageText() string {
	return `usage: sandboxctl [global flags] <command> [command flags]

instance commands（平台主键是 instance_id）:
  create   -instance ID -user USER -image IMAGE [-memory 64m] [-cpus 0.5] [-expose 80] [-cmd "..."] [-net-instance]
  ensure   同上，并 Start + WaitReady（可加 -http/-tcp/-exec）
  start    INSTANCE
  stop     [-t SECONDS] INSTANCE
  restart  [-t SECONDS] INSTANCE
  destroy  INSTANCE [-purge]          # 下课保数据；-purge 清空作业
  restore  INSTANCE [-reset]          # 保数据重建；-reset 清空作业
  inspect  INSTANCE                   # 有 HostPort 时打印 http://127.0.0.1:<port>
  ps                                  # 列出实例和端口池水位
  wait     INSTANCE [-http PATH] [-tcp] [-exec CMD] [-port N]

legacy container commands:
  legacy-create -image IMAGE [-name NAME] [-user USER] [-cmd "..."] [-publish 8080:80/tcp] [-memory 64m] [-rm]
  remove   [-force] [-volumes] CONTAINER_ID   # 删容器，不走编排器
  containers | images | ping | version | info
  active USER | inactive USER | reap [USER]
  volume INSTANCE | volumes
  build    -tag NAME -base IMAGE [-dockerfile PATH] [-env K=V] [-label K=V] [-inject host:dest] [-step "RUN ..."] [-workdir DIR] [-cmd "..."] [-no-cache] [-pull]

global flags:
  -json          脚本用 JSON；默认人类可读
  -journal PATH  补偿失败落盘，进程重启后可重放
  -host -proxy -no-proxy -timeout -prefix
  -default-memory 256m  -default-cpus 0.5  -max-instances 300

`
}
