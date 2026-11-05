# 网关租户管理代理（tenant-admin）设计

2026-11-05 · 状态：已确认，实施中

## 1. 要解决的问题

多个公司共用一个 LiteLLM 网关（`litellm.teenet.app`），按 LiteLLM 的 team 区分公司。每个公司有一套自己的后台，后台开户、开密钥、改预算都要调网关的管理接口。

开源版 LiteLLM 的管理权限只有全局角色。后台用的管理员密钥（proxy_admin）能读写网关上所有公司的数据。只限本团队的管理员角色在企业版里，开源版的插件机制也没有干净的接入点。详见 `docs/网关管理密钥权限问题_2026-11-05.md`（在 windows-pc 目录）。

目标：一个公司的后台，或者它持有的凭证，只能操作本公司 team 下的用户和密钥。LiteLLM 本身不改。

## 2. 方案概览

在网关机上加一个小服务 `tenant-admin`，挂在 `litellm.teenet.app/tenant-admin/` 下：

```
公司后台 ──Bearer 租户令牌──► Cloudflare ─► nginx ─┬─ /tenant-admin/* ─► tenant-admin ──管理员密钥──► LiteLLM
                                                    └─ 其余路径 ──────────────────────────────────────► LiteLLM
员工 Codex ──员工令牌──────► Cloudflare ─► nginx ──────────────────────────────────────────────────────► LiteLLM
```

- 只有 tenant-admin 持有网关管理员密钥。公司后台拿的是**租户令牌**，每个令牌在 tenant-admin 上绑定一个 team。
- tenant-admin 只放行后台用到的接口（第 4 节）。每个请求都检查只涉及本 team，然后用管理员密钥转发给 LiteLLM。
- 员工调模型不经过 tenant-admin。它挂了，员工照常使用，只是后台暂时不能开户、改预算。
- 我们自己的运维操作（渠道试用、健康检查、看全网关数据）继续用运维密钥直连 LiteLLM，不经过 tenant-admin。

## 3. tenant-admin 自身的鉴权

tenant-admin 有两类密钥：公司后台拿来访问它的**租户令牌**，和它自己访问 LiteLLM 的**管理员密钥**。前者决定"你是哪家"，第 4 节的规则决定"这家能做什么"。

### 3.1 租户令牌

- **格式**：`ta-` 加 32 字节随机数（base64url），由我们生成。明文只交给对应公司的后台，存在后台的环境变量 `AIENVMGR_GATEWAY_ADMIN_KEY` 里。
- **存储**：配置文件 `tenants.json` 只在网关机上，权限 600，不进仓库。文件里只存哈希，不存明文：

  ```json
  {"tenants": [
    {"team": "bippo", "tokens": [{"id": "bippo-1", "sha256": "<令牌的 SHA-256>"}], "enabled": true},
    {"team": "wewe",  "tokens": [{"id": "wewe-1",  "sha256": "<令牌的 SHA-256>"}], "enabled": true}
  ]}
  ```

  令牌是 32 字节的高熵随机数，用 SHA-256 存储就够了，不需要 bcrypt 这类慢哈希。
- **一家公司可以同时有两个有效令牌**，用于轮换：先加新令牌，后台换上新令牌并重启，确认正常后再删旧令牌，中间不中断服务。
- **生成令牌**：用命令行 `tenant-admin token new --team wewe`。它做三件事：team 在 LiteLLM 上不存在就先建好；把令牌哈希写进 `tenants.json`；打印令牌明文。明文只显示这一次。流程见第 4 节之后的"开通一家新公司"。
- **吊销**：删掉那一行令牌，或者把整个公司设成 `enabled: false`，然后发 SIGHUP 让服务重读配置，不用重启。
- **管理操作只能在网关机本机做**：tenant-admin 没有任何管理用的 HTTP 接口。建 team、发令牌、吊销令牌、停用公司，都要 SSH 登录网关机，用命令行或改配置文件完成。所以公网上能做的事，最多就是第 4 节那张表；建 team 也不在其中。
- **命令行怎么跑**：在网关机上执行 `docker exec tenant-admin tenant-admin token new ...`。`tenants.json` 挂载进容器，命令行改完后自动通知服务重读配置。
- **谁能登录网关机**：SSH 只认我们运维的密钥。AWS 安全组只对运维的出口 IP 开放 22 端口（上线前核对一遍现状）。

### 3.2 每个请求怎么鉴权

