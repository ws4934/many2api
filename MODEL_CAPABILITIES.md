# 各渠道模型能力与限制

依据代码版本：`3d53b95a`；整理日期：2026-11-04。本文描述 **many2api 当前适配器实际接收、归一化和发送的参数**。固定目录共 52 个“渠道＋模型”条目，另列动态模型的发现规则和代码已识别的 Build 模型。

## 先看这些约定

- **输入图片数**与**输出图片数 `n`**是两回事。视频的首帧、尾帧、关键帧、参考图也分别计数。
- 图片 API 的 `n` 默认 1，归一化到 1–10；流式图片请求固定为 1。下表的“输出 1–10”是 API 请求目标数，可能拆成多次生成，部分失败时不保证补足。
- “支持”表示网关已实现相应通路；账号套餐、上游权限、配额及临时封禁仍决定是否可用。
- 表中模型名不带渠道前缀。同名模型会按路由选择账号；需要明确渠道时可用 `Build/`、`Web/`、`Console/`、`chatgpt/`、`adobe/`、`muse/` 前缀，例如 `adobe/gpt-image-2`。
- 动态模型和自定义别名不能靠静态文档列尽。运行实例的最终可调用列表以当前客户端密钥访问 **`GET /v1/models`** 的结果为准。

来源：[模型名称与路由](backend/internal/domain/model/model.go)、[图片 API 参数](backend/internal/transport/http/inference/image_handler.go)。

## 1. 渠道总览

| 渠道 | Provider ID | 对话接口 | 图片生成／编辑 | 可恢复图片任务 | 视频 | 独立语音接口 |
|---|---|---|---|---|---|---|
| Grok Build | `grok_build` | Responses、Chat Completions、Messages；支持 compact | 不提供 Images API | — | 生成 | 无 |
| Grok Web | `grok_web` | Responses、Chat Completions、Messages | 支持，按模型区分 | 无 | 生成 | 无 |
| Grok Console | `grok_console` | Responses、Chat Completions、Messages | 支持 | 无 | 生成；经典视频模型另支持编辑、延长 | TTS、STT、Realtime |
| ChatGPT | `chatgpt` | Responses、Chat Completions、Messages | 支持 | Web 图片支持；`codex-*` 图片不支持 | 无 | TTS、STT |
| Adobe Firefly | `adobe` | 无 | 支持 | 支持 | 生成；部分模型支持编辑、延长 | 无 |
| Muse | `muse` | Responses、Chat Completions、Messages | 支持 | 无 | 生成，可凭对话路径恢复 | 无；对话可读取音频附件 |

“可恢复图片任务”指实现了 `AsyncImageAdapter` 的持久任务，不是 SSE 流式响应。所有视频生成通过异步视频任务接口提交；是否能安全恢复还取决于 Provider 是否已记录上游任务或对话。

## 2. 对话模型

### 2.1 Grok Build：动态目录，下面是代码已识别的模型规则

Build 从上游 `/v1/models` 同步模型。以下条目是代码的已知模型规则，**不是所有账号必定拥有的固定目录**；上游返回的新 ID 也可以进入目录。

| 模型 | 能力／用途 | 推理档位的本地规则 | 可用条件或说明 |
|---|---|---|---|
| `grok-4.7` | 通用对话、工具调用 | low／medium／high／xhigh | 以账号模型目录为准 |
| `grok-4.6` | 通用对话、工具调用 | low／medium／high／xhigh | 目录出现该模型时，补保留 `grok-4.5` 兼容路由 |
| `grok-4.5` | 通用对话、工具调用 | low／medium／high | 不能用 none 关闭推理 |
| `grok-4.3` | 通用对话、工具调用 | none／low／medium／high | 以账号模型目录为准 |
| `grok-4.20-0309-reasoning` | 推理对话 | low／medium／high | 与 Console 同名模型的固定推理规则不同 |
| `grok-4.20-0309-non-reasoning` | 非推理对话 | none | 以账号模型目录为准 |
| `grok-4.20-multi-agent-0309` | 多代理推理 | low／medium／high／xhigh | 以账号模型目录为准 |
| `grok-build-0.1` | Build 对话 | none | 以账号模型目录为准 |
| `grok-3-mini` | 兼容的推理模型名 | low／medium／high | 代码保留规则，不代表当前账号目录一定返回 |
| `grok-3-mini-fast` | 兼容的快速推理模型名 | low／medium／high | 同上 |
| `grok-composer-2.5-fast` | Composer 编程模型 | 不接受可配置 effort | Build OAuth 会话补入该模型，使用独立上游会话 |
| 其他上游返回的模型 ID | 按实际模型运行 | 优先使用上游发布的菜单 | 未知模型没有菜单时不自动扩展推理别名 |

