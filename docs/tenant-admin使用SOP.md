# tenant-admin 使用 SOP

版本：2026-11-06
负责人：Wei
给谁看：网关运维
管哪些东西：网关机上的 `tenant-admin` 服务，包括租户令牌、升级、排障、备份和恢复。

新公司的完整开通流程见 `新公司开通与开户SOP.md`，词语解释也在那里。

## 它是什么

`tenant-admin` 是我们自研的小服务，跑在 LiteLLM 旁边，不改 LiteLLM。它替各公司后台持有超级 key，并逐个检查后台的请求，只放行本公司范围内的操作。

| 项目 | 位置 |
|---|---|
| 网关机 | `ec2-user@13.229.202.110`，目录 `~/gateway-litellm` |
| 容器 | `tenant-admin`，只绑定 `127.0.0.1:4100` |
| 对外地址 | `https://litellm.teenet.app/tenant-admin/`，由 nginx 转发 |
| 令牌哈希 | `~/gateway-litellm/tenant-admin/tenants.json`，权限 600，不进仓库 |
| 超级 key | `~/gateway-litellm/tenant-admin.env`，权限 600，不进仓库；LiteLLM 上的别名是 `tenant-admin-proxy-20261105` |
| 源码 | ai-env-mgr 仓库 `go/cmd/tenant-admin`、`go/internal/tenantadmin` |
| 部署配置 | gateway-litellm 仓库 `docker-compose.ragflow.yml`、`litellm-ragflow.conf` |

它没有管理网页，也没有管理接口。签发、吊销、停用都要登录网关机操作，下文命令都在 `~/gateway-litellm` 下运行。

## 1. 使用规则

- 租户令牌只交给对应公司的后台，不发到聊天或邮件里。客户自己部署后台时，用一次性加密链接。
- 超级 key 不交给任何公司的后台。
- 员工 key 由后台派生，不在 LiteLLM 管理界面手工开。
- 公司后台能做的操作只有开户、改预算和模型、发 key、关户这几类。渠道试用、健康检查和修改 team 设置被拦截，由网关运维用运维密钥直连 LiteLLM 完成。

## 2. 管理租户令牌

| 要做的事 | 命令或操作 | 结果 |
|---|---|---|
| 建 team 并签发令牌 | `docker exec tenant-admin tenant-admin token new -team wewe` | 打印 team、令牌 ID 和令牌，令牌只显示这一次 |
| 查看所有公司和令牌 ID | `docker exec tenant-admin tenant-admin token list` | 不显示令牌本身 |
| 吊销一个令牌 | `docker exec tenant-admin tenant-admin token revoke -id wewe-1` | 5 秒内生效 |

### 轮换令牌

一家公司可以同时有两个有效令牌，所以轮换不中断服务：

1. 运行 `token new -team wewe`，得到新令牌。
2. 把新令牌写进那家后台的 `AIENVMGR_GATEWAY_ADMIN_KEY`，重启后台。
3. 在后台开一个测试员工，确认正常后关户。
4. 运行 `token revoke -id <旧令牌 ID>`。

令牌泄露时步骤相同，但要立刻做，并用下面的命令查它被用来做过什么：

```bash
docker logs tenant-admin 2>&1 | grep '"token_id":"wewe-1"'
```

### 暂停一家公司的后台

1. 编辑 `tenant-admin/tenants.json`，把这家公司的 `enabled` 改成 `false`。
2. 等 5 秒，运行 `docker logs --since 1m tenant-admin`，确认有 `"event":"reloaded"`。如果看到 `reload_failed`，说明文件写错了，服务仍在用旧配置。

暂停后这家公司的后台不能开户和改预算，**但员工 key 仍然有效**。要停掉员工调用，先在后台逐个关户，再暂停。

## 3. 检查运行状态

```bash
curl -sS https://litellm.teenet.app/tenant-admin/healthz
docker ps --filter name=tenant-admin
docker logs --since 1h tenant-admin 2>&1 | grep -v '"result":"allowed"'
```

