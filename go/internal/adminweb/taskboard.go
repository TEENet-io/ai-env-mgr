package adminweb

// Task cards are a read-only projection; moving a card never drives execution.
type taskCard struct {
	ID, Title, Subject, Detail, Status, Updated, NextRun, Error string
	Attempts                                                    int
}

type taskColumn struct {
	Title, Tone string
	Cards       []taskCard
}

func taskPlacement(state string) (int, string) {
	switch state {
	case "", "pending", "queued":
		return 0, "等待处理"
	case "retry_wait":
		return 0, "等待重试"
	case "deferred":
		return 0, "暂缓执行"
	case "running", "downloading", "verifying", "installing", "verifying_install":
		return 1, "进行中"
	case "reboot_required":
		return 1, "等待重启"
	case "succeeded":
		return 2, "已完成"
	case "cancelled":
		return 2, "已取消"
	case "superseded":
		return 2, "已被新任务替代"
	case "failed":
		return 3, "失败"
	case "blocked":
		return 3, "被阻止"
	default:
		return 3, "未知状态：" + state
	}
}

func taskTitle(kind string) string {
	names := map[string]string{
		"gateway_provision": "网关授权", "gateway_revoke": "撤销网关授权",
		"gateway_delete": "删除网关账号", "oss_export": "设备配置同步",
		"reconcile": "网关状态核对", "release_scan": "安装包扫描",
		"usage_snapshot": "用量汇总", "alert_eval": "告警检查",
		"credential_rotation": "凭据轮换", "device_prune": "设备清理",
	}
	if title, ok := names[kind]; ok {
		return title
	}
	return kind
}

func buildTaskBoard(tasks []taskRow, apps []applicationTaskRow) []taskColumn {
	columns := []taskColumn{{Title: "等待处理", Tone: "waiting"}, {Title: "进行中", Tone: "running"}, {Title: "已完成", Tone: "done"}, {Title: "异常", Tone: "failed"}}
	// Foreground install requests precede routine Worker jobs in each column.
	for _, app := range apps {
		state := app.State
		if app.Desired != "installed" {
			state = "cancelled"
		}
		col, label := taskPlacement(state)
		if state == "" {
			label = "等待 Agent 回报"
		}
		columns[col].Cards = append(columns[col].Cards, taskCard{ID: app.TaskID, Title: "软件安装", Subject: app.Machine, Detail: app.AppID + " · " + app.Version, Status: label, Updated: app.Updated, Error: app.LastError})
	}
	for _, task := range tasks {
		col, label := taskPlacement(task.Status)
		subject := task.Employee
		if subject == "" {
			subject = "系统任务"
		}
		columns[col].Cards = append(columns[col].Cards, taskCard{ID: task.ID, Title: taskTitle(task.Kind), Subject: subject, Detail: task.Kind, Status: label, Updated: task.Updated, NextRun: task.NextRun, Error: task.LastError, Attempts: task.Attempts})
	}
	return columns
}
