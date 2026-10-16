# Droid / Factory 平台适配

基线：官方 `Wei-Shaw/sub2api`，提交 `b8cfb7884084393e08f0e2c4027afc3755357b98`（2026-10-16，VERSION 为 `0.2.16`）。
维护仓库：`https://github.com/cgl1307929582-prog/sub2api`。
平台 ID 为 `droid`，界面名称为 **Droid / Factory**。

## 已实现

- 账号创建、编辑、筛选与分组中的独立平台入口，Factory API Key 密码字段及后端校验。只支持 API Key，不实现 OAuth/refresh token。
- 共用 sub2api 的网关鉴权、分组、账号调度、并发限制、配额、计费、错误状态与临时冷却。
- 模型目录和默认协议在平台 profile 中定义，生成前端清单。后台测试模型选择、公共 `/v1/models`、白名单选择器均有 Droid 候选模型；支持自定义映射。
- Factory 请求头在统一出站边界生成；每次请求独立生成 session/message ID，删除调用者认证、Cookie 与会话标识，并禁止 HTTP 重定向。
- 默认客户端版本为 `0.242.0`，同时设置 User-Agent 与 X-Client-Version；显式覆盖 `factory-cli/<version>` 时同步两者。三个协议均在调用者指令之前补入 Factory 要求的 Droid 身份前缀，重复准备请求不会重复添加。原有系统内容块、缓存标记、工具参数、大整数和 signed thinking 保持原值。
- 管理端 Anthropic 账号测试使用 Droid 身份提示词。通用测试正文中的 Claude Code 身份提示词会导致 Factory 返回 403，即使前面已加入 Droid 身份；因此只替换该测试自动生成的身份，不改写网关调用者的系统指令。已用同一服务器、Key、`claude-opus-4-6` 对照确认这一差异；Key 和模型可用时也可能因测试正文不兼容而报 403。
- 401 使用现有 API Key 失效处理；429 优先采用 `Retry-After`，缺失时使用现有可配置的短时冷却。连接拒绝、DNS、代理认证等持久故障保留冷却分类，但丢弃可能含凭据的错误原文。
- 响应正文、SSE 和响应头中的完整 API Key 回显会被替换；正文规范等价的 ASCII JSON 转义，错误体和 SSE 错误帧进一步递归处理嵌套 JSON 字符串。SSE 完整行及时输出；错误 JSON 上限 1 MiB/16 层，超限使用固定安全错误，SSE 单行超过 1 MiB 则安全中止。
- Droid 保留调用者取消上下文。已经输出部分流后不换账号重放；跨协议 Anthropic 转换必须收到 `message_stop`，截断、错误事件和非法 JSON 不再伪造成功终态。Responses 转换失败发送保留响应 ID/事件序号的 `response.failed`；排队心跳之后的本地预检错误也使用对应 SSE 格式。
- 不新增数据库迁移。该基线已有迁移 `242_drop_platform_check_constraints.sql`，平台约束改由应用登记表统一校验。这里没有连接或迁移任何生产数据库。

## 路由与模型

| 上游模型族 | Factory 路径 | X-API-Provider | 首选入站 |
|---|---|---|---|
| `claude-*` | `/api/llm/a/v1/messages` | `anthropic` | Messages |
| `minimax-*` | `/api/llm/a/v1/messages` | `fireworks` | Messages |
| `gpt-*` | `/api/llm/o/v1/responses` | `openai` | Responses |
| `grok-*` | `/api/llm/o/v1/responses` | `xai` | Responses |
| `gemini-*` | `/api/llm/g/v1/generate` | `google` | Chat / Responses / Messages，经内部转换 |
| `mistral-*` | `/api/llm/o/v1/chat/completions` | `mistral` | Chat Completions |
| `glm-*`、`kimi-*`、`deepseek-*`、`qwen*`、`nemotron-*`、`inkling` | `/api/llm/o/v1/chat/completions` | `fireworks` | Chat Completions |

默认主机为 `https://api.factory.ai`。上游路径与模型族须匹配；未支持的模型族在发送前拒绝，不按上游故障轮换账号。高级地址配置沿用平台现有校验，不另开安全豁免。

