# Bippo 正式环境

2026-11-19 上线。数据全新，不从旧控制台（windows-control.teenet.app）迁移。

## 一览

| 项 | 值 |
|---|---|
| 控制台地址 | https://bippo-workspace.teenet.app （Cloudflare 橙云） |
| 机器 | AWS 新加坡 `ec2-user@175.41.186.104`，和樂山（bippo.teenet.app，旧名 bippo-demo.teenet.app）同机 |
| 身份 | 樂山单点登录（OIDC，client_id `bippo-workspace`），名单每 5 分钟从樂山同步 |
| 网关 | `https://litellm.teenet.app`，经 tenant-admin，公司 `bippo_prod`，令牌 `bippo_prod-1` |
| 存储 | 阿里云 OSS `bippo-workspace-sg`（新加坡，私有） |
| 用量 | 从网关的每日统计取（`/team/daily/activity`），不用 SLS |
| Codex | 和老环境共用 `ai-collect-sg/agent_workdir/_codex/`（`AIENVMGR_CODEX_BUCKET`），codex-kiosk 照旧只传一份；`bippo-workspace-console` 对那个前缀只读 |
| Agent | CI 构建 `agent-bippo.exe`（控制台地址编译进去），传到 `bippo-workspace-sg/agent_workdir/_agent/<版本>/agent.exe` |

## 机器上的东西

| 路径 / 名字 | 说明 |
|---|---|
| `/etc/ai-env-mgr/console.env` | 控制台全部配置和密钥，root 600。改完 `systemctl restart ai-env-mgr-console` |
| `/etc/ai-env-mgr/master.key` | 主密钥，root 600。**丢了库里的令牌和验证器种子都解不开**，要另外备份 |
| `/etc/ai-env-mgr/migrate.env` | 表属主（postgres）的连接串，只给迁移用 |
| `ai-env-mgr-console.service` | 控制台，监听 docker0 `172.17.0.1:8090`（`--behind-proxy`），健康检查 `/healthz` |
| Docker `aienv-pg15` | postgres:15-alpine，只监听 `127.0.0.1:5433`，数据卷 `aienv-pgdata`，限 512 MB |
| `/home/ec2-user/edge-nginx/conf.d/bippo-workspace.conf` | 站点配置。改完 `docker exec edge-nginx nginx -t && docker exec edge-nginx nginx -s reload` |
| `ai-env-backup.timer` | 每天 UTC 2:00 导出数据库，传到 `bippo-workspace-sg/agent_workdir/_backup/`，留 30 天 |
| `/var/log/ai-env-mgr/` | 控制台的统一日志（admin.jsonl、audit.jsonl） |

樂山的站点（`knowledge-databases.conf`，含 bippo.teenet.app、bippo-demo.teenet.app）是 nginx 的 `default_server`。**新加站点时不要依赖默认站点**：2026-11-19 加 bippo-workspace.conf 后它按文件名排到最前，成了默认站点，bippo.teenet.app 有约半小时进了控制台。

旧的 `windows-control.conf`（指向早已不存在的 :9080）挪到了 `edge-nginx/disabled/`。

## 樂山一侧

`/home/ec2-user/gateway/config.env` 末尾加了：`OIDC_ISSUER=https://bippo.teenet.app`、`OIDC_CLIENT_ID=bippo-workspace`、`OIDC_CLIENT_SECRET`、`OIDC_REDIRECT_URIS`、`OIDC_POST_LOGOUT_REDIRECT_URIS`、`CONSOLE_URL`、`DIRECTORY_TOKEN`。客户端密钥和名单令牌两边各存一份，换的时候两边一起换，再重启 `renewal-gateway` 和 `ai-env-mgr-console`。

## 登录