- 上游目录发布的 `reasoning_efforts`、默认档位、上下文窗口、最大输出 token、后端搜索能力优先于本地兜底规则；不能把测试样例中的数值当成所有 Build 模型的上限。
- 支持文本、图片、`input_file`、函数工具及自定义工具兼容处理。Chat 的 `file` 会转换为 `input_file`；音频内容块没有开放。文件实际接受范围仍由上游决定。
- Build 的图片输入是**对话附件**，不等于提供图片生成接口；对话附件没有沿用“视频最多 8 张参考图”的限制。

来源：[Build 模型发现](backend/internal/infra/provider/grok/cli/models.go)、[推理规则](backend/internal/domain/model/reasoning.go)、[动态模型元数据](backend/internal/domain/model/upstream_profile.go)、[输入转换](backend/internal/infra/provider/grok/cli/responses_input.go)。

### 2.2 Grok Web：4 个固定对话入口

| 模型 | 模式 | 最低账号档位 | 输入与工具 |
|---|---|---|---|
| `grok-chat-fast` | fast | Basic | 文本、图片、文件；函数工具及搜索按 Web 适配器处理 |
| `grok-chat-auto` | auto | Super | 同上 |
| `grok-chat-expert` | expert | Super | 同上 |
| `grok-chat-heavy` | heavy | Heavy | 同上 |

这些是网页模式名，不是固定的底层模型版本承诺。对话不支持 `input_audio`；图片、文档格式由 Web 自己的附件校验决定，不能套用图片编辑的 5／8 张上限。

来源：[Web 目录](backend/internal/infra/provider/grok/web/catalog.go)、[对话输入](backend/internal/infra/provider/grok/web/chat_input.go)、[附件校验](backend/internal/infra/provider/grok/web/attachments.go)。

### 2.3 Grok Console：6 个固定对话模型

| 模型 | 推理 | 可配置 effort | 最大输出 token 上限 | 特别限制 |
|---|---|---|---:|---|
| `grok-4.3` | 支持 | none／low／medium／high；默认 medium | 1,000,000 | 原生函数工具与搜索按模型处理 |
| `grok-4.20-0309-reasoning` | 固定开启 | 不接受配置 | 1,000,000 | 会推理不等于能传 `reasoning_effort` |
| `grok-4.20-0309-non-reasoning` | 无 | none | 1,000,000 | 非推理入口 |
| `grok-4.20-multi-agent-0309` | 支持 | low／medium／high／xhigh | 1,000,000 | 多代理模型 |
| `grok-4.5` | 支持 | low／medium／high；默认 medium | 1,000,000 | 适配器禁用内置 web／x 搜索工具；函数工具另算 |
| `grok-build-0.1` | 无可配置推理 | none | 256,000 | 与 Build 渠道同名入口不是同一账号通路 |

表中 token 数是适配器目录的**输出参数上限**，不是上下文窗口，也不是每次保证输出的长度。Console 对话无状态，历史需随请求携带。

来源：[Console 目录](backend/internal/infra/provider/grok/console/catalog.go)、[Console 请求归一化](backend/internal/infra/provider/grok/console/normalize.go)。

### 2.4 ChatGPT 与 Muse

| 渠道／模型 | 目录方式 | 输入 | 工具与输出 |
|---|---|---|---|
| ChatGPT：账号 `/backend-api/models` 返回的所有对话 ID | 动态，例如 `auto`、`gpt-6`、`gpt-5-3`、`gpt-5-3-mini`；以账号返回为准 | 文本、图片 | JSON／SSE；函数工具用提示词模拟，联网搜索由 ChatGPT 服务端执行；文件、音频内容块不在当前对话实现范围内 |
| Muse：`muse-spark` | 固定入口 | 文本、图片、视频、音频、PDF／DOC／DOCX；最后一条用户消息最多 10 个附件 | JSON／SSE；网页 Markdown 转换；不输出函数工具调用，`tools` 被忽略 |

