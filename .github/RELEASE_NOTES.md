自动构建的 Windows 二进制（windows/amd64）。

## 两个二进制的凭据状态不一样

| 文件 | 凭据 | 说明 |
|---|---|---|
| `agent.exe` | **已注入受限密钥** | CI 在链接时把 agent 那把**受限** OSS 密钥（来自仓库 Secrets）编了进去，`config=built-in`，可直接进镜像 |
| `admin.exe` | **无凭据模板** | 管理员那把**全桶读写**密钥**绝不**编进发布物；这个 admin.exe 需在本机填密钥后自行编译，或配 `admin.config.json` |

> 若仓库未配置 agent 的 Secrets，`agent.exe` 也会退化成无凭据模板（运行显示 `config=config file`），CI 日志里会有 warning。

## 安全说明

- `agent.exe` 里含**受限**密钥的明文（`strings` 可见）——这是既有设计:它本来就要分发到每台云电脑。真正把损失锁死的是该密钥的 RAM 策略（只能读策略/凭据、写状态，读不到员工名单、改不了配置）。
- `admin.exe` 的读写密钥不在此处，也不该在任何公开发布物里。
- 请确保本仓库为 **private**。

## 校验

`SHA256SUMS.txt` 内含两个 exe 的 SHA-256。版本号已通过 `-X main.version=<tag>` 编入，`agent.exe version` / `admin.exe version` 可查。
