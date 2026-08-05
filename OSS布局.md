# OSS 布局规范

> **本文件是 OSS 结构的唯一权威来源。**
> 其他文档（`操作手册.md`、`实施计划.md`、`架构说明.md`、`dist/部署说明.md`、`README.md`）涉及 OSS 路径、对象格式、RAM 权限时，一律以本文件为准，不再各自重复描述。
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
    ├── work1/                           按【员工】分目录
    │   ├── credentials.zip              该员工的 AI 工具凭据（agent 只读）
    │   └── data_collect/                【预留】该员工与 AI 的交互数据
    │       └── ...                      结构待定，见 §6
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
| 采集数据【预留】 | `agent_workdir/{员工}/data_collect/` | `DataCollectPrefix(user)` | agent | admin |

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
| `errors` | 本轮出的错，逐条文本 |

**agent 只写不读**。管理员用 `admin.exe status` 汇总所有机器。

### 4.4 `agent_workdir/policy.json`

```json
{
  "blockEnabled": true,
  "blockedDomains": ["openai.com", "chatgpt.com", "claude.ai", "anthropic.com"],
  "syncIntervalMinutes": 30,
  "updatedAt": "2026-08-04T10:00:00Z"
}
```

**全局唯一一份**，与员工名单无关。管理员改一次，所有机器下次同步就拿到。

agent **无条件读它、无条件应用**，不看有没有绑定。所以一台刚从镜像开出来、还没分配给任何人的机器，第一次同步就会被封住。

`syncIntervalMinutes` 会被钳制在 **1–1440** 分钟。字段缺失或为 0 时，用 agent 自带配置里的值；再没有就用内置默认 30 分钟。钳制是为了防止误设一个极端值导致机器再也拉不到新配置而失联。

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
| `agent_workdir/{员工}/data_collect/` | 读写 | 暂无（见 §6） |
| Bucket 内其他路径 | 无 | 无 |

agent **明确不能做的**：读员工名单、写任何策略或凭据、删除任何对象、列举 Bucket、访问 `agent_workdir/` 以外的任何内容。

注意 agent 的授权里**根本没有出现过 `admin/`**——不是"允许了又排除"，而是从未授予。

这是"agent 密钥泄露也没关系"这个判断成立的前提——密钥本身在 exe 里是明文的，挡不住有心人，真正限制损失的是这份策略。

---

## 6. `{员工}/data_collect/`（预留，本版不实现）

用于后续采集员工与 Codex / Claude 的交互数据。**当前版本不写任何代码，也不给 agent 任何权限**，只在结构上把位置留出来。

```
agent_workdir/work1/data_collect/
```

**为什么在每个员工目录里，而不是一个共享的 `data_collect/`**：

- **按人清理** —— 员工离职，删 `agent_workdir/work1/` 一个目录，策略、凭据、对话记录一起没了，不用再去别处翻
- **按人设生命周期** —— 交互数据体量远大于配置，要设 OSS 生命周期规则时，一个员工一个前缀最好写
- **按人授权** —— 将来如果要做"某个负责人只能看自己组的数据"，前缀就是现成的边界
- **一致性** —— 属于某个员工的东西都在他自己的目录下，不用记住"这个例外放在别处"

**为什么在 `agent_workdir/` 内而不是和它平级**：这些数据是 **agent 写的**，属于 agent 的活动范围。`admin/` 那边只放管理员自己维护、agent 永远碰不到的东西。

落地前必须先定的事项、以及对权限的影响，见 `实施计划.md` §13。这里只强调一条：

> 采集要落地就得给 agent 加一条：
>
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
> 这条权限在功能落地前不要提前加。

---

## 7. 改布局时的检查清单

布局是 agent 和 admin 之间的契约，两边任一侧单独改都会导致"写进去了但没人读"。改动时：

- [ ] 改 `go/internal/ossclient/client.go` 的 `Root` / `AdminRoot` / 前缀常量 / key helper
- [ ] `admin/` 与 `agent_workdir/` 必须保持平级、互不嵌套（有测试守着），否则一条通配授权会连带覆盖另一个
- [ ] 确认所有调用方都走 helper，没有手工拼接的路径（`admincore` 曾经犯过这个错）
- [ ] 跑 `go test ./...`，`ossclient` 和 `admincore` 里有守这条契约的测试
- [ ] 更新本文件
- [ ] 更新 `dist/ram-policy-agent.json`
- [ ] 已部署的环境：RAM 策略要先加新路径、等所有 agent 升级完，**再撤旧路径**——顺序反了会让机器集体失联
