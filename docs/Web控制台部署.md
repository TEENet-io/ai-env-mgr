# Web 控制台部署

`admin web` 是同一个 `admin` 二进制里的一个子命令。页面模板和样式都编译进了可执行文件，
**没有任何运行时依赖**：不需要 Node、不需要数据库、不需要装依赖。产物约 10 MB，静态链接。

> 先明确风险等级：控制台登录后可以写 `_agent/`，而**每台云电脑都会下载并执行那里的二进制**。
> 也就是说它等价于一个全机队远程执行控制台。下面三种方式按暴露面从小到大排列，
> 建议从能满足需求的最小那个开始。

---

## 构建

```bash
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o admin ./cmd/admin
```

Windows 换 `GOOS=windows GOARCH=amd64 -o admin.exe`。产物是单文件，直接拷贝即可。

---

## 方式一：本机运行（无需部署）

```bash
admin web
```

浏览器打开 `http://127.0.0.1:8080`。

只监听回环地址，**没有任何网络暴露**，也不需要证书。想要 Web 界面而不是命令行，这就够了。

---

## 方式二：服务器 + SSH 隧道（推荐先用这个）

控制台跑在服务器上只监听回环，你通过 SSH 隧道访问：

```bash
# 服务器上
admin web --listen 127.0.0.1:8080

# 你的电脑上
ssh -N -L 8080:127.0.0.1:8080 user@server
```

然后本地浏览器打开 `http://127.0.0.1:8080`。

这种方式的性价比最高：

- **不开任何公网端口**，扫描器根本看不到它
- **不用申请证书**，SSH 本身就是加密的
- 认证由 SSH 密钥承担，比任何 Web 登录都强
- 出差、在家都能用

---

## 方式三：公网 + HTTPS

控制台会**拒绝**在非回环地址上无证书启动：

```
refusing to serve 0.0.0.0:8080 without TLS: sign-in posts OSS credentials
that can push a binary to every machine
```

因为登录表单里提交的是能控制全机队的凭证，明文传输等于直接送人。

### 取得证书

两条路：

**A. 反向代理自动签发（省事）** —— 用 Caddy，证书自动申请和续期：

```caddyfile
admin.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

控制台仍然只监听 `127.0.0.1:8080`（不需要证书参数，因为是回环），Caddy 负责对外的 HTTPS。

**B. 自带证书** —— 阿里云免费证书或 Let's Encrypt 签好后：

```bash
admin web --listen 0.0.0.0:443 --cert /etc/ssl/admin.pem --key /etc/ssl/admin.key
```

注意绑 443 需要 root 或 `setcap`。走 A 方案就不用操心这个。

### 强烈建议再加一层

公网暴露时，前面加 **IP 白名单**或阿里云 WAF。不影响你远程办公，
但能把互联网上无差别的扫描和探测挡在门外——而这类流量占绝大多数。

Caddy 里限制来源 IP：

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

## systemd 单元

```ini
[Unit]
Description=AI Env Mgr admin console
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/ai-env-mgr/admin web --listen 127.0.0.1:8080
Restart=on-failure
RestartSec=5

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

```bash
sudo systemctl enable --now ai-env-mgr-admin
```

**重启服务会让所有人退出登录**，这是设计使然：凭证只在内存里，没有任何东西留在磁盘上
供入侵者翻找。所以 `Restart=on-failure` 而不是激进重启。

