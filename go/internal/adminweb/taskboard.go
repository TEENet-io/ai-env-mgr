package adminweb

// Task cards are a read-only projection; moving a card never drives execution.
type taskCard struct {
	ID, Title, Subject, Detail, Status, Updated, NextRun, Error string
	Attempts                                                    int
	Cancelable                                                  bool
}

type taskColumn struct {
	Title, Tone string
	Cards       []taskCard
}

type taskAttemptView struct {
	Attempt                               int
	Owner, Outcome, Error, Started, Ended string
}

type taskDetailView struct {
	ID, Kind, Status, Subject, Detail, Error, Created, Updated, Finished string
	Attempts                                                             []taskAttemptView
	CanRetry, CanCancel                                                  bool
}

type taskKindOption struct {
	Value, Label string
}

var taskKindOptions = []taskKindOption{
	{Value: "application", Label: "软件安装"},
	{Value: "admin_job", Label: "后台发布 / 上传"},
	{Value: "gateway_provision", Label: "网关授权"},
	{Value: "gateway_revoke", Label: "撤销网关授权"},
	{Value: "gateway_delete", Label: "删除网关账号"},
	{Value: "oss_export", Label: "设备配置同步"},
	{Value: "reconcile", Label: "网关状态核对"},
	{Value: "release_scan", Label: "安装包扫描"},
	{Value: "usage_snapshot", Label: "用量汇总"},
	{Value: "alert_eval", Label: "告警检查"},
	{Value: "alert_notify", Label: "告警通知"},
	{Value: "audit_publish", Label: "审计发布"},
	{Value: "status_import", Label: "状态导入"},
	{Value: "credential_rotation", Label: "凭据轮换"},
	{Value: "device_prune", Label: "设备清理"},
	{Value: "data_cleanup", Label: "采集数据清理"},
}

func taskStatusMatches(status, filter string) bool {
	switch filter {
	case "open":
		return status == "pending" || status == "queued" || status == "retry_wait" || status == "deferred" || status == "running" || status == "downloading" || status == "verifying" || status == "installing" || status == "verifying_install" || status == "reboot_required"
	case "failed":
		return status == "failed" || status == "blocked"
	case "done":
		return status == "succeeded" || status == "cancelled" || status == "superseded"
	default:
		return true
	}
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
		"admin_job":         "后台发布 / 上传",
		"gateway_provision": "网关授权", "gateway_revoke": "撤销网关授权",
		"gateway_delete": "删除网关账号", "oss_export": "设备配置同步",
		"reconcile": "网关状态核对", "release_scan": "安装包扫描",
		"usage_snapshot": "用量汇总", "alert_eval": "告警检查",
		"credential_rotation": "凭据轮换", "device_prune": "设备清理",
		"data_cleanup": "采集数据清理",
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
		columns[col].Cards = append(columns[col].Cards, taskCard{ID: app.TaskID, Title: "软件安装", Subject: app.Machine, Detail: app.AppID + " · " + app.Version, Status: label, Updated: app.Updated, Error: app.LastError, Cancelable: state == "pending" || state == "running"})
	}
	for _, task := range tasks {
		col, label := taskPlacement(task.Status)
		subject := task.Employee
		if subject == "" {
			subject = "系统任务"
		}
		detail := task.Detail
		if detail == "" {
			detail = task.Kind
		}
		columns[col].Cards = append(columns[col].Cards, taskCard{ID: task.ID, Title: taskTitle(task.Kind), Subject: subject, Detail: detail, Status: label, Updated: task.Updated, NextRun: task.NextRun, Error: task.LastError, Attempts: task.Attempts})
	}
	return columns
}