本地候选目录：`claude-sonnet-4-6`、`claude-opus-4-6`、`claude-haiku-4-5-20251001`、`gpt-5.4`、`gpt-5.4-mini`、`gpt-5.3-codex`、`glm-5.3-flash`、`glm-5`、`kimi-k2.5`、`minimax-m2.5`。新增候选包含 Gemini 3 Flash、3.1 Pro、3.5–3.8 Flash，Grok 4.5–4.7，DeepSeek V4 Flash 0731、V4 Pro、V4.1 Flash，Mistral Large 4、Medium 3.5，Qwen3.8 Max、Nemotron 3 Ultra 和 Inkling。默认连接测试使用 Haiku 4.5。

在创建或编辑 Droid API Key 账号的模型白名单中点击 **同步上游支持的模型**，会实时读取 [Factory 官方模型目录](https://docs.factory.com/models) 的 Markdown 版本 `https://docs.factory.com/models.md`，增量加入当前适配器可转发的模型，保留已有条目和自定义映射。保存账号后白名单生效；编辑页面显示最近同步时间。以后官方目录添加同协议模型时，再次同步即可获取，无需更新代码中的默认清单。

成功同步的目录保存在账号 `extra.droid_model_catalog`，用于下拉选择、管理端账号测试和公共 `/v1/models`（仍受账号映射和分组限制）。每次点击重新获取；没有后台定时同步。请求或解析失败时保留上次目录和映射，未同步账号使用上述本地默认候选。已配置代理但代理不可用时查询失败，不静默直连；查询遵守服务器 URL 白名单、20 秒超时和 1 MiB 响应上限，不跟随重定向，且不会把 Factory Key 发给文档主机。

**这是官方公开目录，不是该 Key 的模型权限清单。** Factory 当前标准 `/models` 路径未提供可用列表；具体权限、地区、额度和即时可用性仍需账号测试确认。同步只纳入上表支持的模型族，未知协议和图像生成模型仍会显示排除数量，不宣称可转发。只读取官方名称和 reasoning 档位，不推测上下文、价格或输出上限。本地配额和 token 计费与下面的 Factory 套餐额度分别记录。

## Gemini 与其他新增模型

Factory 的 Google 端点始终返回 SSE，并拒绝请求体中的 `stream` 字段。传输层将共享 Chat 请求转换为 Google 请求，再将流式片段转换成 Chat 格式；普通响应会聚合成 JSON。三种入站与管理端账号测试共用这一转换，不需要重建账号。Droid 的固定模型协议优先于旧账号保存的协议规则，避免新增 Grok 被旧规则送入 Chat 端点。

- Gemini 函数声明使用 `parametersJsonSchema`，工具参数保持 JSON 大整数；工具结果按函数名配对。`thoughtSignature` 编入带识别标记的工具调用 ID，随 Chat、Responses、Messages 往返，不依赖进程缓存或伪造签名。客户端需原样保留工具 ID。
- Gemini 支持文本、内联媒体输入、输出 token 上限、温度、停止序列、JSON 输出和函数选择。媒体 URL 只转交上游，网关不自行抓取。推理档位映射为 Google thinkingLevel；不支持完全关闭思考的模型用最低支持档位。
- Gemini 输出计费包含 candidatesTokenCount 与 thoughtsTokenCount，缓存读取单独保留。必须收到有效 finishReason 才发送成功终态；错误、断流、畸形及超大 SSE 帧不会补造 `[DONE]`。
- Mistral 使用 `mistral` 供应商，只接受 high/none：启用思考的通用档位映射为 high，off/none 映射为 none。Large 4 的 text/thinking 数组转换为正文和 reasoning_content，包含流式 delta；Inkling 与 Fireworks 的思考 token 用量转换为标准字段。
- 本次适配记录上游 token，用量价格仍使用部署方的 Sub2API 价表。没有价格条目的新模型可能记为 0 美元（本地验收中的 Mistral 即如此），不能据此判断 Factory 免费；用于对外计费前应配置并验证对应模型的自定义价格。Factory 目录倍率不自动换算为美元单价。
- 2026-10-16 原生请求中，旧 `deepseek-v4-flash-0731`、`deepseek-v4-pro` 均返回 `accounts/fireworks/models/deepseek-v4p1-flash`。保留可调用 ID，不保证弃用别名对应旧模型；精确选型请使用当前 ID，并核对上游返回。

签名处理依据 [Google thought signatures](https://ai.google.dev/gemini-api/docs/thought-signatures)；Factory 路径、供应商和返回格式以实际请求验证。此处没有复制第三方适配器实现。

## Factory 套餐额度与额外余额

账户页会使用该账号的 API Key 读取同一配置主机的 `GET /api/billing/limits`，显示 Standard 与 Core 各自的 5 小时、7 天、30 天已用比例和重置时间，以及 `extraUsageBalanceCents` 对应的 USD 额外充值余额。百分比额度不换算为美元或剩余 token；套餐未返回的字段显示未知，真实 0 余额显示 `$0.00`，无活动窗口不会伪造重置时间。

- 打开账户页按需读取，可点击“刷新 Factory 额度”。桌面端使用现有批量查询，移动端沿用可见区域加载。页面缓存最长 5 分钟，服务端成功缓存 3 分钟、错误缓存 1 分钟，手动刷新最短间隔 5 秒；并发请求合并。缓存仅在进程内，容量上限 1024 项，重启后重新查询。
- 缓存绑定账号及凭据/地址/代理配置摘要，更换 Key 不复用旧额度。失败保留最后成功快照、原查询时间和失败标记；成功响应明确缺失的值清为空，不把未知当 0。查询失败不改变账号的推理状态。
- 查询走现有出站 HTTP 传输、账号代理及主机白名单；不跟随重定向。配置的代理不可用时返回查询失败，不静默直连；不回显上游原始错误、认证头或 Key。
- 此功能只读取额度，不修改 Extra Usage 开关、充值、套餐或 Factory 重置时间。429 调度仍优先服从当前请求的 `Retry-After`，不依据某个池的百分比禁用整个账号。

2026-10-16 已用授权 Key 通过实际 `AccountUsageService`、`DroidQuotaService` 和仓库 HTTP 传输读取到两个套餐池、额外充值余额及重置时间，并确认再次查询复用缓存；此验收没有发送模型生成请求。表单显示和单账号/批量查询走自动测试验证，没有代用户确认管理控制台的部署承诺。`/api/billing/limits` 是当前实测可用的 Factory 接口，未来接口或不同账号权限变化会按查询失败/未知处理。

套餐窗口含义参考 [Factory Individual Plans](https://docs.factory.com/pricing/individuals)。

## 协议边界

同协议转发保留原有工具、thinking 和 usage 语义。其他入站复用 sub2api 转换器：

- Responses ↔ Claude 支持工具调用及结果。签名 thinking 使用原有 `anthropic-thinking-v1:` envelope，显式开启 4.5/4.6 系列保留。它是可逆编码，并非新加密；客户端须原样回传 `encrypted_content`，签名最终由上游验证。
- 转换时显式 `none/minimal` reasoning 不开启手动 thinking；thinking 预算须小于输出上限。上限过小（≤1024 且请求需要更高 thinking 预算）返回 400，不擅自提高输出或计费上限。
- 生成 Anthropic 请求时按现有 Claude 能力目录处理 effort。Haiku/Sonnet 4.5 保留手动 thinking 预算，但不发送它们不支持的 `output_config.effort`。
- Chat Completions 不具备 Claude 签名续轮字段。Claude 的显式中高 reasoning 或已携带 reasoning 历史会返回 400，提示使用 Messages 或 Responses。不要宣称这种转换无损。
- Gemini 已可经 Chat Completions、Responses 和 Messages 使用；没有为 Droid 增设 Google 原生 `/v1beta` 入站。图像生成、音频、视频、Factory refresh-token 和 CLI/SDK 子进程桥接不在本实现范围。
- usage 基于上游已收到的数据；取消或半流可能没有最终 output usage，不推算为真实最终用量。

## 本地验证与构建

所需工具版本取自此基线：Go 1.27.2（Go 自动工具链）、Node、pnpm 9。锁文件未改动。

```sh
cd frontend
npx --yes pnpm@9.15.9 install --frozen-lockfile --ignore-scripts
npx --yes pnpm@9.15.9 run build
cd ../backend
go test -tags unit ./...
CGO_ENABLED=0 go build -tags embed -trimpath \
  -ldflags='-s -w -X main.Version=0.2.16-droid.5 -X main.BuildType=source' \
  -o bin/sub2api-droid ./cmd/server
```

前端构建写入 `backend/internal/web/dist`，嵌入构建须在前端构建后执行。测试包含临时 localhost HTTP 服务与 miniredis，需要允许本机监听端口，不依赖生产数据库或真实 Factory 凭据。

专门回归：

```sh
cd backend
go test -tags unit ./internal/service ./internal/handler ./internal/handler/admin -run '^TestDroid' -count=1
go test ./internal/pkg/apicompat
```

自动测试覆盖协议矩阵、管理端三种协议测试、工具往返、签名 thinking、预算、usage 与缓存计费、401/429换号、冷却、本地预检、排队后的 SSE 错误、半流终态、分块/转义脱敏和 localhost HTTP 取消/重定向行为。

2026-10-16 另使用临时 PostgreSQL、Redis 和嵌入前端的 Sub2API 二进制进行了真实 Factory 验证：

- Haiku 4.5、GPT-5.4-mini、GLM-5.3-flash 各自的原生普通和流式请求：6 项通过。
- Chat → Claude 普通、Responses → Claude 流式、Messages → GPT 普通、Chat → GPT 流式、Responses → GLM 普通、Messages → GLM 流式：6 项通过。
- Claude 工具调用与提交工具结果后的续轮：2 项通过。
- 登录成功，无 Key 请求被拒绝，带 Key 的 Droid 模型目录正常；14 次成功生成均写入 usage 记录。

同日新增 17 个模型通过完整 Sub2API 实例的验收：每个型号分别测试 Chat Completions、Responses、Messages 的普通与流式输出，共 102 项；另外 30 次工具调用及结果续传请求通过，包括 Gemini 六个型号及三种入口。验收要求 HTTP 200、正文 OK（允许结尾标点）、流式成功终态且无错误；初次 Mistral reasoning 400 已修复复测。Mistral Medium 3.5 在 auto 模式下曾再次调用工具，续轮显式 none 后正确返回工具结果。用量写入临时 PostgreSQL，服务与凭据数据库完成清理。

只测试列出的模型与能力；套餐权限与内部接口可能变化。真实 Key 从仓库外读取，未写入源码、测试夹具或公开报告。账号表单及管理端连接测试使用自动化测试覆盖；真实链路使用临时数据库夹具，不代替用户确认管理控制台中的部署承诺。

## 启动、安全交接与回滚

临时测试实例应使用自己的数据库和 Redis。生产发布需要先记录当前镜像与配置，并备份数据库和数据目录。

先确认独立测试实例及其专用 PostgreSQL/Redis 配置。使用该基线原有的配置/初始化流程，完成实例配置后可从其工作目录运行构建产物；避免将新二进制指向未经确认的旧生产数据库，因为启动可能应用基线已有迁移。需要 Docker 部署时，在已确认的环境以本分支源码构建原有 Dockerfile。

真实验收的最少交接步骤：

1. 确认目标实例地址、数据库和配置位置，以及是否允许保存测试账号和发起少量计费调用。
2. 用户直接在该实例管理界面的 **账号 → 添加 → Droid / Factory → API Key** 密码框输入 Factory key；留空编辑保留原值。不要将 key 放进聊天、命令行参数、源码、日志或测试夹具。
3. 创建/选择 Droid 测试分组，分别运行 Claude Messages 与 GPT Responses 测试，再进行工具续轮、流式取消和少量计费对账。模型权限以实际返回为准。
4. 确认上述结果后，再单独批准生产部署/重启。禁止把本机 mock 通过当作此项已完成。

应用回滚先恢复上一版二进制/镜像，保留账号与用量记录。模型目录同步前另备份账号配置，回滚时检查旧版是否支持新增模型。数据库回退需按部署前备份与实际迁移记录单独处理，不在此文提供自动破坏性 SQL。

## 来源与许可

实现依据公开的 Factory HTTP 路径/协议观察自行编写，参考 `https://github.com/1e0n/droid2api`。未复制该项目代码，未复用其 refresh-token 日志或无网关鉴权做法。未复制保留所有权利的 `Sibuxiangx/nicedroid`，未引入 `mrwogu/factory-droid-openai` 的 SDK/CLI 进程桥接。改动沿用 sub2api 仓库的 LGPL-3.0 许可与其原有许可文件。
