自动构建的 Windows 二进制（`agent.exe` / `admin.exe`，windows/amd64）。

## ⚠️ 这是无凭据模板，不能直接部署到生产

仓库里的 `go/cmd/agent/credentials.go` 与 `go/cmd/admin/credentials.go` 是**空模板**，所以这里的二进制**不带任何 OSS 密钥**：

- `agent.exe` 运行时会显示 `config=config file`（源码里没填密钥），**不要直接打进镜像**。
- 生产用的二进制要在 `go/cmd/*/credentials.go` 填入密钥后**本地自行编译**（`cd go && make all`，见 `dist/部署说明.md`）。
- `admin.exe` 那把是**全桶读写**密钥，**绝不能**编进任何公开发布物——真正的 admin 二进制只应留在管理员本机。

## 用途

- 快速验证构建产物、临时联调（配合本地 `agent.config.json` / `admin.config.json`）。
- 校验完整性：`SHA256SUMS.txt` 里有两个 exe 的 SHA-256。

版本号已通过 `-X main.version=<tag>` 编进二进制，`agent.exe version` / `admin.exe version` 可查。