- 运维：`/ops/login` →「使用樂山帳號登入」。第一次登录会进「待開通」，由已有管理员在「設定 → 管理員」里建立并绑定。
- 客户：樂山里的「聯絡 TEENet」/「客戶控制台」直接带单点登录进工单中心。樂山 admin = 客户管理员（viewer，可办理全部申请），employee = 普通用户。
- 应急账号：`AIENVMGR_EMERGENCY_ADMIN` 写的是**控制台用户名**（首次启动自动建的本地管理员），不是邮箱。只在樂山不可用时用，每次登录都会告警。

## 升级控制台

在本机编译后传上去（这台机器上没有代码目录）：

```sh
cd go && V=$(git describe --tags --always)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$V" -o /tmp/ai-env-admin ./cmd/admin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/ai-env-migrate ./cmd/migrate
scp /tmp/ai-env-admin /tmp/ai-env-migrate ec2-user@175.41.186.104:/tmp/
```

机器上（root）：

1. 有新迁移时先备份：`docker exec aienv-pg15 pg_dump -U postgres --no-owner aienv | gzip > /var/backups/ai-env-mgr/aienv-predeploy-$(date -u +%Y%m%dT%H%M%SZ).sql.gz`
2. 用新的 `ai-env-migrate` 以属主身份迁移：`set -a; . /etc/ai-env-mgr/migrate.env; set +a; /tmp/ai-env-migrate up`
3. 旧程序留作 `.prev`，装上新程序，`systemctl restart ai-env-mgr-console`，确认 `curl http://172.17.0.1:8090/healthz` 返回 `ok`。不通就换回 `.prev`。

## 用老镜像的云电脑换成 Bippo Agent

老镜像里的服务 `AIEnvMgrAgent` 启动的是 `C:\Tools\ai-env-mgr.exe`，不是 `setup` 安装的 `C:\Program Files\AIEnvMgr\agent.exe`。

**已知 bug（还没修）**：`agent.exe setup` 看到服务「已注册」就跳过，不检查服务指向哪个文件，结果只换了一个没人用的文件。症状：
- `C:\Program Files\AIEnvMgr\agent.exe status` 显示的是新控制台；
- 但日志里一直在和老控制台同步，新控制台一个请求也收不到；
- `sc.exe qc AIEnvMgrAgent` 的 `BINARY_PATH_NAME` 是 `C:\Tools\ai-env-mgr.exe run`。

老控制台推的 Agent 升级也会替换 `C:\Tools` 下那个文件（2026-11-19 换包时正好撞上一次）。

修好之前，在模板机上按这个顺序做（管理员 PowerShell）：

```powershell
# 先等几分钟，让老控制台的升级和 Codex 安装跑完
cd C:\Users\<你>\Downloads            # agent-bippo.exe 放在这里
.\agent-bippo.exe setup                  # 复制到 C:\Program Files\AIEnvMgr\agent.exe
& 'C:\Program Files\AIEnvMgr\agent.exe' uninstall
Rename-Item C:\Tools\ai-env-mgr.exe ai-env-mgr.exe.old-console
& 'C:\Program Files\AIEnvMgr\agent.exe' setup
sc.exe qc AIEnvMgrAgent                  # 应为 "C:\Program Files\AIEnvMgr\agent.exe" run
```

`setup` 第 5 步报 `the console does not accept this machine's token` 是正常的（带的是老控制台的令牌），服务会自己重新注册，约一分钟后在后台出现。老控制台里会留一条这台机器的记录，删掉即可。

修复已写好，在分支 `fix/setup-repoint-service`（提交 `0d0eeb2`，未推送）：`setup` 读服务的实际路径，不是自己就把服务改指向 `C:\Program Files\AIEnvMgr\agent.exe`。合并发版后，上面的步骤就只剩 `setup` → `prepare-image` → 关机。

## 还没做的

- 飞书告警 webhook：在设置页的通知渠道里配置。
- `ai-collect-sg` 的 Bucket 授权策略：首次备份误传时发现，`bippo-workspace-console` 这对密钥能往那个 bucket 写，要收紧。
- `setup` 不检查服务实际路径的 bug（见上一节），修复在 `fix/setup-repoint-service`，待合并发版。
