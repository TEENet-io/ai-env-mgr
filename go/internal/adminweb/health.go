package adminweb

import (
	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
)

// stateLabel renders the shared classification with this console's wording.
//
// The console decides nothing here: admincore.Health owns the rules, and both
// front ends read from it, so the terminal and the browser can never disagree
// about whether a machine is in trouble.
//
// The distinctions matter and are worth the extra words. A machine that
// announced it was suspending is not the same as one that went quiet -- that
// difference is the whole reason the agent reports lifecycle events -- and
// "quiet for ten minutes" (an evening) is not "quiet for three days" (go and
// look).
func stateLabel(m admincore.MachineState) string {
	switch m.Health() {
	case admincore.HealthNoReport:
		return "从未上报"
	case admincore.HealthUnbound:
		return "未分配"
	case admincore.HealthDisabledUser:
		return "已离职"
	case admincore.HealthUserMissing:
		return "缺少用户"
	case admincore.HealthCredsPending:
		return "待代登录"
	case admincore.HealthStopped:
		return "已关机"
	case admincore.HealthSleeping:
		return "休眠中"
	case admincore.HealthHibernated:
		return "休眠中"
	case admincore.HealthAgentDown:
		return "agent 无响应"
	case admincore.HealthCloudMissing:
		return "云端已删除"
	case admincore.HealthStale:
		return "长期失联"
	case admincore.HealthOffline:
		return "离线"
	case admincore.HealthErrors:
		return "有报错"
	default:
		return "正常"
	}
}

// stateSeverity picks the colour, from the same three-way grouping the fleet
// strip counts by.
func stateSeverity(m admincore.MachineState) string {
	return m.Health().Severity()
}

// codexNote is the tag beside a machine's Codex version: what is keeping it
// from the published one, or nothing when it is already there.
//
// A version on its own does not answer the question the administrator has
// after publishing, which is "did it land". A machine can sit on an older
// version for three quite different reasons -- it is still working through
// the update, it deferred because Codex was open, or the install failed -- and
// only the first resolves itself.
func codexNote(m admincore.MachineState, target string) string {
	switch m.Status.CodexState {
	case agentcore.CodexDownloading:
		return "下载中"
	case agentcore.CodexInstalling:
		return "安装中"
	case agentcore.CodexDeferred:
		// Not a fault: Codex was in use or the disk was full, and it will try
		// again. Worth showing so a machine that never moves gets noticed.
		return "已推迟"
	case agentcore.CodexFailed:
		return "安装失败"
	}
	if target == "" || m.Status.CodexVersion == target {
		return ""
	}
	// Only say "pending" about a machine that could actually act on it.
	//
	// A machine that is asleep, shut down, or has never reported is behind on
	// every published version by definition, and the state column already says
	// why. Tagging those too put "待更新" on nearly every row and buried the one
	// machine whose install had failed -- the same way one Errors bucket once
	// made ordinary onboarding look broken.
	switch m.Health() {
	case admincore.HealthOK, admincore.HealthErrors,
		admincore.HealthUnbound, admincore.HealthUserMissing,
		admincore.HealthCredsPending, admincore.HealthDisabledUser:
		return "待更新"
	default:
		return ""
	}
}

// codexNoteSeverity colours the tag. Only a failure is an actual problem; the
// rest are stages of an update that is still moving.
func codexNoteSeverity(note string) string {
	switch note {
	case "安装失败":
		return "err"
	case "":
		return ""
	default:
		return "warn"
	}
}