- Muse 的 `input_file` 使用 `file_url` 或 `file_data`，对话不支持 `file_id`；Chat 支持 `file`／`input_audio`，Messages 支持 `document`。以前轮次的附件不会重新上传，视频附件也受 120 秒上限约束。
- ChatGPT 图片路由下面的 8 张限制属于**图片编辑**，不能据此宣称所有对话模型都限制 8 张图。

来源：[ChatGPT 模型发现](backend/internal/infra/provider/chatgpt/accounts.go)、[ChatGPT 对话](backend/internal/infra/provider/chatgpt/responses.go)、[Muse 对话与附件](backend/internal/infra/provider/muse/chat.go)。

## 3. 图片模型：参考图数与出图数分开看

### 3.1 Grok Web

| 模型 | 生成／编辑 | 编辑输入图上限 | API 目标输出 `n` | 流式／蒙版 | 比例与分辨率 |
|---|---|---:|---|---|---|
| `grok-imagine-image-lite` | 仅生成 | — | 1–10 | 不支持流式；无蒙版 | Web 通用比例组；分辨率由产品决定 |
| `grok-imagine-image` | 仅生成 | — | 1–10 | 支持 SSE；无蒙版 | Web 通用比例组；`resolution`、`quality` 不作为独立档位发送 |
| `grok-imagine-image-2.0` | 生成＋编辑 | 5 | 1–10；编辑每次上游 1 张，由网关聚合 | 支持 SSE；无蒙版 | Web 2.0 比例组；使用 `enable_pro` 区分产品 |
| `grok-imagine-image-edit` | 仅编辑 | 8 | 1–10；每次上游 1 张，由网关聚合 | 支持 SSE；无蒙版 | Web 通用比例组；编辑未指定比例时尽量保持原图 |

- Web 通用比例组：`1:1, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, 2:1, 1:2, 19.5:9, 9:19.5, 20:9, 9:20`，也接受自动比例。
- Web 2.0 比例组：`1:1, 16:9, 9:16, 4:3, 3:2, 2:3, 21:9, 5:2`，也接受自动比例。
- `partial_images` 支持 0–3，只在流式时使用。Web 2.0 编辑超过 5 张会报参数错误；旧编辑入口超过 8 张保留前 8 张。
- Web 的公开 `grok-imagine-image` 对应上游 `grok-imagine-image-quality`；不能据同名把它与 Console 的经典图片产品混为一谈。

来源：[Web 图片策略](backend/internal/infra/provider/grok/web/image_policy.go)、[生成与比例](backend/internal/infra/provider/grok/web/image.go)、[编辑](backend/internal/infra/provider/grok/web/image_edit.go)。

### 3.2 Grok Console

| 模型 | 生成／编辑 | 编辑输入图上限 | API 目标输出 `n` | 分辨率／质量 | 流式／蒙版 |
|---|---|---:|---|---|---|
| `grok-imagine-image` | 生成＋编辑 | 3 | 1–10，原生批量 | 1k／2k；默认 1k；忽略 `quality` | 均不支持 |
| `grok-imagine-image-quality` | 生成＋编辑 | 3 | 1–10，原生批量 | 1k／2k；默认 1k；忽略 `quality` | 均不支持 |
| `grok-imagine-image-2.0` | 生成＋编辑 | 3 | 1–10，原生批量 | 1k／2k；质量 low／medium；默认生成 low、编辑 medium | 均不支持 |

比例组：`1:1, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, 2:1, 1:2`。超出的参考图保留前 3 张；4k 等尺寸档位会映射到 2k，不代表实际支持 4k。

来源：[Console 图片策略](backend/internal/infra/provider/grok/console/image_policy.go)、[媒体请求](backend/internal/infra/provider/grok/console/media.go)。

### 3.3 ChatGPT：Web 图片与 Codex 图片要区分

