# Web 控制台部署

给谁看：要部署或维护管理控制台网页版的开发和运维同事。

## 先认识几个词

| 词 | 意思 |
|---|---|
| `admin` | 管理员用的命令行程序。网页控制台是它的一个子命令 `admin web`。 |
| 机队 | 所有被管理的云电脑。 |
| `_agent/` | 云存储（OSS）里的一个目录。每台云电脑都会从这里下载程序并运行。 |
| OSS | 阿里云的对象存储，存放安装包和配置。 |
| AccessKey | 访问 OSS 的密钥，分 ID 和 Secret 两部分。 |
| RAM 子账号 | 阿里云主账号下面建的子账号，可以单独授权、单独吊销。 |
| 回环地址 | `127.0.0.1`，只有本机自己能访问的地址。 |
| SSH 隧道 | 通过 SSH 连接把服务器上的端口"搬"到你自己电脑上访问，全程加密。 |
| TLS / HTTPS | 网页加密传输。需要证书。 |
| 反向代理 | 挡在程序前面接收外部请求再转发进去的服务，例如 Caddy、nginx。 |
| systemd | Linux 上管理后台服务的工具，负责开机启动、崩溃重启。 |
| CloudFlare（CF） | 一家 CDN 服务商。网站流量先经过它的"边缘节点"再到我们的服务器。 |

## 它是什么

`admin web` 是 `admin` 程序里的一个子命令。页面模板和样式都编译进了程序文件。

运行时**不依赖任何东西**：不需要 Node，不需要数据库，不需要装依赖。程序约 10 MB，是静态链接的单个文件。

> **先明确风险。** 登录控制台后可以往 `_agent/` 写文件，而**每台云电脑都会下载并运行那里的程序**。
> 所以控制台等于一个能在所有云电脑上远程执行程序的入口。
> 下面三种部署方式按暴露范围从小到大排列。建议选能满足需求的最小那种。

---

## 构建

```bash
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o admin ./cmd/admin
```

要构建 Windows 版，把参数换成 `GOOS=windows GOARCH=amd64 -o admin.exe`。产物是单个文件，直接拷贝过去就能用。

---

## 方式一：在自己电脑上运行（不用部署）

```bash
admin web
```

浏览器打开 `http://127.0.0.1:8080`。

它只监听回环地址，**外部网络完全访问不到**，也不需要证书。如果只是想用网页界面代替命令行，这种方式就够了。

---

## 方式二：跑在服务器上，通过 SSH 隧道访问（推荐先用这个）

控制台在服务器上只监听回环地址，你通过 SSH 隧道访问：

```bash
# 服务器上
admin web --listen 127.0.0.1:8080

# 你的电脑上
ssh -N -L 8080:127.0.0.1:8080 user@server
```

然后在自己电脑的浏览器打开 `http://127.0.0.1:8080`。

这种方式投入最少、收益最大：

- **不开任何公网端口**，网上的扫描程序根本发现不了它。
- **不用申请证书**，SSH 本身就是加密的。
- 身份验证靠 SSH 密钥，比任何网页登录都可靠。
- 出差、在家都能用。

---

## 方式三：直接放到公网，用 HTTPS 访问

如果监听的不是回环地址，又没配证书，控制台会**拒绝启动**，并报：

```
refusing to serve 0.0.0.0:8080 without TLS: sign-in posts OSS credentials
that can push a binary to every machine
```

意思是：没有加密就不在 `0.0.0.0:8080` 上提供服务，因为登录时提交的 OSS 凭证能往所有机器推送程序。明文传输这种凭证，等于直接送给别人。

### 取得证书

有两种办法。

**A. 用反向代理自动申请（省事）**：用 Caddy，它会自动申请和续期证书。

