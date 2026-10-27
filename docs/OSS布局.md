# OSS 布局规范

> **OSS 里的目录和文件怎么放，以本文件为准。**
> 其他文档（`操作手册.md`、`架构说明.md`、`dist/部署说明.md`、`README.md`）讲到 OSS 路径、文件格式、RAM 权限时，都以本文件为准，不再各写一遍。
> 代码里以 `go/internal/ossclient/client.go` 为准：所有文件路径（对象键）都由这个文件里的辅助函数生成。改布局时，**本文件和这个代码文件必须一起改**。

## 先认识几个词

| 词 | 意思 |
|---|---|
| OSS | 阿里云的云存储，本系统用它在管理员和云电脑之间传配置、凭据和状态。 |
| Bucket | OSS 里的一个存储空间，相当于一个顶层文件夹。文中的 `your-bucket` 指你们自己的 Bucket。 |
| 对象、键（key） | OSS 里的一个文件叫一个"对象"；它的完整路径叫"键"，例如 `agent_workdir/policy.json`。 |
| 前缀 | 键开头的一段路径，作用相当于目录，例如 `agent_workdir/work1/`。 |
| agent | 装在每台云电脑上的管理程序 `agent.exe`，以 Windows 的 SYSTEM（系统最高权限）身份运行。 |
| admin | 管理员使用的程序 `admin.exe`，只在管理员自己的电脑上运行。 |
| RAM | 阿里云的访问控制服务。给每把密钥配一份"权限策略"，规定它能读写哪些路径。 |
| 无影 | 阿里云的云电脑产品。ECD 是它的英文缩写。 |
| 绑定 | 管理员指定"这台机器归哪个员工用"。 |
| SHA-256 | 文件的"指纹"。指纹一致，说明下载到的文件没被换掉或损坏。 |
| 封禁策略 | 禁止云电脑访问某些网站（如 `openai.com`）的规则。 |

---

## 1. 总则

**Bucket 下只有两个顶级目录，二者平级。哪些东西放在哪个目录，就决定了谁有权限碰它。**

```
your-bucket/
├── admin/                 管理员专用，agent 完全够不到
│   └── users.json
└── agent_workdir/         agent 的全部活动范围
    ├── policy.json        机器级封禁策略，全局一份
    ├── _bindings/         按【主机名】索引
    ├── _status/           按【主机名】索引
    ├── work1/             按【员工】索引，一个员工的东西全在自己目录里
    └── work2/
```

### 为什么分成两个目录

员工名单是唯一一样所有 agent 都不该看到的东西。有两种做法：

- 把名单放在 `agent_workdir/` **外面**：agent 的权限范围本来就不包含它。
- 把名单放在 `agent_workdir/` 里面，再在权限里把它排除：权限范围包含它，靠人记得排除。

前一种更牢靠，因为少了一个可能写错的地方。所以采用前一种。

### 为什么 agent 的东西都收在一个目录下

1. **Bucket 可以复用**：客户可以在同一个 Bucket 里放别的东西，互不干扰。
2. **权限好写**：agent 的 RAM 权限只需锁在 `agent_workdir/` 这一条路径上，碰不到 Bucket 里的其他内容。
3. **好清理**：整套方案下线时，删掉两个目录即可。

### 一个员工一个目录

属于某个员工的所有东西（凭据，以及后来的采集数据）都放在 `agent_workdir/{员工}/` 下。

- 员工离职时，删掉这一个目录就清理干净了。
- 要给采集数据设自动过期规则（OSS 叫"生命周期规则"），也只需针对这一个前缀。

### 封禁策略不属于任何员工

封禁策略放在 `agent_workdir/policy.json`，全局只有一份。原因：

- 它写进注册表的 `HKLM`（对整台机器生效的部分），保护的是整台机器，和谁登录无关。
- 如果按员工存，agent 要先知道这台机器归谁，才能取到策略。**可是最需要封住的，恰恰是还没分配的机器。**

以前就是按员工存的，结果出了问题：刚开出来、还没绑定的机器，以及刚登记、还没被任何策略命令覆盖到的新员工，浏览器访问完全没有限制。

### 为什么 `_bindings/` 和 `_status/` 不能放进员工目录

因为这两个目录按**主机名**查找，不按员工查找。