---

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:8080` | 监听地址。非回环地址必须配证书 |
| `--cert` / `--key` | 无 | TLS 证书与私钥，两者必须同时给 |
| `--idle-timeout` | `30m` | 空闲多久自动退出登录 |

会话另有 12 小时**绝对上限**，无论是否活跃。只有空闲超时的话，
一个被盗的 cookie 只要持续被使用就能永不过期。

---

## 登录

填 endpoint、bucket、AccessKey ID/Secret —— 和 `admin.config.json` 里是同一套东西。

凭证**只存在于服务进程的内存中**：不写磁盘、不进 cookie、不进日志。
cookie 里只有一个随机 ID。

建议给每个管理员建**独立的 RAM 子账号**：这样能单独吊销某个人，
而且 OSS 的访问日志天然就是一份审计记录，能看出是谁做的操作。

---

## 目前的能力范围

只读：机器列表、策略查看。

发布 agent / Codex 等写操作仍然只能用命令行 —— 先让这套安全模型经过实际检验，
再把高危操作接到公网上。

---

## 实际部署记录：windows-control.teenet.app

生产环境（47.236.115.50）的做法，可作为同类部署的样板。

该机器上 nginx 跑在 Docker 里，配置是单文件 `/root/nginx/nginx.conf`，
证书 `/root/nginx/cert` 挂载为容器内 `/cert`，其中 `teenet.app.pem` 是
CloudFlare Origin 泛域名证书（`*.teenet.app`，有效期至 2041），新子域直接复用。

### 控制台侧

```ini
ExecStart=/opt/ai-env-mgr/admin web --listen 172.23.0.1:9080 --behind-proxy
```

`172.23.0.1` 是 nginx 所在 Docker 网络（`nginx_default`）的宿主机网关地址。
**不能用 `127.0.0.1`** —— 那是容器自己的回环，nginx 到不了。
该地址是私有地址，外部无法连接，所以明文只在宿主机内部这一跳。

网关地址用这条命令查，不要猜：

```bash
docker network inspect nginx_default --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}'
```

### nginx 侧

照抄现有 server 块，`proxy_pass http://172.23.0.1:9080;` 即可。

### CloudFlare 后面必须做的一件事

DNS 记录要开橙色云朵（proxied）—— Origin 证书只被 CloudFlare 边缘信任，
不被浏览器直接信任，走灰云会报证书错误。

而一旦走了 CloudFlare，`$remote_addr` 就变成 CF 边缘节点的地址，
`X-Real-IP` 传给控制台的也是它。后果是**限流退化为“每个 CF 边缘 10 次/分钟”**
（攻击者的请求天然分散到不同边缘，等于绕过；正常用户之间反而互相挤占），
而且**审计日志记的全是 CloudFlare 的 IP，出事查不出是谁**。

所以 server 块里要加 real_ip，且只信任 CloudFlare 官方 IP 段
（取自 https://www.cloudflare.com/ips-v4 和 ips-v6）：

```nginx
set_real_ip_from 173.245.48.0/20;
# ... 其余 CloudFlare 段
real_ip_header CF-Connecting-IP;
```

只信任这些段，是为了防止有人绕过 CloudFlare 直连源站、自己伪造
`CF-Connecting-IP`。配好后控制台日志里出现的就是真实访客 IP。

CloudFlare 的 IP 段偶尔会调整，变更时需要同步更新。

### 发布私有 release 用的 GitHub Token

`admin codex publish --url` 要从私有仓库拉 release，需要一个 GitHub token。
表单里可以每次手输；留空则用服务器上的环境变量：

```ini
# /etc/ai-env-mgr/env   （chmod 600）
GITHUB_TOKEN=github_pat_xxx
```
```ini
# systemd 单元
EnvironmentFile=-/etc/ai-env-mgr/env
```

**不要把 token 编译进二进制。** 这台机器是公网可达的，而控制台之所以能说
"静态零凭证"，正是因为 OSS 的 AccessKey 只存在会话内存里；烤进去的 token
会成为这台机器上唯一一份落盘的密钥，拿到二进制就等于拿到你的私有仓库。

Token 请建成 **fine-grained、只读 Contents、只授权 codex-kiosk 一个仓库**。
这样万一泄露，代价是那个仓库被读，而不是账号被接管；轮换也只是改文件重启，
不用重新编译。

### 验证清单

```bash
docker exec nginx nginx -t          # reload 前必做：这是共享配置，改错会影响其它站点
docker exec nginx nginx -s reload
journalctl -u ai-env-mgr-admin -n 5 # 确认日志里是真实 IP，不是 CloudFlare 的
```

排错时注意区分「站点问题」和「网络问题」：拿一个**已有站点做对照组**，
两者表现一致就说明不是新部署的锅。这台机器上 IPv6 对所有站点都不通，
而直连源站 10/10 正常，据此可以判断部署本身是好的。