| 模型 | 实际通路／引擎 | 编辑输入图上限 | API 目标输出 `n` | 蒙版／可恢复任务 | 账号条件 |
|---|---|---:|---|---|---|
| `gpt-image-2` | Web 对话模型 `gpt-5-3` 的图片工具 | 8 | 1–10；单次上游 1 张 | 支持蒙版；可恢复任务 | 账号目录中该对话模型启用生图工具 |
| `gpt-image-2.5` | Web `gpt-6`，其次 `auto` | 8 | 同上 | 同上 | 账号有可用的图片工具 |
| `gpt-image-2.5-flare` | 与 Web `gpt-image-2.5` 同一通路 | 8 | 同上 | 同上 | 兼容别名，不能强制 Flare 引擎 |
| `gpt-image-2.5-sunburst` | 与 Web `gpt-image-2.5` 同一通路 | 8 | 同上 | 同上 | 兼容别名，不能强制 Sunburst 引擎 |
| `gpt-image-2.6` | Web `gpt-6`，其次 `auto` | 8 | 1–10；单次上游 1 张 | 静态图支持蒙版；可恢复任务 | 账号有可用的图片工具；GIF 见下文 |
| `gpt-image-2.6-flare` | 与 Web `gpt-image-2.6` 同一通路 | 8 | 同上 | 同上 | 兼容别名，不能强制 Flare 引擎 |
| `gpt-image-2.6-sunburst` | 与 Web `gpt-image-2.6` 同一通路 | 8 | 同上 | 同上 | 兼容别名，不能强制 Sunburst 引擎 |
| `codex-gpt-image-2` | Codex Images API：`gpt-image-2` | 8 | 1–10；网关拆分单图 | 支持蒙版；无可恢复上游任务 | 付费套餐 |
| `codex-gpt-image-2.5` | Codex Images API：`gpt-image-2.5` | 8 | 同上 | 同上 | 付费套餐 |
| `codex-gpt-image-2.5-flare` | Codex Images API：`gpt-image-2.5-flare` | 8 | 同上 | 同上 | 付费套餐 |
| `codex-gpt-image-2.5-sunburst` | Codex Images API：`gpt-image-2.5-sunburst` | 8 | 同上 | 同上 | 付费套餐 |
| `codex-gpt-image-2.6-flare` | Codex Images API：`gpt-image-2.6-flare` | 8 | 1–10；网关拆分单图 | 静态图支持蒙版；无可恢复上游任务 | 付费套餐；GIF 还需上游开放权限 |
| `codex-gpt-image-2.6-sunburst` | Codex Images API：`gpt-image-2.6-sunburst` | 8 | 同上 | 同上 | 同上 |

共同规则：

- 超过 8 张保留前 8 张；**有蒙版时只能有 1 张输入图**。蒙版不是第 2 张参考图。
- 静态图支持网关 SSE，但当前实现不提供真正的中间预览图，`partial_images` 不代表能获得逐步画面。
- 静态图 `size` 的参数适配范围为每边 256–4096；也可由 `aspect_ratio` 和分辨率档位转换。Web 通路把形状／尺寸写入提示词，不能保证像素严格一致；Codex 通路发送 Images API 参数。
- 静态图质量支持 auto／low／medium／high；输出格式 PNG／JPEG／WebP；支持透明背景请求，但 JPEG 不能保存透明通道。透明结果还会经过本地检查／处理。
- Codex 付费套餐名单：Plus、Pro、Team、Business、Enterprise、Edu。Codex 路由都不接可恢复的异步图片任务。
- `provider.chatgpt.imageConversationModel` 配置可覆盖 Web 图片路由选用的对话模型。

**GPT Image 2.6 GIF**

- Images 生成、编辑接口接受 `output_format: "gif"`；Web 还支持 Responses `image_generation` 工具及后台图片任务。Codex 继续使用 Images API，不接可恢复任务。
- `frame_count` 为 2–16；`frame_duration_ms` 至少 20 ms。省略时 Codex 使用上游默认值（9 帧、200 ms/帧）；Web 只把明确提供的值写入提示词，实际帧数、帧间隔由图片工具决定。
- `complexity` 接受 low／medium／high：Codex 直接透传，Web 作为规划复杂度提示，不改变 `quality`。
- GIF 不支持 `stream: true`、非零 `partial_images`、蒙版、`output_compression` 或透明背景请求。尺寸为 auto 或每边为 16 倍数的正整数；最终限制由上游模型执行，Web 尺寸仍属于提示。
- 原始 GIF 字节直接归档并以 `image/gif` 返回，保留所有帧、延迟、循环与 disposal 信息；不会放大、重新编码成静态图，也不会把返回的 PNG 冒充 GIF。

```json
{
  "model": "chatgpt/gpt-image-2.6",
  "prompt": "一只小猫眨眼，首尾衔接的循环动画",
  "output_format": "gif",
  "frame_count": 9,
  "frame_duration_ms": 200,
  "response_format": "url"
}
```

