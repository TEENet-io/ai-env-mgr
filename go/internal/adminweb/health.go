package adminweb

import "github.com/TEENet-io/ai-env-mgr/internal/admincore"

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
