# IM 统一引用输出实现说明

> 当前实现核对日期：2026-08-06。生产设备跳转、PDF 静态资源和部署边界见
> [当前生产实现与部署基线](./当前生产实现与部署基线.md)。

## 目标与边界

所有智能体类型和所有 IM 适配器共用一个最终出站引用渲染入口。模型、召回、重排和
数据库中保存的回答继续使用统一的 `<src id="Sx" />` 协议；只有向外部 IM 发送最终
正文前，才根据消息绑定的真实 `knowledge_references` 做一次确定性转换。

模型正文的自然语言来源说明与平台引用是两套共存体系：提示词可以要求输出
`📄《制度名》第X条`，但必须继续保留 `<src id="Sx" />`。前者属于回答正文，后者由
平台转换为可点击引用；任何一套都不能顶掉另一套。

该能力：

- 不增加模型调用、重试生成、召回或重排；
- 不接受旧引用标签，也不猜测、补造或降级到整篇文档；
- 错误、未知、缺坐标的引用标签在最终出口过滤；
- 流式中间帧继续隐藏内部标签，最终 replace 帧一次性显示稳定引用；
- 新增智能体类型天然复用 `CustomAgentConfig` 与 `im.Service.RenderFinalOutbound`。

## 代码边界

- `internal/custom/modules/imoutput`：平台无关的校验、编号、目标地址和 Markdown/
  Slack/plain-text 方言渲染。
- `internal/custom/modules/impreview`：对已持久化真实消息执行相同渲染的只读 API。
- `internal/im/service.go`：流式终帧、完整输出和流式启动失败回退的唯一原生挂接点。
- `internal/im/wecom`：企业微信应用 Markdown 分包和智能 Bot `stream` 协议封装。
- `frontend/src/custom/modules/imoutput`：电脑端独立公共引用阅读页、文档原文预览和
  公共引用 API 客户端。
- `frontend/src/custom/modules/mobile/views/MobileReferenceView.vue`：移动端独立公共
  引用阅读页和文档原文预览。

## 生效范围

统一转换是所有 IM 最终发送的强制出口，不属于某个智能体类型或某个 IM 渠道，也不
设置单独开关。快速问答、简单对话、智能推理、Wiki、数据分析、表格分析、通用智能体、
文档处理智能体以及后续新增类型，只要通过统一 IM 服务发送最终回答，都会使用同一转换。

生产必须配置浏览器可访问的前端 Origin：

```text
FRONTEND_BASE_URL=https://knora.moutai.com.cn
```

文档分片和 Wiki 使用 IM 专属的签名能力链接。没有合法绝对 Origin 时不发送伪装成
可点击的相对引用；非本地 Origin 仍强制使用 HTTPS。普通 Web 聊天和知识库页面的
认证规则不因此改变。

## 三类引用与编号

最终用户可见类型保持为三类：

| 类型 | 计数与目标 |
| --- | --- |
| 文档分片 | 每个被引用 chunk 单独计数，进入对应设备的独立公共阅读页并定位精确分片；页面可继续“查看原文档”，但不会进入知识库 |
| Wiki | 每个被引用 Wiki 来源计数，进入对应设备的独立公共阅读页并显示精确 Wiki 页面 |
| 网页 | 每个被引用网页计数，直达原始 HTTP(S) 地址 |

编号按正文第一次出现的顺序从 1 开始。同一来源重复出现复用编号；同一文档的不同分片
是不同引用。外部 IM 与 Web 一样只在正文相邻位置显示编号，不追加底部“参考来源”
列表；编号、地址和顺序均来自同一个经过校验的结果对象。

企业微信应用与智能 Bot 都使用：

```markdown
[\[1\]](https://knora.example.com/source)
```

客户端显示可点击的 `[1]`，不显示裸网址。应用模式发送 Markdown 消息，超过 2048
字节时使用真实分包器且不会从链接中间切开；Bot 模式将同一最终正文放入 replace-based
`stream.content`，结束帧设置 `finish=true`。

