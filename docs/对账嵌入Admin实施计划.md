# 账单对账嵌入 Admin 实施计划

版本：V1（方案评审稿）  
范围：第一阶段 AWS Bedrock 与 LiteLLM 对账；架构预留 Azure、GCP  
原则：Admin 负责管理、排队、展示；对账引擎负责读取和计算；凭据不进入页面和数据库。

## 1. 目标

把现有独立的 `billing-reconcile` 程序接入 Admin，使管理员可以在一个界面中：

- 手动发起指定月份的对账；
- 查看本次运行状态和历史记录；
- 查看 AWS 供应商费用、LiteLLM 费用、差额和异常；
- 下载或查看详细报告；
- 在任务中心统一看到对账任务；
- 让每日自动对账、失败重试和差额告警都由统一任务机制管理。

第一阶段只启用 AWS Bedrock。Azure 和 GCP 只接入统一接口，不在本阶段改变其生产账单配置。

## 2. 不采取的方案

不把 Python 对账代码直接编译进 Go Admin，也不在 HTTP 请求中同步执行完整账单下载和解析。

原因：

1. CUR 下载、Parquet 解析和 LiteLLM 查询可能持续数分钟；不能阻塞 Admin 请求或影响机器控制。
2. Python 依赖和虚拟环境不应成为 Admin 二进制的隐式运行时依赖。
3. AWS AK/SK、LiteLLM DSN 等凭据不应因此扩大到所有 Admin 进程权限。
4. 对账失败需要可重试、可接管、可审计，不能只依赖一个 HTTP 请求的生命周期。

## 3. 推荐架构

```text
Admin UI
  │  POST /reconciliation/run
  ▼
Admin PostgreSQL: tasks(kind=billing_reconcile)
  │
  ├── Admin 任务中心：等待、进行中、完成、异常
  │
  ▼
Billing Reconcile Worker
  │  独立 Unix 用户、独立凭据、独立 Python 环境
  │
  ├── AWS CUR 2.0 S3（只读）
  ├── LiteLLM PostgreSQL 受限视图（只读）
  ├── billing-reconcile SQLite（快照、输入、明细）
  └── 报告目录（Markdown / CSV / JSON）
  │
  ▼
Admin PostgreSQL 汇总结果 + 认证后的报告查看接口
```

执行器可以先与 Admin 部署在同一台 Linux 服务器，但必须是独立进程和独立系统用户。后续需要扩容时，可以将执行器移动到单独机器，不改变 Admin 页面和任务协议。

## 4. 任务模型

新增任务类型：

```text
billing_reconcile
```

不要复用现有的 `reconcile`，现有任务用于网关状态核对。

任务 payload 只包含业务参数，不包含凭据和文件路径：

```json
{
  "provider": "aws",
  "period": "2026-09",
  "scope": "bedrock",
  "requested_by": "admin"
}
```

幂等键：

```text
billing_reconcile:aws:bedrock:2026-09
```

同一供应商、范围和账期已有成功任务时，默认返回已有结果；失败任务可以由管理员显式重试。运行中的同一任务不能重复启动。

任务状态沿用现有任务队列：

```text
pending → running → succeeded
                   ├→ retry_wait → running
                   └→ failed
```

对账等待账单生效不算系统故障，使用明确状态 `waiting_for_bill`，避免反复重试造成误报。

## 5. 数据库改造

### 5.1 对账运行汇总表

新增 `billing_reconcile_runs`：

| 字段 | 说明 |
|---|---|
| id | 对账运行 ID |
| task_id | 对应 Admin 任务 |
| provider | `aws` / `azure` / `gcp` |
| scope | 第一阶段为 `bedrock` |
| period | `YYYY-MM` |
| status | running / completed / failed / waiting_for_bill |
| provider_cost | 供应商可比费用 |
| gateway_cost | LiteLLM 费用 |
| difference | 供应商费用减 LiteLLM 费用 |
| relative_difference | 相对差额 |
| issue_count | 问题数量 |
| report_path | 受控报告引用，不直接暴露文件系统路径 |
| report_hash | 报告完整性哈希 |
| mapping_hash | 路由配置哈希 |
| source_updated_at | 账单源更新时间 |
| started_at / finished_at | 运行时间 |
| error | 脱敏错误信息 |

### 5.2 问题摘要表

新增 `billing_reconcile_issues`，只保存页面需要的摘要：

- 类型：未匹配、未知路由、账单过期、数量差异、价格差异、测试/未托管用量；
- 供应商、账期、模型、区域、SKU；
- 数量、费用、差额；
- 是否已确认、确认人、确认时间；
- 详情文件中的行定位。

原始 Parquet、LiteLLM 输入快照和完整 CSV 不写入 Admin 主表，继续由对账程序保存到受限目录。

## 6. 对账执行器改造

在现有 `billing-reconcile` 基础上增加 Admin 集成模式：

1. 接收 task ID、provider、period；
2. 从受限配置读取 AWS AK/SK、LiteLLM DSN 和路由配置；
3. 执行现有 CLI 对账逻辑；
4. 输出机器可读的最终 JSON 摘要；
5. 将摘要、问题统计和报告引用回传 Admin；
6. 详细报告仍写入专用目录；
7. 退出码保持现有语义：成功、等待账单、配置/来源失败分别区分。

执行器必须：

- 使用 `exec.CommandContext` 或等价的受控进程调用；
- 禁止 shell 拼接和用户输入直接变成命令参数；
- 设置最大执行时间；
- 输出中禁止 AK/SK、DSN 密码、完整连接串和请求内容；
- 支持取消和租约丢失后的安全停止；
- 同一 provider + period 使用现有锁，防止并发下载和重复激活快照。

