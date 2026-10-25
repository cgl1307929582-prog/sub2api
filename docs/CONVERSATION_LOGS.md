# 对话正文记录

管理员在「使用记录」页面打开「对话记录设置」，启用后，新调用的「对话详情」可查看最近一条用户提问和模型可见回复。请求错误详情也提供查看入口。历史调用无法补录。

- 安装默认关闭，默认保留期限为 7 天；后台可选 7、30、90 天或一直保留。设置 API 支持 0–3650 天，0 表示一直保留。
- 普通 HTTP、SSE 流式及 Responses WebSocket 分轮记录均受同一开关控制。支持 Messages、Chat Completions、Responses、Gemini generateContent/streamGenerateContent 的文本内容。
- 不复制 system/developer 提示词、隐藏思考、工具结果、鉴权头或附件二进制。附件显示类型占位符。每次调用只保存最近一条有可见内容的 user 消息，不重复保存完整历史对话。
- 每侧正文最多 1 MiB；超限显示截断标记。读取请求的捕获上限为 8 MiB，超限且无法解析时不保存提问正文。回复中断时保留已捕获的文本并显示未完成。
- 仅管理员接口能读取正文，详情响应禁止缓存；页面将内容作为纯文本展示。
- 正文保存在独立的 `conversation_logs` 表，通过请求 ID、用户 ID 和 API Key ID 关联调用。保留期清理每小时执行，最多有一小时延迟，不删除原有用量、计费或错误记录。
- 写入使用异步有界队列（128 条、32 MiB），数据库故障或队列满时不阻塞模型调用。设置区域显示丢弃及写入失败计数；这些计数在进程重启后重置。

管理员接口：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / PUT | `/api/v1/admin/usage/conversation-config` | 读取或修改开关、保留天数 |
| GET | `/api/v1/admin/usage/:id/conversation` | 按用量记录 ID 查看正文 |
| GET | `/api/v1/admin/usage/conversation-errors/:id` | 按请求错误 ID 查看正文 |

修改配置同时提交 `enabled` 和 `retention_days`。详情未记录、已过期或调用发生在未支持的接口时返回 404。
