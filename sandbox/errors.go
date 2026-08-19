package sandbox

import (
	"fmt"
	"strings"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func orchError(action string, err error, inst, user, image, status string) error {
	if err == nil {
		return nil
	}
	kind := sandboxerr.Classify(err)
	reason := chineseReason(kind, err)
	parts := make([]string, 0, 4)
	if inst != "" {
		parts = append(parts, "实例="+inst)
	}
	if user != "" {
		parts = append(parts, "用户="+user)
	}
	if image != "" {
		parts = append(parts, "镜像="+image)
	}
	if status != "" {
		parts = append(parts, "状态="+status)
	}
	if len(parts) == 0 {
		return sandboxerr.WrapKind(action, kind, fmt.Errorf("%s失败：%s: %w", action, reason, err))
	}
	return sandboxerr.WrapKind(action, kind, fmt.Errorf("%s失败：%s；%s: %w", action, strings.Join(parts, ", "), reason, err))
}

func chineseReason(kind sandboxerr.Kind, err error) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	switch kind {
	case sandboxerr.KindInvalid:
		switch {
		case strings.Contains(msg, "instance id is empty"):
			return "实例 ID 不能为空"
		case strings.Contains(msg, "user id is empty"):
			return "用户 ID 不能为空"
		case strings.Contains(msg, "image is required"):
			return "镜像名称不能为空"
		case strings.Contains(msg, "not running"):
			return "实例未运行"
		default:
			if msg == "" {
				return "参数无效"
			}
			return "参数无效（" + msg + "）"
		}
	case sandboxerr.KindNotFound:
		switch {
		case strings.Contains(msg, "镜像不存在") || strings.Contains(lower, "image") || strings.Contains(lower, "pull"):
			return "指定的镜像不存在或拉取失败"
		case strings.Contains(msg, "注入文件"):
			return "注入文件不存在"
		case strings.Contains(msg, "工作区") || strings.Contains(msg, "卷"):
			return "指定的工作区卷不存在"
		default:
			return "指定的实例不存在"
		}
	case sandboxerr.KindConflict:
		switch {
		case strings.Contains(msg, "destroyed") || strings.Contains(msg, "use restore"):
			return "实例已销毁，请使用还原而不是重新创建"
		case strings.Contains(msg, "reached max"):
			return "已达活跃实例上限，请稍后重试或换用新的 instance_id"
		case strings.Contains(msg, "端口池已满") || strings.Contains(lower, "port pool"):
			return "宿主机端口池已满（最多 500 个）"
		case strings.Contains(msg, "already exists"):
			return "实例已存在"
		default:
			return "实例已存在或当前状态不允许该操作"
		}
	case sandboxerr.KindTimeout:
		return "等待服务就绪超时"
	case sandboxerr.KindCanceled:
		return "操作已取消"
	case sandboxerr.KindUnavailable:
		return "Docker 守护进程未运行或无法连接"
	default:
		if msg == "" {
			return "发生未知错误"
		}
		return "发生未知错误（" + msg + "）"
	}
}