## 7. Admin 页面

### 7.1 导航

在「用量」下新增页签：

```text
用量 · 调用明细 · 费用对账
```

### 7.2 对账首页

页面分为四部分：

1. **当前状态**：AWS Bedrock、本月账期、最近运行时间、账单更新时间；
2. **金额卡片**：AWS 费用、LiteLLM 费用、差额、差额比例；
3. **问题摘要**：未托管用量、未知路由、账单过期、价格差异、数量差异；
4. **历史运行**：按供应商、账期、状态筛选，默认只显示最近记录，其余折叠。

操作按钮：

- 「运行本月对账」；
- 「运行上月对账」；
- 「重新运行」；
- 「查看报告」；
- 「导出摘要」。

点击后只创建任务，页面立即返回，不等待账单下载或数据库查询完成。

### 7.3 任务中心

任务中心增加：

```text
费用对账 · AWS Bedrock · 2026-09
```

卡片显示状态、账期、运行耗时、差额、问题数量和最近错误。详细历史默认折叠，只显示每个状态栏的第一条。

## 8. 自动调度

第一阶段保留两步：

1. Admin 每日创建上一账期和当前账期的对账任务；
2. 任务幂等键防止同一账期重复执行。

验收完成后，停用原来的独立 systemd timer，避免 Admin 调度和旧 timer 各自运行一份对账。

首次切换期间可以暂时保留旧 timer，但必须设置为只读观察模式，不能同时写入相同的 Admin 运行记录。

## 9. 告警规则

对账结果进入现有告警中心：

- 对账任务连续失败；
- 账单等待超过预期窗口；
- manifest 或账单快照过期；
- 差额绝对值超过阈值；
- 差额比例超过阈值；
- 出现未知路由或未托管 Bedrock 用量。

告警必须区分“账单未生效”和“账单真实差异”，不能把 AWS CUR 延迟直接判定为漏账。

## 10. 权限和凭据

### Admin 角色

| 角色 | 查看汇总 | 发起/重跑 | 确认差异 | 查看明细 |
|---|---:|---:|---:|---:|
| viewer | 是 | 否 | 否 | 脱敏摘要 |
| operator | 是 | 是 | 否 | 是 |
| security | 是 | 否 | 是 | 是 |
| admin | 是 | 是 | 是 | 是 |

### 凭据边界

- AWS 只读 AK/SK 留在 `/etc/billing-reconcile.env`，权限 600；
- LiteLLM 使用单独数据库只读账号和受限视图；
- Admin 页面不显示、编辑或回显任何凭据；
- 任务 payload、审计日志和错误信息不得包含凭据；
- AWS 只读取 Bedrock 可比账单，其他 AWS 费用保留在供应商报告中但不纳入 LiteLLM 差额。

## 11. 实施阶段

### Phase 0：接口和数据库设计

- 定义 `billing_reconcile` 任务 payload；
- 新增运行汇总和问题摘要迁移；
- 定义执行器 JSON 输出协议；
- 明确报告目录和访问权限。

完成条件：迁移可回滚，现有任务和对账 CLI 测试不受影响。

### Phase 1：AWS 手动对账

- Admin 新增对账页；
- 增加“运行本月/上月”按钮；
- Worker 调用现有 Python 对账程序；
- 任务中心显示执行状态；
- 展示费用、差额和问题数量。

完成条件：使用 2026-09 样本和线上 CUR 各运行一次，结果与现有独立程序一致。

### Phase 2：报告和告警

- 增加报告查看/下载；
- 增加差额和过期告警；
- 增加重跑、失败原因和审计记录；
- 验证凭据不会进入日志。

完成条件：人为制造失败、等待账单、超阈值三种情况，页面和告警状态正确。

### Phase 3：自动调度切换

- Admin 每日排队当前/上月账期；
- 观察一周重复执行、延迟和失败率；
- 停用旧 systemd timer；
- 保留回退脚本。

完成条件：连续 7 天没有重复账期、任务丢失或报告覆盖错误。

### Phase 4：Azure / GCP

- 复用 provider 适配器接口；
- 每家供应商独立凭据和账期状态；
- 页面按供应商筛选，不混合不同币种和费用口径；
- 通过真实脱敏样本后再启用生产调度。

## 12. 验收清单

- [ ] Admin 可以创建 AWS 指定月份对账任务；
- [ ] 请求返回不阻塞，任务中心能看到进行中状态；
- [ ] Worker 重启后任务可恢复，不重复激活快照；
- [ ] 同一账期重复点击不会生成重复运行；
- [ ] AWS、LiteLLM、差额和问题数量与独立程序一致；
- [ ] 失败、等待账单、超阈值均有明确状态；
- [ ] 报告只能由已登录且有权限的管理员访问；
- [ ] 日志和审计中没有 AK/SK、DSN 密码或完整连接串；
- [ ] 旧 systemd timer 停用后，Admin 调度仍能正常补跑；
- [ ] 不影响机器同步、软件安装、网关开户和现有任务队列；
- [ ] Azure/GCP 尚未配置时，不会显示为“对账正常”或零费用。

## 13. 回滚方案

1. 停止 Admin 的 `billing_reconcile` 调度，不再创建新任务；
2. 保留并单独启用现有 `billing-reconcile.timer`；
3. 回滚 Admin 页面和任务处理器版本；
4. 不删除 SQLite 快照、报告和 Admin 汇总记录；
5. 已完成的对账结果保持只读，避免回滚造成历史结果消失。