## 设备跳转与匿名隔离

文档与 Wiki 在 IM 正文中使用同一个绝对
`/api/v1/custom/im-output/reference?token=...` 地址。`token` 只由最终 IM 出口根据已绑定
到真实消息的来源签发，不接受模型生成的地址。用户点击时，后端先验证签名，再按
User-Agent 和 `Sec-CH-UA-Mobile` 做无状态设备判断：企业微信电脑端跳到独立
`/im-reference?token=...`，移动端跳到独立 `/mobile/reference?token=...`。网页引用始终
直接使用原始 HTTP(S) 地址。文档分片与 Wiki 必须固定使用企业微信内置阅读链路，
不能因来源类型或设备头差异随机跳到外部浏览器；网页来源允许外跳。

这两个阅读页专用于 IM 应用与 Bot 返回的引用，免 WeKnora 登录且不展示知识库导航。
公共后端仅开放以下精确只读路由：

```text
GET  /api/v1/custom/im-output/reference
GET  /api/v1/custom/im-output/reference/data
GET|HEAD /api/v1/custom/im-output/reference/original
```

能力链接不能携带任意跳转地址，也不能仅靠原始知识库、文档、分片或 Wiki 坐标访问。
服务端每次打开都会重新核对同一租户下的知识库、文档、精确分片或 Wiki 发布状态；来源
删除、禁用或归档后链接停止解析。文档页的“查看原文档”复用同一文档能力，只能打开该
分片所属原文档，不会跳回知识库或扩大为其他文档访问。响应使用 `no-store`、
`no-referrer` 和 `noindex`，正文中的受保护图片仅在点击阅读时换成短期签名文件地址。

普通电脑端 `/platform/*`、移动端产品页、知识库、Wiki 和聊天 API 仍使用原有登录与
权限校验；它们不会因为 IM 公共阅读页而变成匿名可见。IM 能力签发与页面读取都不参与
回答生成、召回或重排，因此不增加回答响应时间。

公共移动阅读页的原文 PDF 与知识库移动预览共用本站 PDF.js 资源：worker、WASM、
CMap、字体和 ICC 均从 `knora.moutai.com.cn` 加载，不访问公共 CDN。页面使用懒渲染
和像素上限控制性能，并覆盖复杂 JPX/JPEG2000 PDF、解析分片、窄屏布局和全屏按钮。

## 只读预览 API

```http
POST /api/v1/custom/im-output/preview
Authorization: Bearer <token>
Content-Type: application/json
```

请求必须引用当前租户可见的真实已完成助手消息，不接受调用方传入正文或引用：

```json
{
  "session_id": "...",
  "message_id": "...",
  "agent_id": "...",
  "platform": "wecom",
  "mode": "websocket",
  "output_mode": "stream"
}
```

也可以传 `channel_id`，此时平台、模式、输出模式和智能体都从真实渠道读取。响应同时
返回统一渲染结果和 `transport_payloads`：企业微信应用为实际 Markdown 分包，Bot 为
实际 `stream` 最终 body。接口不发送任何外部消息。

## 验收重点

- 所有平台 × 流式/完整输出；
- 快速问答与全部 ReAct/Claude Agent SDK 智能体；
- 单轮、多轮、取消知识库、无召回、零引用、重复引用、多分片、多来源、错误标签；
- 企业微信应用长回答分包、Bot 中间 replace 与最终 `finish`；
- Web 和移动端正文引用位置、顺序、完整性及三类来源跳转；
- 同一条企业微信引用分别从电脑端和移动端匿名点击，确认进入各自独立阅读页的精确来源；
- 文档分片页继续打开原文档，且全过程不进入知识库、不要求登录；
- 普通 Web/移动端知识库、Wiki 与聊天接口仍拒绝匿名访问；
- 本地 `-local` 模型为主，协议正常时可用 V4 Flash 排除模型能力因素；不采用降级实现。
