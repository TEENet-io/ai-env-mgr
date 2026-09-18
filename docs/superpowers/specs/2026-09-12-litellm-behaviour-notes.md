# LiteLLM 行为核实记录(RDS 阶段 0 Task 0.2)

> 日期:2026-09-18 · 网关:`litellm.teenet.app`(AWS 新加坡机,镜像 `ghcr.io/berriai/litellm@sha256:20b5044b…`)
> 方法:在生产网关上建临时用户 `emp-test-budget`,实调一次 `gemini-2.5-flash-lite`,逐项观察后删除。临时用户与两把键已清理。
> 用途:决定 RDS 里 `employee_quotas` 的周期规则、关户/重发令牌的流程,以及对账口径。

## 1. 预算周期是自然月,不是"开户 + 一个月"

- 2026-09-18 建的用户,`budget_duration: 1mo`,`budget_reset_at` 返回 **`2026-10-01T00:00:00Z`**。
- 结论:**按 UTC 自然月 1 日零点重置**,与开户日期无关。
- 对设计的影响:`employee_quotas.period_rule` 取 `calendar_month_utc`(原计划里写的 `gateway_1mo` 作废);`period_tz` 仍保留,但要注明"网关按 UTC 切,页面展示可按 Asia/Shanghai,统计边界以 UTC 为准",不要在两个时区之间换算后再去对网关的数。
- 仍待观察:跨月那一刻是否真的清零(本次无法快进时间)。10 月 1 日后复查一次 `spend` 与 `budget_reset_at`,把结果补在本文件。

## 2. 改额度不会清空已花费

- `POST /user/update` 把 `max_budget` 从 0.01 改到 5,`spend` 保持原值,`budget_reset_at` 不变。
- 对设计的影响:控制台"改额度"是纯粹的额度变更,不触发周期重置,也不需要额外补偿逻辑;`employee_quotas` 只存当前额度与版本,已花费始终由网关持有。

## 3. 用量入账很快,归属到员工

- 一次调用后约 5 秒,`/user/info` 的 `spend` 从 0 变为 3.3e-06,`LiteLLM_SpendLogs` 里该行的 `user` 就是 `emp-test-budget`。
- 对设计的影响:预算告警和页面展示可以直接读网关的 `spend`,不必自己累加;RDS 不复制每条调用(与 spec §9.4 一致)。

## 4. 删键后立即失效,查询返回 404

- `POST /key/delete`(按别名)返回 `{"deleted_keys": [...]}`;随后 `GET /key/info` 返回 **404**,用该键调用返回 **401**。
- 对设计的影响:关户任务判断"吊销已生效"的依据是 `key/info` 404 或 `key/list` 里查不到该别名;删一个不存在的键按成功处理(spec §6.3)。

## 5. 删用户会级联删除它名下的键

- 给用户再发一把键后 `POST /user/delete`,`user/info` 返回 404,`key/list` 里该用户的**所有键都不见了**。
- 对设计的影响:关户**不要**用 `user/delete`。员工要保留历史用量与审计,应当只删键(`key/delete`)、把用户的额度/状态留在网关;`user/delete` 只用于清理测试账号。现有 `Offboard` 的行为需按此复核。

## 未覆盖

- 跨自然月重置的实际表现(见第 1 条)。
- `budget_duration` 改成其他值(如 `30d`)时 `budget_reset_at` 的算法。
- 并发下 `spend` 的最终一致窗口上界(本次只观察到约 5 秒)。