- agent 开机时只知道自己的主机名，**还不知道自己给谁用**。`_bindings/{主机名}.json` 正是用来查这个的。如果把它放进员工目录，就成了死循环：要先知道员工是谁，才能找到"员工是谁"。唯一的办法是列出所有目录去猜，但列目录的权限是特意不给 agent 的，因为给了就等于能看到全部员工。
- 机器上报状态时，可能**还没绑定**（`admin.exe status` 里显示 `UNBOUND`，即"未绑定"）。这时它不属于任何员工，没有员工目录可放。但这时最需要它上报，否则管理员不知道有台机器在等分配。

所以 `agent_workdir/` 下有三类东西：

- 全局的 `policy.json`；
- 按机器划分的 `_bindings/` 和 `_status/`；
- 按员工划分的 `{员工}/`。

目录名前面的下划线，用来一眼看出"这不是某个员工的目录"。

### 其他要求

- 所有目录都在第一次写入文件时自动出现，**不需要手工创建**。
- Bucket 必须设为**私有**读写，绝不能设成公共读。

---

## 2. 完整结构

```
your-bucket/
│
├── admin/                               管理员专用，agent 无任何权限
│   └── users.json                       员工名单与 AI 账户映射
│
└── agent_workdir/                       agent 的全部活动范围
    │
    ├── policy.json                      封禁策略 + 同步间隔（全局一份，agent 只读）
    │
    ├── _bindings/                       机器归属，agent 只读
    │   ├── DESKTOP-A.json               { "user": "work1", ... }
    │   ├── iZt4n2up5vmbxv0bvvnp1oZ.json
    │   └── ...                          一台机器一个文件，按主机名命名
    │
    ├── _status/                         机器上报，agent 只写
    │   ├── DESKTOP-A.json
    │   ├── iZt4n2up5vmbxv0bvvnp1oZ.json
    │   └── ...                          一台机器一个文件，按主机名命名
    │
    ├── _agent/                          agent 自更新用（agent 只读，见 §7）
    │   ├── agent.exe                    当前全局目标的服务端副本（控制台复制，不再由人直接上传）
    │   └── {版本}/agent.exe             版本库按版本留存
    │
    ├── _codex/                          Codex 桌面版分发（agent 只读，见 §7b）
    │   └── codex-setup-{版本}.exe       按版本留存，便于回滚
    │
    ├── _logs/                           机器日志上报（agent 只写,admin 读）
    │   └── {主机名}.log                 每台机器最近的 agent 日志尾部
    │
    ├── work1/                           按【员工】分目录
    │   ├── credentials.zip              该员工的 AI 工具凭据（agent 只读）
    │   └── data_collect/                该员工的原始会话（已实现，默认关闭）
    │       └── ...                      结构见 §6
    ├── work2/
    │   ├── credentials.zip
    │   └── data_collect/
    └── ...
```

一个员工的东西全在自己目录里，`agent_workdir/{员工}/` 这一个前缀就覆盖了这个人的全部内容。

**为什么 `agent_workdir/` 里面还要再分前缀**：agent 的密钥存在云电脑本地，有本地管理员权限的人理论上能把它取走。按前缀分别限制权限后，即使密钥泄露，拿到的人也：

- **改不了任何配置**；
- **读不到别的机器的状态**；
- **动不了 `agent_workdir/` 以外的东西**。

员工名单本来就在这个目录之外，不受影响。

---

## 3. 两个身份

这套布局里有两个不同的名字，容易混淆：

