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