```caddyfile
admin.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

控制台仍然只监听 `127.0.0.1:8080`。因为是回环地址，控制台本身不需要证书参数。对外的 HTTPS 由 Caddy 负责。

**B. 自己准备证书**：用阿里云免费证书或 Let's Encrypt 签好证书后运行：

```bash
admin web --listen 0.0.0.0:443 --cert /etc/ssl/admin.pem --key /etc/ssl/admin.key
```

注意：监听 443 端口需要 root 权限，或者用 `setcap` 给程序授权。用 A 方案就没有这个问题。

### 强烈建议再加一层防护

放到公网时，在前面加 **IP 白名单**或阿里云 WAF（网页防火墙）。这不影响你远程办公，但能挡住互联网上漫无目的的扫描和探测，而这类流量占了绝大多数。

在 Caddy 里限制来源 IP 的写法：

```caddyfile
admin.example.com {
    @allowed remote_ip 203.0.113.0/24 198.51.100.7
    handle @allowed {
        reverse_proxy 127.0.0.1:8080
    }
    respond 403
}
```

---

## 用 systemd 管理服务

```ini
[Unit]
Description=AI Env Mgr admin console
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/ai-env-mgr/admin web --listen 127.0.0.1:8080
Restart=on-failure
RestartSec=5

# 发布 Codex 要搬约 700 MB，而策略是在最后一刻才写的。进程中途退出会
# 把整个发布毁掉，且不留任何痕迹：策略没变、没有审计日志、任务页面因为
# 状态只在内存里而变回空白——管理员会以为发布成功了，其实机队什么都没收到。
# 所以控制台收到 SIGTERM 后会先把在跑的发布做完（最多等 20 分钟）再退出。
# 这个值必须大于 20 分钟，否则 systemd 先发 SIGKILL，等待就白等了。
TimeoutStopSec=25min

# 控制台不持有任何静态凭证，也不写文件，所以可以收得很紧。
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
MemoryDenyWriteExecute=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

配置里两处要点的说明：

- `TimeoutStopSec=25min`：发布 Codex 要传约 700 MB 文件，策略在最后一步才写入。如果进程中途退出，这次发布就白做了，而且看不出来：策略没变，没有审计日志，任务页面也变回空白（任务状态只存在内存里）。管理员会以为发布成功了，其实所有机器什么都没收到。所以控制台收到停止信号 `SIGTERM` 后，会先把正在进行的发布做完（最多等 20 分钟）再退出。这个超时必须大于 20 分钟，否则 systemd 会先发 `SIGKILL` 强制结束进程，前面的等待就没用了。
- 下面那串 `Protect...` 等安全限制：控制台不保存任何固定密钥，也不写文件，所以可以把权限收得很紧。

启用并启动服务：

```bash
sudo systemctl enable --now ai-env-mgr-admin
```

**重启服务会让所有人退出登录。** 这是故意这样设计的：登录凭证只存在内存里，磁盘上什么都不留，入侵者也就翻不到。也因为这个原因，配置用的是 `Restart=on-failure`（只在出错时重启），而不是更频繁的重启策略。

---

## 启动参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:8080` | 监听地址。非回环地址必须配证书 |
| `--cert` / `--key` | 无 | TLS 证书与私钥，两者必须同时给 |
| `--idle-timeout` | `30m` | 空闲多久自动退出登录 |

另外，每次登录最长有效 12 小时，到时间一定退出，不管是否在用。原因是：如果只有空闲超时，被盗的登录 cookie 只要一直有人在用，就永远不会过期。

---

## 登录

登录时填 endpoint（OSS 地址）、bucket（存储桶名）、AccessKey ID 和 Secret。这和 `admin.config.json` 里填的是同一套信息。

凭证**只保存在服务进程的内存里**：不写磁盘，不放进 cookie，不写进日志。cookie 里只有一个随机 ID。

建议给每个管理员建一个**独立的 RAM 子账号**。好处有两个：

- 可以单独吊销某一个人的权限。
- OSS 的访问日志本身就能当审计记录，看得出是谁做的操作。

---

## 目前能做什么

只能查看：机器列表、策略。

发布 agent、发布 Codex 等写操作，目前仍然只能用命令行。先让这套安全设计经过实际使用检验，再把高风险操作放到公网上。

---

## 实际部署记录：windows-control.teenet.app

这是生产环境（47.236.115.50）的做法，同类部署可以照着做。

这台机器的情况：

- nginx 跑在 Docker 容器里，配置是一个文件：`/root/nginx/nginx.conf`。
- 证书目录 `/root/nginx/cert` 挂载到容器内的 `/cert`。
- 其中 `teenet.app.pem` 是 CloudFlare Origin 泛域名证书（覆盖 `*.teenet.app`，有效期到 2041 年）。新加的子域名直接用它。

### 控制台这边

```ini
ExecStart=/opt/ai-env-mgr/admin web --listen 172.23.0.1:9080 --behind-proxy
```

