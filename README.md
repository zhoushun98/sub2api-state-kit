# Sub2API STATE Kit

> 本仓库是 [wangyunjeff/sub2api-state-kit](https://github.com/wangyunjeff/sub2api-state-kit) 的 fork，在原版 0.1.0 之上追加了下面两项改造；构建好的镜像发布在 Docker Hub `zhoushun98/sub2api`。原作者的说明从「相比上游增加了什么」一节起原样保留。

## 本 fork 的改动（2026-09-19）

### 0.1.1：账号级多模型票据

- 每个账号可同时勾选 `gpt-6-astra` 与 `gpt-5.6-sol`，各自独立采集、经固定代理复验、注入；已勾选但缺票的模型只暂停该模型到本账号的调度，其他模型不受影响。
- 采集任务与票据状态按「账号 × 模型」拆分；`GET / PUT /api/v1/admin/accounts/:id/codex-ticket` 新增 `models` 与 `model_statuses`，仍兼容单个 `model` 字段。
- 仅增删模型时保留未变模型的票据与 revision；「重新获取」会把所有已选模型重采一遍。

### 0.1.2：新账号默认开启 STATE

- 「系统设置 → 网关服务 → Codex 设置」新增「新账号默认开启 STATE」、默认套餐、默认目标模型。
- 只认领在开启该默认之后创建、已绑定固定代理、且从未手动配置过票据的 OpenAI OAuth 账号（含导入、复制）；存量账号与手动关闭过的账号不受影响；未绑代理的新账号绑上后才自动开启；关闭再开启会重新计时。
- 设置键：`openai_codex_ticket_default_enabled` / `_enabled_since` / `_plan` / `_models`。

### 镜像与构建

- `zhoushun98/sub2api:state-kit-0.1.2-defaults`（linux/amd64，含以上全部改动）
- `zhoushun98/sub2api:state-kit-0.1.1-multimodel`
- 构建：`python3 scripts/prepare.py ../sub2api-state-source && cd ../sub2api-state-source && docker build --build-arg VERSION=0.2.6-state-kit.0.1.2-defaults -t sub2api:state-kit-0.1.2-defaults .`

### 验证

- 后端：`go test ./internal/service ./internal/handler/admin -run 'Codex|codex|Setting' -count=1` 与 `go test -tags unit ./internal/server -run TestAPIContracts`
- 前端：`vitest run CodexAccountTicketSettings SettingsView localeKeyCompleteness`、`vue-tsc --noEmit`、`pnpm run check:i18n`
- 两次改动的补丁见 `docs/patches/`，对应 `overlay/` 与 `UPSTREAM.json` 已重新生成（56 个覆盖文件）。

---


首先感谢 **[gylive/ccodex-sleep-state](https://github.com/gylive/ccodex-sleep-state)** 的思路分享，也感谢群里各位大佬在讨论、测试和排查中的帮助！这个扩展是在 [Sub2API](https://github.com/Wei-Shaw/sub2api) 的基础上折腾出来的，离不开原项目和大家的经验。

为 Sub2API 增加 **账号级 STATE 管理、Pro / Team 选择和异常动态守护**。可以逐个账号编辑、开启或关闭：只对需要处理的账号启用，正常账号继续按原流程使用。

代码免费开源，欢迎使用、交流与改进。如果这个项目对你有帮助，希望顺手点一个 **Star ⭐**。

这是基于 Sub2API **v0.2.6** 的非官方实验扩展，目前以精简源码覆盖包发布，不是可直接安装的插件，也不包含预先配置好的账号或代理。

## 相比上游增加了什么

对比基线为 [Wei-Shaw/sub2api v0.2.6](https://github.com/Wei-Shaw/sub2api/tree/49a39b6dc1abed30fd227611e8af1108bc427610)，不代表上游后续版本的能力。

| 功能 | 上游基线 | 本扩展 |
| --- | --- | --- |
| 启用入口 | 全局 STATE 开关与策略配置 | 保留总开关，增加**账号级入口和独立开关**，可以逐个账号编辑 |
| Pro / Team | 全局目标长度默认 292，可修改配置 | 账号界面明确提供 **Pro（292）/ Team（332）**，每个账号单独选择 |
| 异常处理 | 本扩展基于其原有票据机制扩展 | 增加动态守护，处理成功响应中的模型不符或 **312 字节 STATE** 信号 |
| 代理与验证 | 基于上游采集机制 | 全局动态池采集，同账号固定业务代理复验；日常请求继续用固定代理 |
| 使用状态 | 原有票据摘要 | 账号展示可用时间、续期、失败冷却及守护触发摘要 |

“Team 支持”指增加手动套餐选择及对应长度筛选，并不是自动读取或修改订阅。**长度只作实验筛选，不能单独证明模型身份或回答质量。** 312 也只是本实现采用的实验异常信号，不是上游官方确认的撤销协议。

## 使用前后，看图更直接

下面是这次更新的 **Team / Pro 原图**：原图上半部分为启用 STATE 后的请求，下半部分为未启用 STATE 的请求。保留原图的完整界面和上下顺序，用红框标出模型列，并标注“已启用 STATE / 未启用 STATE”。

未启用时，日志显示请求 `gpt-6-astra`，上游响应为 `gpt-5.6-luna`，并标记“模型不一致”；启用后，这组截图里不再出现该标记。原图不缩放、不裁剪、不重排，顶部另加 Team / Pro 大标题，并添加红框、文字与必要的隐私遮挡；模型字段和请求记录顺序未改动。

### Team 对照

![Team 原图：完整界面，上方启用 STATE、下方未启用并显示上游 Luna 与模型不一致](docs/images/team-state-routing.png)

### Pro 对照

![Pro 原图：完整界面，上方启用 STATE、下方未启用并显示上游 Luna 与模型不一致](docs/images/pro-state-routing.png)

## 设置顺序：先全局，再单个账号

### 第一步：打开全局 STATE 票据总开关

先进入「系统设置 → 网关服务 → Codex 设置」，打开下图红框中的 **STATE 票据总开关**。这是**全局开关**；仅打开它不会自动为所有账号启用 STATE。

![全局 STATE 票据总开关：先打开红框中的开关，再编辑单个账号](docs/images/global-state-settings.png)

*原图仅叠加红框，未裁剪、缩放或修改其他内容。*

在下方「全局动态 IP 池」填写完整代理 URL 并保存，格式见下文「使用前准备：动态 IP」。

### 第二步：编辑单个账号并启用 STATE

再进入「账号管理 → 编辑账号」，编辑需要启用的 OpenAI OAuth 账号：先保存固定业务代理，再重新打开编辑窗口，在「STATE 票据」区域选择 Pro / Team，打开**该账号的 STATE 开关**并点击该区域的保存。这里的设置只针对当前账号，需要使用的账号要分别开启。

启用后，可以查看票据剩余时间和动态守护状态，也可以手动重新获取。

![账号级 STATE 设置：独立开关、套餐选择、票据有效时间与动态守护](docs/images/account-state-settings.png)

*图中代理地址已隐藏，其余保留原始界面。*

## 使用前准备：动态 IP

**使用这套 STATE 采集功能，需要先准备一个动态 IP 代理池。** 如果还没有，可以通过下面的邀请链接了解和开通：

👉 **[1024proxy 动态 IP（作者邀请链接）](https://api.1024proxy.com/share/uiym4vcvd)**

拿到代理连接信息后，在「系统设置 → 网关服务 → Codex 设置 → 全局动态 IP 池」填写完整代理 URL，SOCKS5 代理使用以下格式：

```text
socks5h://ENCODED_USERNAME:ENCODED_PASSWORD@HOST:PORT
```

将 `ENCODED_USERNAME` 和 `ENCODED_PASSWORD` 替换为分别经过 URL 百分号编码的代理用户名和密码，将 `HOST` 和 `PORT` 替换为代理服务器地址和端口。只编码用户名和密码，不要把整个 URL 一起编码；例如用户名或密码中的 `@`、`:`、`#`、`%` 分别写成 `%40`、`%3A`、`%23`、`%25`。`socks5h` 表示通过代理解析目标域名。

程序支持自动更新代理用户名里的 `{sid}`，也适配了 1024proxy 的 SID 格式，不用每次手动生成、导入一堆 IP。

动态 IP 用于采集 STATE，日常业务请求仍走各账号原来的固定代理。已有兼容的动态代理服务也可以继续用，不必重复开通；代理服务费用由服务商收取，项目源码仍然免费。

## 工作方式

1. 在系统设置中开启总开关，配置全局动态代理池。
2. 编辑指定 OpenAI OAuth 账号，保存固定业务代理，选择 Pro 或 Team 并开启该账号的 STATE 功能。
3. 使用**同一个账号**采集候选 STATE，再通过该账号固定业务代理复验。两次完整成功响应的实际模型都须匹配目标模型，才保存使用。
4. 业务成功响应触发模型不符或 312 信号时，守护程序作废本次使用的票据并重新采集。旧请求不会作废更新后的票据，并发异常会合并处理。

不会跨账号转移票据，也不会修改响应模型名称、伪造结果或重放已完成的业务请求。开关默认关闭，正常账号不必启用。支持 HTTP/SSE/JSON，以及启用账号的 WebSocket HTTP 桥接。

本地有效期 60 分钟、提前 10 分钟续期；每轮最多 8 次、失败冷却 5 分钟。续期失败保留尚未过期的旧票据。401 / 403 / 429 会终止本轮，不继续轮换出口尝试。

## 获取完整可构建源码

需要 Python 3 和 Git。在本仓库目录执行：

```bash
python3 scripts/prepare.py ../sub2api-state-source
cd ../sub2api-state-source
```

脚本下载固定的上游提交，验证原文件和覆盖文件的 SHA-256，再写入**全新目录**。拒绝覆盖现有源码。输出包含完整上游源码及本扩展；不会复制任何本地配置、数据库或原仓库 Git 历史。

后续可按照生成源码中的上游文档编译，或在生成源码根目录构建镜像：

```bash
docker build --build-arg VERSION=0.2.6-state-kit.0.1.0 -t sub2api-state-kit:0.1.0 .
```

构建依赖和资源要求沿用固定的上游版本。具体操作见 [部署说明](docs/deployment.md)、[使用说明](docs/usage.md) 和 [验证范围](docs/validation.md)。本仓库的 `overlay/` 是实际修改源码，`UPSTREAM.json` 固定基线和文件校验值。

## 使用边界

这是实验功能，无法保证上游持续接受 STATE，也不能承诺恢复某个模型或回答质量。路由观测使用原始成功响应的模型字段，不能代替能力评测。Team 分支的自动化验证使用模拟响应；上面的 Pro / Team 图片是使用者提供的界面对照。

当前采集锁为进程内实现，多实例可能重复采集；没有生产负载或完整上游有效期保证。本扩展不新增数据库迁移，但升级上游版本仍需单独评估兼容性。

发布包包含必要源码、测试、说明及经过隐私遮挡与标注的原始界面对照图，不提供账号、真实 STATE、代理凭据、IP 清单、数据库或原始日志。

## 感谢

- 感谢 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 提供基础项目。
- 特别感谢 [gylive/ccodex-sleep-state](https://github.com/gylive/ccodex-sleep-state) 带来的生命周期管理与状态展示思路参考。
- 感谢各位群友在讨论、测试和排查过程中的帮助与反馈。

参考项目的说明与版本记录见 [NOTICE.md](NOTICE.md)。本项目不代表上述项目的官方版本或背书。

## 免费开源与配置帮助

### 💬 作者微信：`wangyunjeff`

> **需要帮忙配置？微信搜索 `wangyunjeff`，添加时备注「Sub2API」。**
>
> 请一杯奶茶，我帮你配置一下 ☕

**代码免费开源，自己部署和使用不收费。** 配置协助与开源代码是两回事，不购买协助也能使用完整源码。

也欢迎通过 GitHub Issues 交流使用问题，发布问题时请先移除账号、密钥、代理密码和真实 STATE。

喜欢的话，欢迎点一个 **Star**，也感谢你把它分享给有需要的朋友。

## 许可证

遵循上游 GNU LGPL v3，详见 [LICENSE](LICENSE)。保留上游版权与署名；覆盖文件是在上游基础上的修改或本扩展新增文件。
