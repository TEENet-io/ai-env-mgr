# 企业微信通知通道(一次性,SLS 控制台)

webhook 地址不进仓库。步骤:

1. SLS 控制台 → 日志审计/告警 → 通知对象 → Webhook 集成 → 新建,类型「企业微信」,名称 `wecom-ops`,粘贴群机器人地址。
2. 行动策略 → 新建 `wecom-ops-policy`:所有等级 → `wecom-ops`;重复通知间隔 30 分钟;勾选「恢复时通知」。
3. 四条规则的 `policyConfiguration.actionPolicyId` 都是 `wecom-ops-policy`,名字对上即可,不用改 JSON。
4. 验证:按计划 Task 6 Step 4 逐条触发,把收到的消息记到验收记录。