- `healthz` 返回 `{"status":"ok"}`，说明服务能用超级 key 访问 LiteLLM。它不需要令牌。
- `tenant-admin` 挂了，员工照常调模型，只是所有公司的后台不能开户和改预算。

访问日志是 JSON，一行一个请求，不含请求体和令牌：

| 字段 | 意思 |
|---|---|
| `ip`、`team`、`token_id` | 请求来源和身份。鉴权失败时没有 `team` |
| `method`、`path` | 调了哪个接口 |
| `result`、`reason` | 结果和拒绝原因 |
| `upstream_status`、`ms` | LiteLLM 的返回码和耗时 |

## 4. 常见故障

| 现象 | 原因 | 处理 |
|---|---|---|
| `healthz` 返回 503 | LiteLLM 不通，或超级 key 失效 | 先查 LiteLLM 容器，再查 `tenant-admin.env` 里的 key |
| 后台收到 401 | 令牌错误、已吊销，或公司已暂停 | 运行 `token list` 核对，必要时轮换 |
| 后台收到 403 | 请求越出本公司，或接口不在放行范围 | 看日志的 `reason`。持续出现说明后台有 bug，或令牌被滥用 |
| 后台收到 429 | 同一 IP 1 分钟内鉴权失败超过 20 次，封 10 分钟 | 先修令牌，再等封禁结束；或重启容器，封禁计数在内存里 |
| 后台收到 502 | 查 key 归属时 LiteLLM 超时或报错 | 查 LiteLLM，恢复后重试 |
| 后台设置页“组织”为空，日志有 `whoami failed` | 后台启动时连不上 `tenant-admin`，或令牌无效 | 修好后重启后台。后台此时不会拒绝启动，会沿用已保存的 team |
| 后台拒绝启动，提示 token 和 team 不一致 | 令牌属于另一家公司 | 换回本公司的令牌，不要改后台数据库 |

## 5. 升级和回滚

以下步骤需要先给新版本取标签，例如提交号 `abc1234`。

1. 在本地 ai-env-mgr 的 `go/` 目录构建镜像：

   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o cmd/tenant-admin/tenant-admin ./cmd/tenant-admin
   docker build -t tenant-admin:abc1234 cmd/tenant-admin
   ```

2. 把镜像传到网关机：

   ```bash
   docker save tenant-admin:abc1234 | ssh ec2-user@13.229.202.110 docker load
   ```

3. 把 `docker-compose.ragflow.yml` 里的镜像标签改成新版本，同步到网关机，再运行：

   ```bash
   docker compose -f docker-compose.ragflow.yml up -d --no-deps tenant-admin
   ```

4. 按第 3 节检查 `healthz`，再用一家公司的后台开一个测试员工，确认正常后关户。

**回滚：** 把标签改回旧版本，再运行第 3 步的 `docker compose` 命令。升级不改 `tenants.json`，令牌继续有效。

## 6. 轮换超级 key

各公司后台只认租户令牌，不受这项操作影响。

1. 在 LiteLLM 上签一把新的 `proxy_admin` key，别名用 `tenant-admin-proxy-<日期>`。不要用 master key。
2. 改 `tenant-admin.env`，运行 `docker compose -f docker-compose.ragflow.yml up -d --no-deps --force-recreate tenant-admin`。
3. `healthz` 返回 ok 后，在 LiteLLM 上删除旧 key。

## 7. 备份和恢复

每次签发、吊销或暂停后，备份 `tenant-admin/tenants.json` 和 `tenant-admin.env`。两个文件都放进加密存储，不进仓库。

| 丢失的文件 | 后果 | 恢复方法 |
|---|---|---|
| `tenants.json` | 服务下次重启后拒绝所有后台，员工调用不受影响 | 从备份恢复；没有备份时，给每家公司重新运行 `token new` 并换到后台。team 和员工 key 都在 LiteLLM 上，不会丢 |
| `tenant-admin.env` | 服务启动不了 | 从备份恢复，或按第 6 节签一把新 key |

## 8. 变更记录

每次签发、吊销、暂停或升级后，在 `gateway-litellm/开通记录.md` 记录：日期、操作人、公司、令牌 ID 或镜像版本、原因。不记令牌明文和超级 key。