发送至 `POST /v1/images/generations`。需要明确指定原生 Flare 引擎时，模型改为 `chatgpt/codex-gpt-image-2.6-flare`。部署后同步 ChatGPT 账号模型以刷新可用路由。上述适配已用模拟上游验证，真实账号能否生成 GIF 仍取决于上游开放情况。

官方参数：[图片生成与 GIF 指南](https://developers.openai.com/api/docs/guides/image-generation)、[Images API 参数](https://developers.openai.com/api/reference/resources/images/methods/generate)。

来源：[Web 图片模型映射](backend/internal/infra/provider/chatgpt/image_models.go)、[Codex 图片路由](backend/internal/infra/provider/chatgpt/codex_images.go)、[图片参数与校验](backend/internal/infra/provider/chatgpt/images.go)、[输出处理](backend/internal/infra/provider/chatgpt/image_output.go)。

### 3.4 Adobe Firefly 图片

这 6 个模型均支持生成、编辑和可恢复图片任务；图片 API 目标输出数为 1–10，不支持图片 SSE。

| 模型 | 上游版本 | 编辑输入图上限 | 分辨率档位 | 蒙版／透明背景 | 比例组 |
|---|---|---:|---|---|---|
| `nano-banana-pro` | `gemini-flash/nano-banana-2` | 14 | 1K／2K／4K | 均不支持 | N |
| `nano-banana-2` | `gemini-flash/nano-banana-3` | 14 | 512／1K／2K／4K | 均不支持 | N＋超宽比例 |
| `nano-banana-2.1` | `gemini-flash/nano-banana-2026-09` | 14 | 1K／2K／4K | 均不支持 | N＋超宽比例 |
| `gpt-image-2` | `gpt-image/2` | 16，另可加 1 张蒙版 | 1K／2K／4K | 蒙版支持；透明背景不开放 | G |
| `gpt-image-2.5-flare` | `gpt-image/gpt-image-2.5-flare` | 16，另可加 1 张蒙版 | 1K／2K／4K | 均支持 | G |
| `gpt-image-2.5-sunburst` | `gpt-image/gpt-image-2.5-prism` | 16，另可加 1 张蒙版 | 1K／2K／4K | 均支持 | G |

- N：`1:1, 3:2, 2:3, 3:4, 4:3, 4:5, 5:4, 9:16, 16:9, 21:9`。
- 超宽比例：`1:8, 8:1, 1:4, 4:1`。
- G：`auto, 1:1, 21:9, 16:9, 3:2, 4:3, 5:4, 4:5, 3:4, 2:3, 9:16, 9:21`。
- 默认分辨率选 1K；不支持的比例／档位就近对齐。超出的参考图保留前面的。
- GPT Image 的 `quality` 映射为 detailLevel：low→1、medium／默认→3、high→5；Nano Banana 不发送这个质量等级字段。
- GPT Image 家族的尺寸规划受**最长边 3840、总像素 8,294,400**约束；“4K 档”不等于固定 4096×4096。Nano Banana 使用自己的尺寸枚举，不套用该上限。
- Adobe 的 Flare、Sunburst 确实发送不同上游版本；与 ChatGPT Web 的同名别名不同。

来源：[Adobe 模型表](backend/internal/infra/provider/adobe/models.go)、[图片规划](backend/internal/infra/provider/adobe/builder.go)、[图片接口](backend/internal/infra/provider/adobe/images.go)。

### 3.5 Muse 图片

| 模型 | 能力 | 编辑输入图上限 | API 目标输出 `n` | 比例／尺寸 | 流式／蒙版 |
|---|---|---:|---|---|---|
| `muse-image` | 生成＋编辑 | 10 | 1–10；每次网页请求 1 张，网关拆分 | 通过提示词表达比例、size、resolution；不承诺固定像素 | 均不支持 |

超过 10 张保留前 10 张。历史运行记录有约 1600 像素宽的 WebP 成品，不能据此把任意 `size` 参数当成硬尺寸保证。音频、文档附件支持在 Muse 对话入口，尚未扩展为 Images API 的编辑素材。

来源：[Muse 图片](backend/internal/infra/provider/muse/images.go)、[附件限制](backend/internal/infra/provider/muse/inputs.go)。

## 4. 视频模型

视频接口每个任务生成 1 个成品，不使用图片 API 的 `n`。以下时长均为秒，写作“范围；默认值”。

### 4.1 Grok 与 Muse

| 渠道／模型 | 生成时长 | 输出分辨率 | 首帧／尾帧／关键帧 | 参考图／参考视频 | 编辑／延长 |
|---|---|---|---|---|---|
| Build：`grok-imagine-video-1.5` | 1–15；默认 8 | 480p／720p；无参考素材时可 1080p | 首帧 1；无尾帧、关键帧 | 参考图 ≤8；不能与首帧同用；不收参考视频 | 均不支持 |
| Web：`grok-imagine-video` | 1–15；默认 8；有参考图最多 10 | 480p／720p；默认 720p | 首帧 1；无尾帧、关键帧 | 参考图 ≤7，可与首帧同用，图片合计最多 8；不收参考视频 | 均不支持 |
| Console：`grok-imagine-video` | 文生／首帧模式 1–15；参考图模式 1–10；默认 8 | 480p／720p；默认 720p | 首帧 1；无尾帧、关键帧 | 参考图 ≤7；不能与首帧同用；不收参考视频 | 编辑支持；延长 2–10，默认 6 |
| Console：`grok-imagine-video-1.5` | 1–15；默认 8 | 480p／720p；纯文本或仅首帧可 1080p | 首帧 1、尾帧 1、内部关键帧 ≤4 | 参考图 ≤14，可叠加首尾帧／关键帧，图片合计最多 20；不收参考视频 | 均不支持 |
| Muse：`muse-video` | 请求对齐到 1–15；默认 5；实际由 agent 决定 | 按 720p 处理 | 显式首帧 1；无尾帧、关键帧 | 首帧＋参考图＋参考视频合计 ≤10 | 均不支持 |

共同与例外规则：

- Grok 视频比例：`1:1, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3`；默认 `16:9`。Muse 为 `16:9, 9:16, 1:1, 4:3, 3:4`。
- **Build 视频 1.5 要求 Super**，Free／Unknown 账号能力同步时会移除该模型；Console 视频 1.5 的目录不要求付费套餐，但仍受账号预算和上游授权限制。
- Build 与经典 Console 同时收到首帧、参考素材时，归一化保留首帧并清掉参考模式。Build 可发送最多 3 个 `reference_audios.voice_id`，不能与首帧同用，实际权限由上游决定。
- Console 1.5 可发送最多 3 个 `voice_id`，可单独使用或与图片输入组合；该权限可能需要上游团队开通。经典 Console 忽略参考音色。**voice ID 不是音频文件 URL。**
- Console 1.5 有参考图／音色／尾帧／关键帧时最高 720p。关键帧必须处于 `(0, duration)` 内，且不能落到同一个 1/3 秒时间槽；尾帧可以不配首帧。
- 只有 Console 生成视频开放 `generate_audio`：true 有声、false 静音，省略用上游默认。其他渠道不能据“视频可能有音轨”推断该开关可用。
- **Web Basic 账号实际时长还受 `provider.web.freeVideoDurationCap` 限制，默认最多 6 秒，可配置 1–15。**有参考图时仍最多 10 秒；按更小上限执行。
- Web 不使用 `reference_audios`；单张参考图仍发送 `useFirstFrame=false`，不会自动变成首帧。
- Muse 参考视频支持 MP4／MOV／WebM，每个最长 120 秒。纯图片超量会截断；包含参考视频且总附件超过 10 个则报错。参考音色忽略；生成时长与输出尺寸不能硬保证，历史记录中请求 5 秒也曾得到 10 秒。
- 经典 Console 视频编辑不接受 duration、比例、分辨率作为有效控制参数；延长不使用比例、分辨率。编辑／延长均以 `video` 提供源视频。

来源：[Grok 视频公共映射](backend/internal/infra/provider/grok/video.go)、[Build 视频](backend/internal/infra/provider/grok/cli/video.go)、[Web 视频](backend/internal/infra/provider/grok/web/video.go)、[Console 策略](backend/internal/infra/provider/grok/console/definition.go)、[Muse 视频](backend/internal/infra/provider/muse/video.go)。

### 4.2 Adobe Firefly 视频

所有 Adobe 尾帧都要求同时有首帧；不提供内部关键帧接口。表中“2 帧”指首帧＋尾帧，不是 2 张风格参考图。

| 模型 | 时长；默认 | 分辨率 | 比例 | 首尾帧 | 参考图／参考视频 | 编辑／延长 |
|---|---|---|---|---|---|---|
| `veo-3.1` | 4／6／8；默认 8 | 720p／1080p | 16:9／9:16 | ≤2 帧 | ≤3 张参考图；与首尾帧互斥；参考图模式固定 16:9、8 秒 | 均无 |
| `veo-3.1-fast` | 4／6／8；默认 8 | 720p／1080p | 16:9／9:16 | ≤2 帧 | 不支持参考图或参考视频 | 均无 |
| `veo-3.1-lite` | 4／6／8；默认 8 | 720p／1080p | 16:9／9:16 | ≤2 帧 | 不支持参考图或参考视频 | 均无 |
| `seedance-2.0` | 4–15；默认 4 | 480p／720p／1080p | S | ≤2 帧 | ≤9 图＋≤3 视频，合计 ≤12；与首尾帧互斥 | 均无 |
| `seedance-2.0-fast` | 4–15；默认 4 | 480p／720p | S | ≤2 帧 | ≤9 图＋≤3 视频，合计 ≤12；与首尾帧互斥 | 均无 |
| `seedance-2.5` | 4–30；默认 4 | 480p／720p | S；带首帧强制 auto | ≤2 帧 | ≤9 图＋≤3 视频，合计 ≤12；与首尾帧互斥 | 支持编辑；延长 4–30，默认 4 |
| `kling-3.0` | 3–15；默认 5 | 720p／1080p | 16:9／9:16／1:1 | ≤2 帧 | 不支持参考图或参考视频 | 均无 |
| `kling-3.0-omni` | 3–15；默认 5 | 720p／1080p | 16:9／9:16／1:1 | ≤2 帧 | ≤3 张参考图，可与首尾帧组合，图片最多 5；不收生成参考视频 | 支持编辑；无延长 |
| `kling-video-2.5-turbo` | 5／10；默认 5 | 1080p | 16:9／9:16／1:1 | **必须有首帧**，可加尾帧 | 不支持参考图或参考视频 | 均无 |

- S：`auto, 21:9, 16:9, 4:3, 1:1, 3:4, 9:16`。未指定时采用 16:9；有明确 auto 时保留 auto，Seedance 2.5 的首帧模式也强制 auto。
- 默认分辨率一般为 720p；Kling 2.5 Turbo 只有 1080p。不支持的时长、比例、分辨率会选最近支持值，不是原样透传。
- Veo／Seedance 的互斥输入混传时保留首尾帧、清掉参考素材。超过参考图／视频数量时保留前面的；模型不支持的素材类型仍报错。
- Seedance 2.5 编辑与延长的比例是 auto；编辑时长随源视频、不发送 duration。Kling Omni 编辑也不发送 duration，保留源音轨。
- 编辑／延长的源视频放在 `video`，与生成参考 `reference_videos` 是不同用途。Adobe 当前接收的源／参考视频为 MP4／QuickTime。
- Adobe 适配器内部按模型设置音轨默认值：Kling 3.0／Omni 生成默认关闭，其他带 `generateAudio` 字段的生成版本默认开启；API 不开放显式 `generate_audio` 控制。

来源：[Adobe 模型与派生编辑版本](backend/internal/infra/provider/adobe/models.go)、[视频归一化及输入组合](backend/internal/infra/provider/adobe/video.go)、[请求构建](backend/internal/infra/provider/adobe/builder.go)。

## 5. 语音模型

| 渠道／模型 | 能力 | 当前网关明确限制 | 格式或说明 |
|---|---|---|---|
| Console：`grok-voice-latest` | TTS、Realtime | TTS 文本 ≤15,000 字符；时长由上游决定 | `language` 必填；可传 voice ID、输出 codec／采样率／码率等 |
| Console：`grok-voice-think-fast-2.0` | TTS、Realtime | 同上 | TTS 走公共 `/tts` 通路，不能仅凭模型名断言选中了不同 TTS 引擎 |
| Console：`grok-voice-think-fast-1.0` | TTS、Realtime | 同上 | 同上 |
| Console：`grok-stt` | STT、流式转录代理 | 未定义统一音频秒数上限；请求体受服务配置限制 | 文件或音频 URL；支持转录参数，按上游结果返回时间信息 |
| ChatGPT：`chatgpt-tts` | TTS | 文本 ≤4,096 字符；无固定输出秒数承诺 | mp3／aac／opus；账号音色；不支持 wav／flac／pcm 输出 |
| ChatGPT：`chatgpt-transcribe` | STT | 音频文件 ≤25 MiB；无固定秒数上限 | 只返回转录文字；支持 WebM、Ogg、MP3、M4A、WAV、FLAC、AAC 等上传 |

ChatGPT 语音路由会加入所有账号的可调度能力；实际请求仍可能受到上游权限或额度限制。Muse 对话读取音频不等于提供 TTS、STT 或 Realtime API。

来源：[Console 语音](backend/internal/infra/provider/grok/console/voice.go)、[ChatGPT 语音](backend/internal/infra/provider/chatgpt/voice.go)、[语音路由](backend/internal/infra/provider/chatgpt/definition.go)。

## 6. 上传、任务和超限行为

`file_id` 主要用于视频素材的暂存引用。当前 Images Edits 入口不接受 `file_id`，需使用 HTTPS URL、Base64 或 multipart 文件；Muse 对话也不接受 `file_id`。

| 层级／场景 | 限制 | 使用时的含义 |
|---|---|---|
| HTTP 请求体 | 由部署的请求体上限配置控制 | Base64 有约 4/3 编码开销；不能用原文件大小直接当 JSON 大小 |
| 临时媒体输入 `file_id` | 单文件 20 MiB | 这是上传／暂存安全上限，不是所有上游原生文件上限 |
| 统一远程素材抓取 | 单文件 20 MiB | Muse、Adobe 使用的统一抓取器受此限制，且会做 DNS 固定与 SSRF 检查 |
| 媒体任务输入预算 | 编码预算 32 MiB | 暂存文件也按展开为 data URI 的体积计入预算；换成 `file_id` 不会绕过预算 |
| 图片任务存储数量 | 最多 20 张输入图 | 仅存储安全边界；实际仍先由 Provider 裁剪／校验为 3、8、10、14、16 等 |
| Muse 附件 | 总共 ≤10 个；每个 ≤20 MiB；总计 ≤100 MiB | 对话／图片／视频各自支持的文件类型不同；媒体任务另受 32 MiB 编码预算约束 |
| Grok Web 自行下载输入图 | 默认每张 32 MiB，可配置 | 不代表 Base64 请求体也能放入 32 MiB 原图；仍需满足 HTTP 和任务预算 |

几种容易混淆的行为：

| 请求 | 实际处理 |
|---|---|
| Console 图片编辑传 10 张图 | 保留前 3 张；不会自动拆成 10 个独立编辑 |
| ChatGPT 编辑传 12 张图 | 保留前 8 张；加蒙版则必须只有 1 张输入图 |
| Adobe Nano Banana 传 16 张图 | 保留前 14 张；GPT Image 家族可以保留 16 张 |
| Muse 图片编辑传 12 张图 | 保留前 10 张 |
| Web 2.0 编辑传 6 张图 | 参数错误，不是截到 5 张 |
| 请求图片 `n=5` | 目标是 5 张输出，与参考图数量无关；单图 Provider 由网关拆分生成 |
| Muse 视频报告“已生成”，附件尚未出现 | 继续等待真实附件，不因文字稳定 15 秒就判失败 |
| 上传素材被 Muse 原样回显 | 不作为生成成品交付；新视频任务恢复时也保留原件识别信息 |
| 模型不接受的比例／时长／分辨率 | 大多就近归一化；蒙版、操作类型和输入组合等硬限制仍可能报错 |

来源：[媒体输入边界](backend/internal/domain/media/job.go)、[任务编码预算](backend/internal/application/gateway/video_input.go)、[统一下载](backend/internal/transport/http/media/ingest.go)、[Provider 图片归一化接口](backend/internal/infra/provider/image.go)。

## 7. 覆盖范围与维护

- 固定目录：Grok Web **9** 个、Grok Console **15** 个、ChatGPT 图片／语音 **10** 个、Adobe **15** 个、Muse **3** 个，共 **52** 个渠道模型条目。一个模型同时支持生成与编辑时只算一个名称。
- Build 对话、ChatGPT 对话按账号动态发现；部署新增的自定义路由不在固定表中。
- `-low`／`-medium`／`-high`／`-xhigh` 等 effort 别名，以及旧版 `*-console` 等兼容名，不作为独立能力模型重复计数。别名只在对应渠道／模型确实支持时生效。
- 新增模型或修改限制时，同步更新本表、对应 Provider 的目录／归一化测试。目录与接口组织方式见 [PROVIDERS.md](PROVIDERS.md)。
