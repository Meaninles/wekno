# 前端二开模块

前端企业二开集中在 `frontend/src/custom/modules/`，原生页面只保留挂载点。总体
架构和后端状态语义见
[当前实现架构与文档索引](../../../docs/custom/当前实现架构与文档索引.md)。

## 主要模块

| 模块 | 作用 |
|---|---|
| `documentQueue` | 系统等待总量、每份文档的全局队列位置 |
| `chatqueue` | 聊天已接受排队卡、个人上限/系统满结构化提示和取消操作 |
| `capacity-control` | 系统管理员独立容量页、有效值/冲突、资源池、模板、绑定和审计 |
| `knowledgeWorkflowStatus` | 主解析和衍生阶段总状态、悬停明细、准确筛选 |
| `knowledgeFolders` | 文件夹、筛选祖先树、递归删除进度及知识库/文件夹任务悬浮概况 |
| `documentPreview` | preview policy、PDF.js 本地 worker/WASM/CMap/字体、懒渲染和大文件安全预览 |
| `wikiGraph` | 分类列表、搜索、分页、ego 图和中心节点切换 |
| `wikiActivation` | Wiki 高成本索引能力的二级入口、启用状态提示和提取粒度面板 |
| `wikiAccess` | Wiki 选择权查询、默认灰态提示和系统管理员按用户授权页 |
| `generalagent` / `dbanalytics` / `skillhub` | Agent 产物、数据分析和技能 |
| `iam` / `configcenter` / `authSecurity` | SSO、组织人员同步、同步异常详情、默认资源和认证安全 |
| `mobile` / `chatshare` / `sourceReferences` / `imoutput` | 移动端多格式文档预览、企业微信设备内引用、原文档、分享和来源展示 |
| `scheduledchat` / `sessionState` / `answerfeedback` | 定时任务、会话状态和反馈 |

## 状态显示约束

- 总状态必须由 `parse_status`、`summary_status`、`enrichment_status` 和
  `wiki_status` 联合计算。
- `none` 在前置阶段未完成时显示“尚未开始/等待前置解析”，不能显示“已跳过”。
- 只有功能关闭、不适用或明确 `skipped` 时显示跳过。
- 衍生失败必须进入“失败”筛选；轮询更新后要立即从旧状态桶移出。
- 文档卡片保持简洁，细分状态放在悬停/详情中。
- 聊天已接受等待使用蓝色状态卡；个人等待满使用橙色提示；模型池满使用红色提示。
- 429/503 必须按结构化 `CHAT_QUEUE_*` code 处理，恢复输入且不得保留幽灵消息。

## 大数据量约束

- 文件夹、搜索、Wiki 节点和邻居使用服务端分页或游标，不能先加载全库。
- 图默认展示有界概览；选中节点只取邻接子图，点击关联节点后以它为新中心。
- 大文档必须先请求 preview policy，不能直接把超大文件或全部页面载入 DOM。

修改后执行前端测试、type-check 和 desktop/mobile build，并至少用 API 数据和
浏览器各验证一次；只通过单元测试不足以证明状态文案和筛选正确。
