# OSS 布局规范

> **本文件是 OSS 结构的唯一权威来源。**
> 其他文档（`操作手册.md`、`架构说明.md`、`dist/部署说明.md`、`README.md`）涉及 OSS 路径、对象格式、RAM 权限时，一律以本文件为准，不再各自重复描述。
> 代码侧的唯一来源是 `go/internal/ossclient/client.go`——所有对象键都由那里的 helper 生成。改布局时**两处必须同时改**。

---

## 1. 总则

**两个顶级目录，平级，边界就是权限边界。**

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

**为什么分成两个而不是一个**：员工名单是唯一一样任何 agent 都不该看到的东西。把它放在 `agent_workdir/` **外面**，比放在里面再排除一个前缀更牢靠——前者是"授权范围根本不包含它"，后者是"授权范围包含它但我们记得排除"。少一个能写错的地方。

**为什么 agent 的东西收在一个目录下**：

1. **Bucket 可复用** —— 客户可以拿同一个 Bucket 放别的东西，互不干扰
2. **权限好写** —— agent 的 RAM 授权锁在 `agent_workdir/` 一条路径上，碰不到 Bucket 里其他内容
3. **好清理** —— 整个方案下线时删两个目录即可

**一个员工 = 一个目录**：属于某个员工的所有东西（凭据、以及后续的采集数据）都在 `agent_workdir/{员工}/` 下。员工离职时删一个目录就干净了；要给采集数据设生命周期规则，也是一个前缀的事。

**但封禁策略不属于任何员工**，所以它在 `agent_workdir/policy.json`，全局一份。理由是它写进 `HKLM`、保护整台机器、与谁登录无关；如果按员工存，取它就必须先知道这台机器归谁——而**恰恰是还没分配的机器最需要被封住**。曾经就是按员工存的，导致刚开出来还没绑定的机器、以及刚登记还没被任何策略命令波及的新员工，浏览器完全放开。

**为什么 `_bindings/` 和 `_status/` 不能塞进员工目录**：这两个是按**主机名**索引的，不是按员工。

- agent 开机时只知道自己的主机名，**还不知道自己服务谁**——`_bindings/{主机名}.json` 正是用来查这个的。放进员工目录就成了死循环：要先知道员工才能找到"员工是谁"，只能靠列举目录去猜，而列举权限是特意不给的（给了就等于能看到全部员工）。
- 机器上报时可能**还没绑定**（`admin.exe status` 里显示 `UNBOUND`）。这时它不属于任何员工，没有员工目录可放，但恰恰这时最需要它上报——不然管理员根本不知道有台机器在等分配。

所以 `agent_workdir/` 下有三样东西并存：全局的 `policy.json`、机器维度的 `_bindings/` 与 `_status/`、员工维度的 `{员工}/`。下划线前缀就是用来一眼区分"这不是某个员工的目录"。

目录都是写入时自动生成的，**不需要手工创建**。

Bucket 本身必须是**私有**读写权限，绝不能设公共读。

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
    │   └── agent.exe                    管理员发布的新版二进制
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

一个员工的东西全在自己目录里，`agent_workdir/{员工}/` 一个前缀就覆盖这个人的全部。

**为什么 `agent_workdir/` 内部还要分前缀**：agent 的密钥在云电脑本地，理论上可能被有本地管理员权限的人取走。按前缀限制权限后，即使密钥泄露，拿到的人也**改不了任何配置**、**读不到别的机器的状态**、**动不了 `agent_workdir/` 以外的东西**——而员工名单本来就在这个目录之外，压根不在讨论范围内。

---

## 3. 两个身份

这套布局里有两个不同的名字，容易混淆：

