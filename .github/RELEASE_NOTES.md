自动构建的二进制。

## 产物

| 文件 | 平台 | 凭据 |
|---|---|---|
| `agent.exe` | windows/amd64 | **已注入受限密钥**（来自仓库 Secrets），`config=built-in`,可直接进镜像 |
| `admin-linux-amd64` | linux/amd64 | **无密钥**,运行时交互输入 |
| `admin-windows-amd64.exe` | windows/amd64 | 同上 |
| `admin-darwin-arm64` | macOS (Apple Silicon) | 同上 |

## admin 怎么用凭据

`admin` **不含任何密钥**。在终端里直接运行(如 `admin status`),没有内置凭据、也没有 `admin.config.json` 时,它会**提示你输入** bucket / endpoint / AccessKeyId / AccessKeySecret(Secret 输入**不回显**),然后**当场向 OSS 验证**:

- 密钥错(`InvalidAccessKeyId`/`SignatureDoesNotMatch`)→ 提示重输;
- 密钥对但权限不足(`AccessDenied`)→ 提示去补 RAM 授权;
- 通过 → 继续执行命令。

**输入的凭据不写磁盘**,每次运行都要重新输入(这是刻意的:管理员那把是**全桶读写**密钥,不落盘最稳妥)。非终端环境(管道/CI)不会卡输入,而是直接报"未配置凭据"。

## 安全说明

- `agent.exe` 里含**受限**密钥的明文(`strings` 可见)——这是既有设计,它本来就要分发到每台云电脑;真正锁死损失的是该密钥的 RAM 策略。
- `admin` 的**读写**密钥**不在任何发布物里**,只在你运行时手动输入。
- 请确保本仓库为 **private**。

## 校验

`SHA256SUMS.txt` 内含全部二进制的 SHA-256。版本号已通过 `-X main.version=<tag>` 编入,`version` 子命令可查。
