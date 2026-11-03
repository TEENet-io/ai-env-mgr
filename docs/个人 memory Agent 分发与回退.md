# 个人 memory 的 Agent 分发与回退

适用范围：个人 memory 内部试用版。Admin 通过现有 Codex 发布目标下发安装包；Agent 不读取、上传或汇总员工的 memory 卡片正文。

## 安装后的文件流

1. Admin 发布包含个人 memory 的 Codex 安装包，现有发布任务把它安装到机器级目录 `C:\tools\Codex`（或 `AIENVMGR_CODEX_ROOT`）。
2. Agent 验证包已安装后，从 `_internal\seed\personal-memory\AGENTS-block.md` 和 `_internal\seed\codex-home\skills` 读取三个 skill，从 `_internal\memory_store.exe` 读取 helper。员工的 `.codex\AGENTS.md` 只插入或更新带 `teenet-personal-memory` 标记的区块；其他内容保留。
3. Agent 将三个 skill 放进绑定员工的 `.codex\skills`，并写入 `.codex\.offline-package\memory-store-path.txt`，指向机器级 helper。同步状态及哈希保存在同目录的 `personal-memory-agent.json`。
4. Codex 在员工自己的会话中调用 helper。卡片只写在该员工的 `%USERPROFILE%\.ai-memory`；Agent 不接触这个目录，也不会因卡片变化重启 Codex。

每次 Agent 同步只核对受管理文件和哈希；内容未变时不重写。若某个同名 skill 是员工自行创建或受管理文件被手工改动，同步报错并保留原文件，避免覆盖个人工作。同步错误进入机器状态。

## 解绑、升级、回退

- **解绑或转绑**：Agent 移除旧员工档案中的受管理 AGENTS 区块、三个 skill 和 helper 路径文件。`.ai-memory` 卡片与历史版本保留，按员工档案的既有生命周期处理。
- **升级**：新包安装成功后，Agent 按包内文件哈希同步；未完成安装时不下发部分内容。
- **回退到不带个人 memory 的 Codex 包**：Agent 识别到包内没有该功能的种子文件，移除之前受管理的入口。卡片不删除。
- **员工手动修改受管理文件**：Agent 停止覆盖并报告冲突；操作人员先与员工确认，再恢复由 Agent 管理。不要直接清空整个 `.codex`。

本分支未发布到生产。目标 Windows 云电脑仍须用普通员工账号验证：AppLocker 放行 `C:\tools\Codex\_internal\memory_store.exe`、Codex 沙盒允许读写 `.ai-memory`、全局 AGENTS 指令确实进入新对话，以及真实点击按钮后的保存和删除链路。
