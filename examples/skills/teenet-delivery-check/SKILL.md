---
name: teenet-delivery-check
description: 验证公司 skill 的加载、附件读取和版本更新。当用户说“公司技能下发测试”、要求检查下发的 skill 是否生效，或点名 teenet-delivery-check 时使用。
---

# 公司技能下发测试

每次执行都先读取本 skill 目录内的 [references/check-marker.txt](references/check-marker.txt)。从本次读取结果中取得 `version` 和 `marker`，不要根据上一轮对话补全或猜测。

用中文输出四行，行名依次为：

- 技能：固定为 `teenet-delivery-check`。
- 版本：参考文件中的 `version` 值。
- 校验码：参考文件中的 `marker` 值，原样保留大小写和连字符。
- 读取文件：本次实际读取的参考文件路径；工具只提供相对路径时如实保留。

如果无法读取参考文件，明确回复“校验失败：无法读取随 skill 下发的标记文件”，并说明遇到的问题，不输出猜测的版本或校验码。

本测试只读取随 skill 提供的标记文件，不需要执行脚本、修改文件或访问外部服务。能读取标记证明该文件可用；是否由 Agent 正确派发，还要结合后台版本回报和文件路径判断。