`172.23.0.1` 是 nginx 所在 Docker 网络（`nginx_default`）在宿主机上的网关地址。

**不能用 `127.0.0.1`**：在容器里，`127.0.0.1` 指的是容器自己，nginx 访问不到宿主机上的控制台。

`172.23.0.1` 是私有地址，外部连不上。所以未加密的流量只出现在宿主机内部这一段。

网关地址要用下面这条命令查，不要猜：

```bash
docker network inspect nginx_default --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}'
```

### nginx 这边

照抄现有的 server 配置块，把转发地址写成 `proxy_pass http://172.23.0.1:9080;` 就行。

### 用了 CloudFlare 必须额外做的事

**1. DNS 记录要打开橙色云朵（proxied，即流量经过 CloudFlare）。** Origin 证书只有 CloudFlare 认，浏览器不认。如果是灰色云朵（不经过 CloudFlare），浏览器会报证书错误。

**2. 要让控制台拿到访客的真实 IP。** 流量经过 CloudFlare 后，nginx 看到的来源地址 `$remote_addr` 变成了 CloudFlare 边缘节点的地址，通过 `X-Real-IP` 传给控制台的也是它。后果有两个：

- **限流失效。** 本来是"每个访客每分钟 10 次"，变成了"每个 CF 边缘节点每分钟 10 次"。攻击者的请求本来就分散在不同边缘节点上，等于绕过了限流；正常用户之间反而会互相抢额度。
- **审计日志里记的全是 CloudFlare 的 IP**，出了事查不出是谁。

所以要在 server 配置块里加 real_ip 设置，并且只信任 CloudFlare 官方公布的 IP 段（来自 https://www.cloudflare.com/ips-v4 和 ips-v6）：

```nginx
set_real_ip_from 173.245.48.0/20;
# ... 其余 CloudFlare 段
real_ip_header CF-Connecting-IP;
```

为什么只信任这些 IP 段：防止有人绕过 CloudFlare 直接连我们的服务器，自己伪造 `CF-Connecting-IP` 请求头冒充别的 IP。配好后，控制台日志里就是访客的真实 IP。

CloudFlare 的 IP 段偶尔会调整，调整时这里要跟着更新。

### 发布私有仓库的 release 需要 GitHub Token

`codex publish --url` 要从私有仓库下载 release（发布包），需要一个 GitHub token（访问令牌）。

**token 由管理员每次发布时在发布页的输入框里现场填写。** 服务器上不保存，也不从环境变量读取。

这样做是有意的：

- 这台机器在公网上能被访问到。
- 控制台敢说"不保存任何固定密钥"，是因为 OSS 的 AccessKey 只存在登录会话的内存里。
- 如果 token 存在这台机器上（不管是编译进程序还是放进环境变量），它就成了机器上唯一一份固定密钥，谁拿到谁就能读私有仓库。
- 而且"token 由管理员输入"这个前提，会在没人察觉的情况下不再成立。

建 token 时请这样设置：选 **fine-grained 类型，只给 Contents 只读权限，只授权 codex-kiosk 这一个仓库**，并设置有效期。这样即使泄露，损失也只是这个仓库被人读到，而不是整个账号被接管。

（`admin` 命令行仍然支持用 `--token` 参数或 `GITHUB_TOKEN` 环境变量传 token。因为命令行跑在管理员自己的电脑上，风险情况不一样。）

### 部署后检查

```bash
docker exec nginx nginx -t          # reload 前必做：这是共享配置，改错会影响其它站点
docker exec nginx nginx -s reload
journalctl -u ai-env-mgr-admin -n 5 # 确认日志里是真实 IP，不是 CloudFlare 的
```

三条命令依次是：

1. 检查 nginx 配置有没有写错。重新加载前必须做，因为这份配置是多个站点共用的，改错会影响其他站点。
2. 重新加载 nginx 配置。
3. 看控制台最近 5 行日志，确认记录的是访客真实 IP，不是 CloudFlare 的 IP。

排查问题时，先分清是"这个站点的问题"还是"网络的问题"。办法是拿一个**已有的站点做对照**：两者表现一样，就说明不是新部署的问题。例如这台机器上，所有站点的 IPv6 都不通，而直连服务器测试 10 次全部正常，由此可以判断部署本身没问题。