| 身份 | 取值 | 决定什么 |
|---|---|---|
| **机器身份** | 主机名（`os.Hostname()`） | 读哪个 `_bindings/{主机名}.json`、写哪个 `_status/{主机名}.json` |
| **服务对象** | 绑定文件里的 `user` 字段 | 读哪个 `{员工}/` 目录、凭据投递到 `C:\Users\{员工}\` |

为什么机器身份只能用主机名：agent 以 SYSTEM 身份运行，用 `os.UserHomeDir()` 取到的是系统账户的目录，不是员工的目录。员工是谁，由管理员在 OSS 上写绑定文件告诉它。

无影批量创建的机器，主机名是自动生成的（形如 `iZt4n2up5vmbxv0bvvnp1oZ`）。管理员不需要事先知道：机器开机后会自己上报到 `_status/`，管理员在 `admin.exe status` 里就能看到它，然后绑定。

---

## 4. 每个文件的说明

所有键都由 `go/internal/ossclient/client.go` 里的辅助函数生成。不要在别处手工拼路径。

| 对象 | 键 | 生成函数 | 谁写 | 谁读 |
|---|---|---|---|---|
| 员工名单 | `admin/users.json` | `AdminKey("users.json")` | admin | admin |
| 机器绑定 | `agent_workdir/_bindings/{主机名}.json` | `BindingKey(machine)` | admin | agent |
| 机器状态 | `agent_workdir/_status/{主机名}.json` | `StatusKey(machine)` | agent | admin |
| 封禁策略 | `agent_workdir/policy.json` | `PolicyKey()` | admin | agent |
| AI 凭据 | `agent_workdir/{员工}/credentials.zip` | `UserKey(user, "credentials.zip")` | admin | agent |
| 员工目录 | `agent_workdir/{员工}/` | `UserPrefix(user)` | — | — |
| 采集数据 | `agent_workdir/{员工}/data_collect/` | `DataCollectPrefix(user)` | agent | admin |

**路径清洗**：`{员工}` 和 `{主机名}` 都来自机器本地，不能信任。辅助函数会把它们压成单独一级路径，所以 `../other`、`a/b/c`、`..\evil` 这类写法都跳不出自己的前缀。这一点有测试覆盖。

### 4.1 `admin/users.json`（员工名单）

```json
{
  "users": [
    {
      "windowsUser": "work1",
      "codexAccount": "ai-work1@company.com",
      "enabled": true
    }
  ],
  "updatedAt": "2026-08-04T10:00:00Z"
}
```

- 账户邮箱**只是给管理员看的备注**，程序不用它登录任何东西。
- `enabled` 为 false 时，admin 不再往这个员工的目录下发策略。

**为什么单独放在 `agent_workdir/` 外面**：这是整套系统里最敏感的数据，一眼就能看出公司有哪些员工、用了哪些 AI 账号。放在 agent 的目录之外，agent 的权限无论怎么写都碰不到它，不依赖"记得排除某个前缀"这种约定。

### 4.2 `agent_workdir/_bindings/{主机名}.json`（机器归属）

```json
{
  "user": "work1",
  "boundAt": "2026-08-04T10:00:00Z",
  "note": "研发部-张三"
}
```

agent 每轮同步的第一件事就是读这个文件。

**读不到，说明这台机器还没分配。** 这时 agent 仍会上报状态，让管理员看到它在等分配；但不下发任何凭据。

### 4.3 `agent_workdir/_status/{主机名}.json`（机器状态）

agent 每轮同步后写入。完整字段见 `go/internal/model/model.go` 里的 `Status`，主要有：

| 字段 | 含义 |
|---|---|
| `machine` / `boundUser` | 主机名、绑定的员工 |
| `localUsers` | 本机上有哪些账户，用来发现不该有的账户 |
| `boundUserExists` | 绑定的员工在本机有没有用户目录 |
| `lastSync` / `agentVersion` | 上报时间、agent 版本 |
| `policyEtag` / `credsEtag` | 已应用的策略和凭据的版本号，用来比较各台机器是否一致 |
| `blockEnabled` / `blockedDomains` | 封禁开关、封禁了多少个域名 |
| `syncIntervalMinutes` | 当前生效的同步间隔 |
| `credsApplied` / `appLockerMode` | 凭据是否已投递；AppLocker（Windows 程序白名单）处于什么模式 |
| `collectEnabled` / `collectUploaded` | 本机会话采集是否开启、本轮上传了几个文件（见 §6） |
| `errors` | 本轮出的错，一条一条的文字 |

**agent 只写、不读这个文件。** 管理员用 `admin.exe status` 汇总查看所有机器。

### 4.4 `agent_workdir/policy.json`（封禁策略）

```json
{
  "blockEnabled": true,
  "blockedDomains": ["openai.com", "chatgpt.com", "claude.ai", "anthropic.com"],
  "syncIntervalMinutes": 30,
  "collectEnabled": false,
  "collectQuietSeconds": 60,
  "collectSince": "",
  "updatedAt": "2026-08-04T10:00:00Z"
}
```

**全局只有一份**，和员工名单无关。管理员改一次，所有机器下次同步时都会拿到。

agent **一定会读它、一定会应用它**，不管机器有没有绑定。所以一台刚从镜像开出来、还没分配的机器，第一次同步就会被封住。

`syncIntervalMinutes`（同步间隔）的规则：

- 取值会被强制限制在 **1–1440** 分钟之间。这样即使误设了一个极端值，机器也不会因为再也拉不到新配置而失联。
- 字段缺失或为 0 时，用 agent 自带配置里的值；自带配置里也没有，就用内置默认值 30 分钟。

`collectEnabled`、`collectQuietSeconds`、`collectSince` 控制会话采集（见 §6）。三个都可以不写，不写就是关闭：

- `collectEnabled` 默认 `false`；
- `collectQuietSeconds` 不写或为 0 时，按 60 秒处理；
- `collectSince` 不写，就采全部历史。

### 4.5 `agent_workdir/{员工}/credentials.zip`（AI 凭据）

zip 里的路径是固定的。agent 按下表把文件放到员工的用户目录：

| zip 内路径 | 放到员工用户目录的位置 |
|---|---|
| `codex/auth.json` | `C:\Users\{员工}\.codex\auth.json` |
| `codex/config.toml` | `C:\Users\{员工}\.codex\config.toml` |

- 管理员执行 `admin.exe login` 时，登录结果（OAuth 授权结果）在内存里打包后直接上传，**不会保存到管理员本地磁盘**。
- 如果只登录了其中一个工具，zip 里只有那一部分。agent 投递时会和机器上已有的文件合并，不会抹掉另一个工具的凭据。

---

## 5. RAM 权限

**要建两个 RAM 用户，权限差别很大，必须分开。**

### 5.1 管理员用户

给 `admin.exe` 用，只放在管理员自己的电脑上。

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["oss:*"],
      "Resource": [
        "acs:oss:*:*:your-bucket-name",
        "acs:oss:*:*:your-bucket-name/admin/*",
        "acs:oss:*:*:your-bucket-name/agent_workdir/*"
      ]
    }
  ]
}
```