| 身份 | 取值 | 决定什么 |
|---|---|---|
| **机器身份** | 主机名（`os.Hostname()`） | 读哪个 `_bindings/{主机名}.json`、写哪个 `_status/{主机名}.json` |
| **服务对象** | 绑定文件里的 `user` 字段 | 读哪个 `{员工}/` 目录、凭据投递到 `C:\Users\{员工}\` |

agent 以 SYSTEM 身份运行，`os.UserHomeDir()` 会返回 system profile 而不是员工目录，所以**机器身份只能用主机名**。员工是谁，由管理员在 OSS 上绑定告知。

无影批量创建的机器主机名是自动生成的（形如 `iZt4n2up5vmbxv0bvvnp1oZ`），管理员不需要事先知道——机器开机后会自己上报到 `_status/`，在 `admin.exe status` 里就能看到并绑定。

---

## 4. 对象逐个说明

所有键都由 `go/internal/ossclient/client.go` 的 helper 生成，不要在别处手工拼接。

| 对象 | 键 | 生成函数 | 写方 | 读方 |
|---|---|---|---|---|
| 员工名单 | `admin/users.json` | `AdminKey("users.json")` | admin | admin |
| 机器绑定 | `agent_workdir/_bindings/{主机名}.json` | `BindingKey(machine)` | admin | agent |
| 机器状态 | `agent_workdir/_status/{主机名}.json` | `StatusKey(machine)` | agent | admin |
| 封禁策略 | `agent_workdir/policy.json` | `PolicyKey()` | admin | agent |
| AI 凭据 | `agent_workdir/{员工}/credentials.zip` | `UserKey(user, "credentials.zip")` | admin | agent |
| 员工目录 | `agent_workdir/{员工}/` | `UserPrefix(user)` | — | — |
| 采集数据 | `agent_workdir/{员工}/data_collect/` | `DataCollectPrefix(user)` | agent | admin |

**路径消毒**：`{员工}` 和 `{主机名}` 都来自机器本地，是不可信输入。helper 会把它们压成单个路径元素——`../other`、`a/b/c`、`..\evil` 之类的构造都逃不出自己的前缀，测试有覆盖。

### 4.1 `admin/users.json`

```json
{
  "users": [
    {
      "windowsUser": "work1",
      "codexAccount": "ai-work1@company.com",
      "claudeAccount": "ai-work1@company.com",
      "enabled": true
    }
  ],
  "updatedAt": "2026-08-04T10:00:00Z"
}
```

账户邮箱**仅作管理员备注**，程序不用它登录任何东西。`enabled` 为 false 时 admin 不再往该员工目录下发策略。

**为什么单独放在 `agent_workdir/` 外面**：这是整套系统里最敏感的一份数据——它一次性暴露公司有哪些员工、用了哪些 AI 账号。放在 agent 的目录之外，意味着 agent 的授权无论怎么写都碰不到它，不依赖"记得排除某个前缀"这种约定。

### 4.2 `agent_workdir/_bindings/{主机名}.json`

```json
{
  "user": "work1",
  "boundAt": "2026-08-04T10:00:00Z",
  "note": "研发部-张三"
}
```

agent 每轮第一件事就是读它。**读不到说明这台机器还没分配**，此时 agent 仍会上报状态（这样管理员才看得见它在等分配），但不下发任何凭据。

### 4.3 `agent_workdir/_status/{主机名}.json`

agent 每轮同步后写入。字段见 `go/internal/model/model.go` 的 `Status`，主要有：

| 字段 | 含义 |
|---|---|
| `machine` / `boundUser` | 主机名、绑定的员工 |
| `localUsers` | 本机存在的账户列表，用于发现意外账户 |
| `boundUserExists` | 绑定的员工在本机有没有 profile |
| `lastSync` / `agentVersion` | 上报时间、agent 版本 |
| `policyEtag` / `credsEtag` | 已应用的策略与凭据版本，用于跨机器比对 |
| `blockEnabled` / `blockedDomains` | 封禁开关、封禁域名条数 |
| `syncIntervalMinutes` | 当前生效的同步间隔 |
| `credsApplied` / `appLockerMode` | 凭据是否已投递、AppLocker 处于什么模式 |
| `collectEnabled` / `collectUploaded` | 本机会话采集是否开启、本轮上传了多少个文件（见 §6） |
| `errors` | 本轮出的错，逐条文本 |

**agent 只写不读**。管理员用 `admin.exe status` 汇总所有机器。

### 4.4 `agent_workdir/policy.json`

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

**全局唯一一份**，与员工名单无关。管理员改一次，所有机器下次同步就拿到。

agent **无条件读它、无条件应用**，不看有没有绑定。所以一台刚从镜像开出来、还没分配给任何人的机器，第一次同步就会被封住。

`syncIntervalMinutes` 会被钳制在 **1–1440** 分钟。字段缺失或为 0 时，用 agent 自带配置里的值；再没有就用内置默认 30 分钟。钳制是为了防止误设一个极端值导致机器再也拉不到新配置而失联。

`collectEnabled` / `collectQuietSeconds` / `collectSince` 控制会话采集（§6），三者都是可选字段，缺省即为关闭；`collectEnabled` 默认 `false`，`collectQuietSeconds` 缺省或为 0 时按 60 秒处理，`collectSince` 缺省即采全部历史。

### 4.5 `agent_workdir/{员工}/credentials.zip`

zip 内的路径固定，agent 按下表映射到员工 profile：

| zip 内路径 | 落到员工 profile 的位置 |
|---|---|
| `codex/auth.json` | `C:\Users\{员工}\.codex\auth.json` |
| `codex/config.toml` | `C:\Users\{员工}\.codex\config.toml` |
| `claude/.credentials.json` | `C:\Users\{员工}\.claude\.credentials.json` |
| `claude.json` | `C:\Users\{员工}\.claude.json` |

管理员执行 `admin.exe login` 时，OAuth 结果在内存里打包直接上传，**不落管理员本地磁盘**。

只登录了其中一个工具时，zip 里只有那部分；agent 投递时会与已有文件合并，不会把另一个工具的凭据抹掉。

---

## 5. RAM 权限

**两个 RAM 用户，权限差别很大，必须分开建。**

### 5.1 管理员用户

给 `admin.exe` 用，只在管理员自己的电脑上。

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

**第一行是 Bucket 本身**（列举对象需要），后两行是两个目录里的对象。三行都要。

### 5.2 Agent 用户

给 `agent.exe` 用，随镜像分发到所有云电脑，权限必须收紧。交付包里的 `dist/ram-policy-agent.json` 就是这份：

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

agent **明确不能做的**：读员工名单、写任何策略或凭据、删除任何对象、列举 Bucket、访问 `agent_workdir/` 以外的任何内容。

注意 agent 的授权里**根本没有出现过 `admin/`**——不是"允许了又排除"，而是从未授予。

这是"agent 密钥泄露也没关系"这个判断成立的前提——密钥本身在 exe 里是明文的，挡不住有心人，真正限制损失的是这份策略。

---

## 6. `{员工}/data_collect/`（本版已实现，默认关闭）

用于采集**绑定员工**与 Codex / Claude 的原始会话数据，供离线分析。采集端本版**已实现**，代码在 `go/internal/agentcore/collect.go`，但**默认关闭**——`policy.json` 里的 `collectEnabled` 默认为 `false`，且 agent 的 RAM 授权默认不含 `data_collect` 的写权限。两者任一没打开都不会有对象写进来。

**采什么**：只采**绑定员工**本人的 profile 下：

- `.claude/projects/**/*.jsonl`
- `.codex/sessions/**/rollout-*.jsonl`

原文照传，**不做任何脱敏或解析**。

**键结构**：与源目录一致，前缀是 `agent_workdir/{员工}/data_collect/`：

```
agent_workdir/work1/data_collect/.claude/projects/{project}/{session}.jsonl
agent_workdir/work1/data_collect/.codex/sessions/{y}/{m}/{d}/rollout-....jsonl
```

即 `DataCollectKey(user, rel)` = `DataCollectPrefix(user)` + 该文件相对 profile 目录的路径（`.claude/...` 或 `.codex/...`），大小写与目录层级原样保留。

**开关字段**（`policy.json`，见 §4.4）：

| 字段 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `collectEnabled` | bool | `false` | 总开关 |
| `collectQuietSeconds` | int，可选 | `60` | 去抖：文件最近一次修改要满这么久才采，避免正在写的会话被采半截 |
| `collectSince` | string，可选，`YYYY-MM-DD` | 空 | 只采这天（按 **UTC** 日期比较）及之后修改的文件；空 = 采全部历史 |

**启用步骤**（顺序不能反）：

1. 先给 agent 的 RAM 策略加 §6 下面那条 `PutObject` 权限（`dist/ram-policy-agent.json` + 已部署的 RAM 策略都要加）
2. 再把 `policy.json` 的 `collectEnabled` 置 `true`

关闭就是反过来：先把 `collectEnabled` 置回 `false`，权限可以留着也可以之后再收回。

**权限边界**：agent 对 `data_collect/` **只写不读**（拿不到 `oss:GetObject`/`oss:ListObjects`），符合 §5 的最小权限原则——机器密钥泄露也读不回任何人的对话记录。去重靠**本地 state 文件**（agent 状态目录下的 `collect-state.json`，记录每个源文件的 mtime+size），不依赖读 OSS 反查；本地源文件被删除后，该文件会从 state 里移除，但已上传的 OSS 副本不会被跟着删除。

**何时跑**：作为 `agent.exe` 同步周期里的一步，只在 `collectEnabled=true` 且机器已绑定且绑定员工在本机有 profile 时才跑；节奏跟着同步间隔走，不单独计时。

**验证**：跑一次 `agent.exe sync`，输出会有一行 `collect=<true/false> uploaded=<N>`；机器写回的 `agent_workdir/_status/{主机名}.json`（即 `admin.exe status` 读取的那个对象）里也带 `collectEnabled` / `collectUploaded` 两个字段，只是当前的汇总表格没有为它们单开列；到 OSS 控制台确认对象确实出现在 `agent_workdir/{员工}/data_collect/` 下。

**为什么在每个员工目录里，而不是一个共享的 `data_collect/`**：

- **按人清理** —— 员工离职，删 `agent_workdir/work1/` 一个目录，策略、凭据、对话记录一起没了，不用再去别处翻
- **按人设生命周期** —— 交互数据体量远大于配置，要设 OSS 生命周期规则时，一个员工一个前缀最好写
- **按人授权** —— 将来如果要做"某个负责人只能看自己组的数据"，前缀就是现成的边界
- **一致性** —— 属于某个员工的东西都在他自己的目录下，不用记住"这个例外放在别处"

**为什么在 `agent_workdir/` 内而不是和它平级**：这些数据是 **agent 写的**，属于 agent 的活动范围。`admin/` 那边只放管理员自己维护、agent 永远碰不到的东西。

代码已实现，权限授权仍是手工的一步。**启用要给 agent 加一条**（`dist/ram-policy-agent.json` 默认不含它，见 `dist/部署说明.md`）：

> ```json
> {
>   "Effect": "Allow",
>   "Action": ["oss:PutObject"],
>   "Resource": ["acs:oss:*:*:your-bucket-name/agent_workdir/*/data_collect/*"]
> }
> ```
>
> **只给写，绝不给读**——一台机器的密钥泄露就等于所有员工的对话记录泄露，这个的敏感度比配置和凭据高一个量级。
>
> 这条权限在功能启用前不要提前加。

---

## 7. `_agent/`：agent 自更新（默认关闭）

管理员发布新版 agent 时,把二进制放到 `agent_workdir/_agent/agent.exe`,并在 `policy.json` 里记下目标版本和该文件的 SHA-256:

| 字段(policy.json) | 含义 |
|---|---|
| `agentUpdateVersion` | 目标版本。**空 = 不更新**(默认)。设了值,自身版本不同的 agent 才会更新 |
| `agentUpdateSHA256` | `_agent/agent.exe` 的十六进制 SHA-256,agent 换之前必须比对一致 |

**agent 侧流程**:每个 sync 周期,若 `agentUpdateVersion` 非空且 ≠ 自身版本 → 下载 `_agent/agent.exe` → **校验 SHA-256**(不一致就不换,并在 status 报错)→ 换文件(旧的存 `agent.exe.old`)→ 重启服务。**同一目标只尝试一次**(本地打标记),坏包不会反复重启。

**权限**:agent 对 `_agent/*` 只需 **GetObject**(读)。`dist/ram-policy-agent.json` 已含这条。**只给读,绝不给写**——给了写就等于任何一台机器都能往这里塞一个所有机器都会执行的二进制。

**风险与边界**(见 `操作手册`):`policy.json` 全局 → **所有机器一起更新**,无灰度;**无自动回滚**。所以:**新 agent 先在一台机器上手动验证能跑,再 `admin agent publish`**;出事用 `admin agent cancel` 急停,并手动把 `agent.exe.old` 换回。

---

## 7b. `_codex/`：Codex 桌面版分发（默认关闭）

管理员用 `admin codex publish` 把重打包的 Codex 安装器放到 `agent_workdir/_codex/codex-setup-{版本}.exe`,并在 `policy.json` 里记下目标:

| 字段(policy.json) | 含义 |
|---|---|
| `codexVersion` | 目标版本。**空 = 不分发**(默认)。**按相等比较**:机器上已装版本 ≠ 此值就安装此值 |
| `codexSHA256` | 安装器的十六进制 SHA-256,装之前必须比对一致 |
| `codexKey` | `_codex/` 下的对象键 |
| `codexRolloutPct` | 灰度比例 0–100。机器满足 `crc32(主机名) % 100 < 此值` 才更新 |

**为什么按相等而不按新旧**:把 `codexVersion` 改回旧版本**就是回滚**,agent 会把旧版装回去。"只升不降"的逻辑做不到这一点,而坏包正是需要往回退的时候。这与 §7 的 `agentUpdateVersion` 语义一致。

**为什么按版本存而不是覆盖同一个键**:安装器约 700 MB,留存旧版意味着回滚只是改一个策略字段,不必重新上传。

**与 §7 的关键差异**:agent 自更新是**全量**下发,Codex 分发有**灰度**。因为这个包是对上游 Codex 的重打包,补丁会随上游版本漂移,CI 通过只能证明补丁正确应用,**不能证明 ChatGPT 入口真的消失或 Computer Use 仍可用**——那只有 Windows 真机能验。所以 `admin codex publish` 的 `--rollout` 默认只有 **10%**。

**权限**:agent 对 `_codex/*` 只需 **GetObject**(读)。**只给读,绝不给写**——理由同 §7。

**尚未实现**:agent 侧的下载与安装。设计见 [Codex分发方案](Codex分发方案.md)，其中两个前置改造(安装器改机器级、包版本区别于 Codex 上游版本)未完成前，发布出去也装不上。

---

## 8. 改布局时的检查清单

布局是 agent 和 admin 之间的契约，两边任一侧单独改都会导致"写进去了但没人读"。改动时：

- [ ] 改 `go/internal/ossclient/client.go` 的 `Root` / `AdminRoot` / 前缀常量 / key helper
- [ ] `admin/` 与 `agent_workdir/` 必须保持平级、互不嵌套（有测试守着），否则一条通配授权会连带覆盖另一个
- [ ] 确认所有调用方都走 helper，没有手工拼接的路径（`admincore` 曾经犯过这个错）
- [ ] 跑 `go test ./...`，`ossclient` 和 `admincore` 里有守这条契约的测试
- [ ] 更新本文件
- [ ] 更新 `dist/ram-policy-agent.json`
- [ ] 已部署的环境：RAM 策略要先加新路径、等所有 agent 升级完，**再撤旧路径**——顺序反了会让机器集体失联