1. 只从 `Authorization: Bearer <令牌>` 读令牌。令牌放在 URL 参数或其他头里，一律不认。
2. 计算令牌的 SHA-256，和配置里所有有效令牌逐个做常数时间比较，找出对应的 team。
3. **没带令牌、令牌不对、或者公司已停用：返回 401**，响应里不说明是哪种情况，然后结束。鉴权通过、但请求越出本 team：返回 403（见第 5 节）。
4. **转发给 LiteLLM 时重新构造请求**，不透传客户端的鉴权头：
   - 去掉 `Authorization`、`x-litellm-api-key`、`api-key`、`x-api-key`、`Cookie` 等所有可能被 LiteLLM 当成凭证的头；
   - 只加上 tenant-admin 自己的管理员密钥。

   不这样做的话，后台可以自己塞一把别的密钥进 `x-litellm-api-key`。LiteLLM 会优先认这个头，绕开 tenant-admin 的检查。
5. **限制试错**：同一个来源 IP，1 分钟内鉴权失败超过 20 次，该 IP 在接下来 10 分钟内全部返回 429。IP 取 nginx 传来的 `X-Real-IP`；nginx 已经按 Cloudflare 的 `CF-Connecting-IP` 还原了真实地址。令牌有 256 位随机数，这个限制主要是为了减少日志噪音和扫描。
6. **传输**：外网到 Cloudflare、Cloudflare 到 nginx 都是 HTTPS。nginx 到 tenant-admin 只走本机回环地址（`127.0.0.1:4100`），不对外开放端口。

### 3.3 管理员密钥

- 给 tenant-admin 单独签一把 proxy_admin 虚拟密钥，例如 `tenant-admin-proxy-20261105`。**不用 LiteLLM 的 master key**，这样它可以单独吊销和轮换，也不影响我们运维用的密钥。
- 管理员密钥通过环境变量注入，放在网关机的 `.env` 文件里（权限 600），不进仓库，也不写进日志。
- 轮换：签一把新的，改 `.env`，重建 tenant-admin 容器，确认正常后在 LiteLLM 上删掉旧的。各公司的后台不受影响，它们只认租户令牌。

## 4. 放行的接口和检查规则

下表列出后台实际调用的全部管理接口（取自 `go/internal/litellm`）。**表里没有的路径一律返回 403。**

"本 team 的用户"指用户 ID 以 `<team>-emp-` 开头。team ID 只允许小写字母、数字和下划线，不含连字符，所以两家公司的前缀不会重叠；`/user/new` 又只允许建本 team 前缀的用户，别家建不出带我们前缀的用户。不按 team 成员关系判断，是因为后台先建用户、再加入 team，加入失败后重试时用户还不在 team 里。没有前缀的旧用户见第 6 节第 2 条。

| 接口 | 用途 | 检查 |
|---|---|---|
| `GET /whoami` | 后台启动时确认自己是哪个 team | 不转发 LiteLLM，直接返回 `{"team": "<本 team>"}` |
| `GET /team/info?team_id=` | 确认 team 存在 | `team_id` 必须是本 team；响应只返回 `team_id`，不带成员和设置 |
| `POST /team/member_add` | 员工加入 team | `team_id` 是本 team；`member.role` 必须是 `user`；`member.user_id` 是本 team 的用户 |
| `POST /user/new` | 建员工用户 | `user_id` 必须带 `<team>-emp-` 前缀；`user_role` 必须是 `internal_user`；请求体字段白名单（见下文） |
| `POST /user/update` | 改预算、模型、部门 | `user_id` 是本 team 的用户；字段白名单同上，但**不允许** `user_role` |
| `GET /user/info?user_id=` | 查用户 | `user_id` 是本 team 的用户 |
| `POST /user/delete` | 删员工用户 | `user_ids` 里每个都是本 team 的用户 |
| `GET /user/list` | 列用户 | 强制把 `team` 参数设为本 team；响应里去掉不带本公司前缀的用户。LiteLLM 建 team 时会把建 team 的网关账号加成成员，这个账号不该让租户看到 |
| `POST /key/generate` | 开员工令牌 | `team_id` 是本 team；`user_id` 是本 team 的用户；`key_alias` 带 `<team>-emp-` 前缀；字段白名单 |
| `POST /key/update` | 改令牌的模型 | 请求体只允许 `key`、`models`；先用 `/key/info?key=` 查出这把令牌，`team_id` 必须是本 team |
| `POST /key/delete` | 吊销令牌 | `keys` 逐个用 `/key/info` 查归属；`key_aliases` 逐个用 `/key/list?key_alias=&team_id=本team` 查，必须在本 team |
| `GET /key/list` | 列令牌、按别名找令牌 | 强制 `team_id=本team`；禁止 `substring_matching`、`include_team_keys`、`include_created_by_keys`、`organization_id`、`user_id` |
| `GET /model/info` | 模型目录 | 只读放行，所有公司都一样 |