`Resource` 里的三行都要有：

- 第一行是 Bucket 本身，列出文件时需要它；
- 后两行分别是两个目录里的文件。

另外，管理员用户还要加阿里云系统策略 **`AliyunECDReadOnlyAccess`**（无影云电脑只读权限）。控制台用同一把密钥查询云电脑的状态，从而区分"机器在休眠"和"agent 挂了"。

**这条只给管理员用户。** agent 的密钥在每台员工机器上都是明文；如果给它这条权限，任何一台机器都能列出全公司的云电脑。

### 5.2 Agent 用户

给 `agent.exe` 用。它会随镜像分发到所有云电脑，所以权限必须收紧。交付包里的 `dist/ram-policy-agent.json` 就是这份策略：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["oss:GetObject"],
      "Resource": [
        "acs:oss:*:*:your-bucket-name/agent_workdir/policy.json",
        "acs:oss:*:*:your-bucket-name/agent_workdir/_bindings/*",
        "acs:oss:*:*:your-bucket-name/agent_workdir/*/credentials.zip"
      ]
    },
    {
      "Effect": "Allow",
      "Action": ["oss:PutObject"],
      "Resource": ["acs:oss:*:*:your-bucket-name/agent_workdir/_status/*"]
    }
  ]
}
```

（`oss:GetObject` 是读文件，`oss:PutObject` 是写文件。）

### 5.3 权限对照

| 路径 | admin | agent |
|---|---|---|
| `admin/` | 读写 | **完全够不到** |
| `agent_workdir/_bindings/` | 读写 | 只读 |
| `agent_workdir/_status/` | 读写 | **只写** |
| `agent_workdir/policy.json` | 读写 | 只读 |
| `agent_workdir/{员工}/credentials.zip` | 读写 | 只读 |
| `agent_workdir/{员工}/data_collect/` | 读写 | 只写，且**默认关闭**（见 §6） |
| Bucket 内其他路径 | 无 | 无 |

agent **明确不能做的事**：

- 读员工名单；
- 写任何策略或凭据；
- 删除任何文件；
- 列出 Bucket 里的文件；
- 访问 `agent_workdir/` 以外的任何内容。

注意：agent 的权限策略里**从来没有出现过 `admin/`**。不是"先允许、再排除"，而是根本没给。

"agent 密钥泄露也没关系"这个判断，就建立在这份策略上。密钥本身在 exe 里是明文，挡不住有心人；真正限制损失的是这份策略。

---

## 6. `{员工}/data_collect/`：会话采集（本版已实现，默认关闭）

### 做什么

采集**绑定员工**使用 Codex 的原始会话记录，用于离线分析。

- 代码已经实现，在 `go/internal/agentcore/collect.go`。
- 但**默认关闭**，有两道开关：
  - `policy.json` 里的 `collectEnabled` 默认为 `false`；
  - agent 的 RAM 权限默认不包含写 `data_collect` 的权限。
- 两道开关任何一道没开，都不会有文件写进来。

### 采什么

只采**绑定员工**本人用户目录下的：

- `.codex/sessions/**/rollout-*.jsonl`

原样上传，**不做任何脱敏，也不解析内容**。

### 存到哪里

路径和源目录保持一致，前面加上 `agent_workdir/{员工}/data_collect/`：

```
agent_workdir/work1/data_collect/.codex/sessions/{y}/{m}/{d}/rollout-....jsonl
```

也就是说，`DataCollectKey(user, rel)` = `DataCollectPrefix(user)` + 这个文件相对于用户目录的路径（`.codex/...`）。大小写和目录层级都原样保留。

### 开关字段

这些字段在 `policy.json` 里（见 §4.4）：

| 字段 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `collectEnabled` | bool | `false` | 总开关 |
| `collectQuietSeconds` | int，可选 | `60` | 文件最后一次修改后，要过这么多秒才采。避免把还在写的会话只采走一半 |
| `collectSince` | string，可选，`YYYY-MM-DD` | 空 | 只采这一天及以后修改的文件（按 **UTC** 日期比较）；空表示采全部历史 |

### 怎么开启和关闭

开启（顺序不能反）：

1. 先给 agent 的 RAM 策略加上本节末尾那条 `PutObject` 权限。`dist/ram-policy-agent.json` 和已经部署的 RAM 策略都要加。
2. 再把 `policy.json` 里的 `collectEnabled` 改成 `true`。

关闭就反过来：先把 `collectEnabled` 改回 `false`。权限可以留着，也可以之后再收回。

### 权限边界

agent 对 `data_collect/` **只能写、不能读**（没有 `oss:GetObject` 读文件权限，也没有 `oss:ListObjects` 列文件权限）。这符合 §5 的"只给必需的最小权限"原则：就算某台机器的密钥泄露，也读不到任何人的对话记录。

怎么避免重复上传：

- 靠**本地状态文件**：agent 状态目录下的 `collect-state.json`。它记录每个源文件的修改时间（mtime）和大小（size）。
- 不靠回头读 OSS 来判断。
- 本地源文件被删除后，它会从状态文件里移除；但已经上传到 OSS 的副本不会跟着删除。

### 什么时候运行

采集是 `agent.exe` 每轮同步中的一步。三个条件同时满足才运行：

- `collectEnabled=true`；
- 机器已绑定；
- 绑定的员工在本机有用户目录。

它跟着同步间隔走，不单独计时。

### 怎么验证

1. 运行一次 `agent.exe sync`，输出里会有一行 `collect=<true/false> uploaded=<N>`（是否开启采集、上传了几个文件）。
2. 机器写回的 `agent_workdir/_status/{主机名}.json`（也就是 `admin.exe status` 读取的文件）里有 `collectEnabled` 和 `collectUploaded` 两个字段。不过目前 `admin.exe status` 的汇总表格没有单独显示这两列。
3. 到 OSS 控制台，确认文件确实出现在 `agent_workdir/{员工}/data_collect/` 下。

### 为什么放在每个员工目录里，而不是一个共享的 `data_collect/`

- **按人清理**：员工离职时，删掉 `agent_workdir/work1/` 这一个目录，策略、凭据、对话记录就一起没了，不用再去别处找。
- **按人设过期规则**：对话数据比配置大得多。要设 OSS 生命周期规则时，一个员工一个前缀最好写。
- **按人授权**：将来如果要做"某个负责人只能看自己组的数据"，前缀就是现成的分界线。
- **一致**：属于某个员工的东西都在他自己的目录下，不用记"这个例外放在别处"。

### 为什么放在 `agent_workdir/` 里面，而不是和它平级

这些数据是 **agent 写的**，属于 agent 的活动范围。`admin/` 只放管理员自己维护、agent 永远碰不到的东西。

### 开启时要加的权限

代码已实现，但授权仍要手工加一步。**开启时给 agent 加这一条**（`dist/ram-policy-agent.json` 默认不含它，见 `dist/部署说明.md`）：

> ```json
> {
>   "Effect": "Allow",
>   "Action": ["oss:PutObject"],
>   "Resource": ["acs:oss:*:*:your-bucket-name/agent_workdir/*/data_collect/*"]
> }
> ```
>
> **只给写，绝不给读。** 如果给了读，一台机器的密钥泄露，就等于所有员工的对话记录泄露。对话记录比配置和凭据敏感得多。
>
> 功能开启前，不要提前加这条权限。

---

## 7. `_agent/`：agent 自动更新（默认关闭）

管理员发布新版 agent 时，把程序文件放到 `agent_workdir/_agent/agent.exe`，并在 `policy.json` 里写上目标版本和这个文件的 SHA-256 指纹：

| 字段（policy.json） | 含义 |
|---|---|
| `agentUpdateVersion` | 目标版本。**空表示不更新**（默认）。设了值后，自身版本和它不同的 agent 才会更新 |
| `agentUpdateSHA256` | `_agent/agent.exe` 的 SHA-256（十六进制）。agent 替换前必须核对一致 |

### agent 这边怎么更新

每轮同步时，如果 `agentUpdateVersion` 不为空、且和自身版本不同：

1. 下载 `_agent/agent.exe`。
2. **核对 SHA-256。** 不一致就不替换，并在状态里报错。
3. 替换程序文件，旧文件另存为 `agent.exe.old`。
4. 重启服务。

**同一个目标版本只尝试一次**（在本地做标记），所以坏包不会导致机器反复重启。

### 权限

agent 对 `_agent/*` 只需要 **GetObject**（读）权限，`dist/ram-policy-agent.json` 里已经有这条。

**只给读，绝不给写。** 如果给了写，任何一台机器都能往这里放一个程序，而所有机器都会执行它。

### 风险和应对（详见 `操作手册`）

- `policy.json` 是全局的，所以**所有只读全局策略的机器会一起更新**，没有"先更新一小部分机器"（灰度）这一步。
- **没有自动回滚。**

因此：

- **新版 agent 先在测试机上验收，再"设为全局目标"。**
- 出事时，在版本库页面点"清除"紧急停止，再手动把 `agent.exe.old` 换回来。

### 7.1 有了版本库之后（控制台数据库模式，2026-09-19 起）

**按版本存放。** 每个上传的 agent 包存为 `_agent/{版本}/agent.exe`，并登记进数据库（版本、SHA-256、大小）。**一个版本号只对应一个包**，重新构建就要换版本号。

**设为全局目标。** 这个操作只改 `policy.json`。Worker（控制台的后台处理程序）每次导出策略前，会在 OSS 服务端把该版本复制到 `_agent/agent.exe`。所以旧版 agent 读的还是这个固定路径，文件内容永远和策略里的 SHA-256 一致。回滚就是把全局目标设回旧版本，不用重新上传。

**按机器指定目标**（agent 1.2.16 起）：`_bindings/<机器>` 里可以写 `agentTarget` / `codexTarget`：

| 字段 | 含义 |
|---|---|
| `version` | 目标版本；**空表示这台机器不装**（即使全局策略设了目标） |
| `sha256` | 包的 SHA-256 |
| `key` | 包在 OSS 上的路径（agent 的这个字段为空时，用固定路径） |
| `generation` | 代次。同一版本换一个代次，就表示"再试一次"。agent 会在状态里回报代次，控制台据此判断一份回报对应的是哪一次决定 |

- 这些字段**只要存在，就覆盖全局目标**；不存在，就沿用全局策略。
- 1.2.16 以前的 agent 会忽略这些字段。
- 没绑定员工的机器也可以有目标（这时文件里的 `user` 为空）。
- RAM 权限不变：agent 读 `_bindings/*`、`_agent/*`、`_codex/*`。

**按需同步**（agent 1.2.16 起）：

- agent 每分钟心跳一次。心跳时用 HEAD 请求查看 `policy.json` 和自己的绑定文件，只取文件信息、不下载内容。
- 如果文件的 ETag（文件版本标识）变了，就立刻做一次完整同步。所以后台的任何改动，一分钟内就能到达机器；同步间隔只是兜底。
- 控制台的"立即同步"按钮，会往绑定文件里写一个随机值 `syncRequested`，让文件内容发生变化，从而触发同步。agent 不理会这个值本身。
- 1.2.16 以前的 agent 只按同步间隔读取。

**新增的状态字段。** agent 状态里多了 `agentUpdateTarget/agentUpdateGeneration/agentUpdateState`、`codexTarget/codexTargetGeneration/codexDeferReason`。控制台根据它们判断每台机器的目标是否完成：

- 实际版本等于目标版本，且上报时间晚于目标创建时间 → 成功；
- 同一代次报了 `failed` → 失败；
- 推迟、下载中、离线 → 保持"待执行"。

---

## 7b. `_codex/`：分发 Codex 桌面版（默认关闭）

管理员用 `admin codex publish` 把重新打包的 Codex 安装程序放到 `agent_workdir/_codex/codex-setup-{版本}.exe`，并在 `policy.json` 里写上目标：

| 字段（policy.json） | 含义 |
|---|---|
| `codexVersion` | 目标版本。**空表示不分发**（默认）。**按"是否相等"比较**：机器上已装的版本和这个值不同，就安装这个值对应的版本 |
| `codexSHA256` | 安装程序的 SHA-256（十六进制），安装前必须核对一致 |
| `codexKey` | 安装程序在 `_codex/` 下的路径 |
| `codexRolloutPct` | **已废弃，固定为 100。** 它曾是灰度比例，按 `crc32(主机名) % 100` 决定哪些机器先装。现在仍然写出，因为 agent 1.2.5–1.2.7 把它当开关用：字段缺失或为 0 时，**一台都不装，也不报错**。新版 agent 忽略它 |

**为什么按"是否相等"、而不按"新旧"比较**：把 `codexVersion` 改回旧版本，**就是回滚**，agent 会把旧版装回去。如果是"只升不降"，就做不到这一点；而包有问题时，正需要往回退。§7 的 `agentUpdateVersion` 也是同样的规则。

**为什么按版本分别存，而不是覆盖同一个文件**：安装程序约 700 MB。保留旧版，回滚时只需改一个策略字段，不用重新上传。

**和 §7 一样，是一次发给所有机器。** 以前这里有灰度，理由是：这个包是对上游 Codex 的重新打包，上游每个版本都可能变化。CI（自动构建流程）通过，只能证明补丁打上了，**不能证明 ChatGPT 入口真的消失了、Computer Use 功能仍然能用**。这个理由现在仍然成立，但灰度解决不了它：机器数量少，10% 的比例常常一台都抽不中；真正的验收只能在 Windows 真机上做。所以：**先在真机上验收，再发布。**

**权限**：agent 对 `_codex/*` 只需要 **GetObject**（读）权限。**只给读，绝不给写**，理由和 §7 相同。

**已实现的情况**：

- agent 从 **1.2.5** 起，会下载、核对 SHA-256、静默安装，并上报实际装上的版本。设计见 [Codex分发方案](Codex分发方案.md)。
- 包的版本号已经和上游版本号区分开，格式是 `<应用版本>-b<构建号>`。
- **安装程序仍然是按单个用户安装的**（`PrivilegesRequired=lowest`）。目前能用，是因为镜像把 Codex 装在 `C:\tools\Codex`。

---

## 8. 改布局时的检查清单

布局是 agent 和 admin 之间的约定。只改其中一边，就会出现"写进去了，但没人读"。改动时逐项确认：

- [ ] 改 `go/internal/ossclient/client.go` 里的 `Root` / `AdminRoot` / 前缀常量 / 生成路径的辅助函数。
- [ ] `admin/` 和 `agent_workdir/` 必须保持平级、互不包含（有测试检查）。否则一条带通配符的授权会把另一个目录也覆盖进去。
- [ ] 确认所有调用的地方都通过辅助函数生成路径，没有手工拼的路径（`admincore` 以前犯过这个错）。
- [ ] 运行 `go test ./...`。`ossclient` 和 `admincore` 里有检查这条约定的测试。
- [ ] 更新本文件。
- [ ] 更新 `dist/ram-policy-agent.json`。
- [ ] 已经部署的环境：RAM 策略要先加上新路径，等所有 agent 都升级完，**再撤掉旧路径**。顺序反了，机器会集体失联。
