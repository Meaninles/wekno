# 前端开发说明

桌面前端位于本目录，开发入口为 `http://localhost:5177`。当前生产还包含独立的
`mobile-web` 两副本入口；两者通过 Nginx 代理同一组 app API。

## 启动与验证

```bash
npm ci
npm run dev -- --host 0.0.0.0 --port 5177
npm run test
npm run type-check
npm run build
npm run build:mobile
```

运行代码修改后先停止旧开发服务，再启动新实例，避免 5177 端口仍指向旧代码。

## 二开边界

新增大段逻辑放在 [`src/custom/README.md`](./src/custom/README.md) 所述目录，
原生页面只保留必要挂载点。完整架构见
[当前实现架构与文档索引](../docs/custom/当前实现架构与文档索引.md)，用户交互见
[用户使用指南](../docs/custom/使用指南/用户使用指南.md)。

## 上传代理

| 请求 | 后端业务上限 | Nginx 上限 |
|---|---:|---:|
| 普通附件 | 50 MiB | 80 MiB |
| 知识源原文件 | 2048 MiB | 2304 MiB |

知识源路由关闭 request buffering，生产读写超时为 7200 秒。Ingress、ELB/WAF
也必须同步设置，否则浏览器会在请求到达 app 前收到 413/504。

## 多副本

frontend 是无状态服务，可部署两副本并由 Service/Ingress 负载均衡，不要求粘性
会话或 RWX。登录态、会话、队列和产物均由后端/持久层管理。验收应确认正常分流、
停止任一副本后持续可用、恢复后重新加入。

## 本地静态资源与 PDF 预览

生产处于受限网络，前端不能依赖公共 CDN。构建脚本会把 `pdfjs-dist 5.4.624` 的
worker、CMap、standard fonts、WASM 和 ICC 资源复制到本站 `/pdfjs/`，同时本站托管
TDesign 图标。Nginx 必须把 `.mjs` 作为 JavaScript 返回。

桌面和移动端 PDF 使用串行懒渲染、像素上限和近邻页驻留控制，支持
JPX/JPEG2000、JBIG2 及缺省字体资源。修改预览代码后至少验证：小 PDF、复杂/大
PDF、解析分片、原文档、企业微信内置浏览器、全屏按钮，以及 Network 中不存在公共
CDN 请求。详见
[当前生产实现与部署基线](../docs/custom/当前生产实现与部署基线.md)。