字段白名单：

- `/user/new`、`/user/update`：`user_id`、`user_alias`、`max_budget`、`budget_duration`、`rpm_limit`、`tpm_limit`、`max_parallel_requests`、`metadata`、`models`；`/user/new` 另加 `user_role`（只能是 `internal_user`）和 `auto_create_key`（只能是 `false`）。
- `/key/generate`：`key_alias`、`user_id`、`models`、`team_id`、`metadata`。

出现白名单以外的字段，整个请求 403。白名单堵住的是这类绕过：建用户时带 `user_role: proxy_admin`、`teams: [别家]`；开密钥时带 `allowed_routes`、`key_type`、`permissions`。

后台的「渠道」页用到的 `/health` 和 `/chat/completions` **不放行**：渠道试用和健康检查是网关级运维操作，公司后台不该有。

### 开通一家新公司

以 wewe 为例。全部在我们这边完成，客户不需要申请，也拿不到自助入口。

1. **定 team ID**：比如 `wewe`。规则是小写字母、数字、下划线，2–32 位，定了以后不能改。
2. **签发**：SSH 登录网关机，运行 `docker exec tenant-admin tenant-admin token new --team wewe`。它在 LiteLLM 上建好 team，登记令牌，打印令牌明文。
3. **交付令牌**：
   - 后台由我们运维（默认）：直接写进 wewe 后台服务器的环境变量 `AIENVMGR_GATEWAY_ADMIN_KEY`，不经过聊天、邮件。
   - 客户自己部署后台：用一次性加密分享链接交给对方技术负责人。
4. **配置后台**：`AIENVMGR_GATEWAY_URL=https://litellm.teenet.app`，`AIENVMGR_GATEWAY_ADMIN_URL=https://litellm.teenet.app/tenant-admin`，`AIENVMGR_GATEWAY_ADMIN_KEY=<令牌>`。后台启动时调 `/whoami` 得到 team ID，不用在设置页手填，也就不会填错。
5. **验证**：开一个测试员工，确认能开令牌、能改模型、能关户，然后删掉。
6. **登记**：在 gateway-litellm 仓库的开通记录里加一行：公司、team ID、后台地址、令牌 id、签发日期、负责人。不记令牌明文。

## 5. 错误语义

后台把某些接口的 404 当成"已经不存在"，比如删除时收到 404 就算成功，查用户收到 404 就去新建。所以：

- **拒绝一律返回 403，绝不返回 404。** 否则后台会把"没权限删"当成"删掉了"。
- 上游 LiteLLM 的状态码和响应体原样转回，包括 404。这样后台按现有逻辑工作。
- 403 的响应体格式和 LiteLLM 一致：`{"error": {"message": "...", "type": "tenant_forbidden"}}`。message 写明哪条规则不满足，方便排查。
- 需要先查归属的接口，查询本身失败时（LiteLLM 超时、5xx）返回 502，不放行。

## 6. 后台要改什么，以及几个边界情况

1. **两个网关地址分开配置。** 现在后台只有一个网关地址，既写进员工的 Codex 配置，也用来做管理操作。改成：
   - 员工地址：`AIENVMGR_GATEWAY_URL`，不变，还是 `https://litellm.teenet.app`。
   - 管理地址：新增 `AIENVMGR_GATEWAY_ADMIN_URL`，填 `https://litellm.teenet.app/tenant-admin`；`AIENVMGR_GATEWAY_ADMIN_KEY` 换成租户令牌。不填时退回员工地址，现有部署不受影响。
   - 配了管理地址时，后台启动时调 `/whoami` 取得 team ID，设置页的「网关客户（team）」改成只读显示。取不到就拒绝启动，以免用错 team 开户。不走代理的部署照旧用设置页里填的值。
   - 「渠道」页的试用和健康检查在走代理时会收到 403。页面要显示"由网关运维执行"，不能当成网关故障。
