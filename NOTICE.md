# 第三方组件与来源声明

本仓库自有代码以 MIT 发布。以下组件来自第三方，各自遵循原许可，**不因本仓库的 MIT 而改变**。

## 前端静态资源（client/android/app/assets、client/harmony/.../rawfile）

| 项 | 说明 |
| --- | --- |
| 来源 | [deepseek-ai/chat-demo](https://github.com/deepseek-ai/chat-demo)（rspack 构建产物） |
| 许可 | MIT，Copyright (c) 2026 DeepSeek（随产物保留在 `app/assets/LICENSE`） |
| 本仓库的改动 | 构建期注入的接口基址改为自托管地址；另有少量补丁，见 [docs/frontend-patches](docs/frontend-patches) |

前端**源码不在本仓库**。要自行构建：在 chat-demo 源码目录 `pnpm build`，
再用 `client/android/sync-web.sh <dist 目录>` 同步进来（该脚本会保留自维护的
`login.html` 与 `icon.png`，其余整体替换）。前端必须打本仓库的补丁，
否则 App 端会一直用旧缓存渲染。

## 后端依赖（Go，见 server/chat-server/go.mod）

| 组件 | 许可 |
| --- | --- |
| modernc.org/sqlite（纯 Go SQLite） | BSD-3-Clause |
| github.com/yuin/goldmark（Markdown） | MIT |
| golang.org/x/crypto（bcrypt） | BSD-3-Clause |
| github.com/google/uuid | BSD-3-Clause |

## 沙箱执行器

| 组件 | 许可 |
| --- | --- |
| [rhai](https://rhai.rs/)（脚本引擎，selftest 用） | MIT / Apache-2.0 双许可 |

## 可选服务端组件（需自行获取二进制，本仓库不附带）

| 组件 | 用途 | 许可 / 来源 |
| --- | --- | --- |
| [TinyAuth](https://github.com/nanotiny/TinyAuth) | OIDC 身份提供方（登录/注册/第三方登录） | 见上游 |
| [PMail](https://github.com/nanotiny/PMail) | 自建收信服务器（注册验证码） | 见上游 |

`server/auth/deploy.sh` 与 `server/mail/deploy.sh` 只做部署编排，
二进制由使用者自行从上游获取并放到脚本约定位置。

## 商标

「DeepSeek」是杭州深度求索公司的商标。本仓库是第三方自托管实现，
与 DeepSeek 官方无任何隶属或背书关系；请勿以官方名义分发本仓库产物。