2. **没有 team 前缀的旧用户。** bippo 迁移前的 `emp-peter` 等旧用户不在 team 里，代理不会放行对它们的操作。影响只有一处：删除员工账号时，后台想顺带删旧用户，会被 403。处理办法：旧用户由我们运维统一清理。要注意，这类删除任务在后台会一直重试，所以删除时要跳过已知的旧用户。目前删除任务实际没有注册（这是已有的 bug），等它修好时一并处理。
3. **团队预算、团队设置**：代理不放行 `/team/update`，公司后台不能改 team 本身的设置。
4. **查归属和执行之间的竞争**：先查归属再执行，中间这段时间里令牌换了 team 的可能性，只存在于我们运维手工改动时，可以接受。
5. **`/model/info` 会暴露各部署的上游信息**（`api_base`、渠道）。LiteLLM 不返回上游密钥，但会让客户看到我们用的供应商。是否要裁剪字段，见第 10 节第 2 条。

## 7. 部署

- **代码**：放在 ai-env-mgr 仓库，`go/cmd/tenant-admin`。检查规则单独成包，配表驱动测试。
- **镜像**：静态 Go 二进制，用 distroless 镜像，大约 10MB；常驻内存估计 20MB 左右，容器上限设 64MB。
- **compose**：在 gateway-litellm 仓库的 `docker-compose.ragflow.yml` 里加一个 `tenant-admin` 服务，和 litellm 在同一个 Docker 网络，上游用 `http://litellm:4000`，只绑定 `127.0.0.1:4100`。
- **nginx**：在 `litellm-ragflow.conf`（线上是 `/etc/nginx/conf.d/litellm.conf`）里加：

  ```nginx
  location /tenant-admin/ {
      proxy_pass http://127.0.0.1:4100/;   # 末尾的 / 会去掉 /tenant-admin 前缀
      proxy_set_header X-Real-IP $remote_addr;
      proxy_read_timeout 60s;
      client_max_body_size 1m;
  }
  ```

- **来源 IP 限制（可选）**：nginx 只允许已知后台服务器的出口 IP 访问 `/tenant-admin/`。令牌泄露后，从别处也用不了。

## 8. 审计和监控

- 每个请求写一行 JSON 日志：时间、来源 IP、team 和令牌 id（鉴权失败时为空）、方法、路径、结果（放行、401、403 或 429）、拒绝原因、上游状态码、耗时。**不记录请求体和令牌本身。** 日志写到 stdout，用 `docker logs` 查看；以后需要时接入 SLS。
- `GET /tenant-admin/healthz`：不需要令牌，用管理员密钥请求 `/key/list?size=1`，检查能否访问 LiteLLM。后台的网关健康探测改成探这个地址。
- 被拒绝的请求说明后台有 bug，或者凭证被人滥用。先在日志里能查到就行，后续再考虑告警。

## 9. 测试和上线

1. **单元测试**：
   - **鉴权**：没带令牌、令牌错误、公司已停用都返回 401；轮换期间新旧两个令牌都能用；客户端塞进来的 `x-litellm-api-key` 等头不会转发给 LiteLLM。
   - **接口规则**：第 4 节每个接口至少覆盖三类情况：本 team 放行；别家 team 被拒；参数被篡改被拒，比如 `user_role` 改成 proxy_admin、带 `teams`、带 `substring_matching`、`/key/delete` 传别家的别名。
2. **集成测试**：本地用线上同一个 LiteLLM 镜像（1.98.0），起 LiteLLM、PostgreSQL、tenant-admin，建两个 team，用后台走一遍开户、改模型、关户、重新开户，再用 A 公司的令牌去碰 B 公司，确认全部被拒。
3. **上线**：
   - 部署 tenant-admin，加 nginx 规则；
   - 给 bippo 生成租户令牌；
   - bippo 后台改用管理地址加租户令牌，重启；
   - 在后台开一个测试员工、改模型、关户，确认正常；
   - 现有的管理员密钥 `windows-control-console-20260917` 从 bippo 后台撤下，只留给运维。
4. **回滚**：后台的两个环境变量改回原值，重启即可。tenant-admin 留着不影响任何东西。

工作量估计：代码和单元测试 1 天，集成测试和后台改造半天，上线半天。

## 10. 已确认的决定（2026-11-05）

1. **现在就做，bippo 也切过去。** 上线后 bippo 后台改走 tenant-admin，现有管理员密钥 `windows-control-console-20260917` 只留给运维。
2. **`/model/info` 隐藏上游信息。** tenant-admin 只返回后台用到的字段（模型名、展示信息），并算好渠道名填进 `model_info.channel`，去掉 `litellm_params` 里的 `api_base` 等上游细节。后台按渠道分组的逻辑本来就优先认 `model_info.channel`，所以不受影响。
3. **不加来源 IP 限制。** 第 7 节的可选项不做。
4. **旧用户 `emp-*` 先不动。** 删除员工账号时，删旧用户的那一步会被 403，这个情况维持现状（删除任务目前本来也没注册）。
