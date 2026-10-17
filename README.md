# Index-Translate-Docker

基于 [bilibili/Index-Translate](https://github.com/bilibili/Index-Translate) 免费公共 API 的本地桥接代理. 使用 Go 重写其 `inference/llm/call_api.py --serve` 的行为, 为沉浸式翻译等浏览器扩展提供 OpenAI 兼容接口, 并支持 Docker 部署.

## 功能

- 与 `python call_api.py --serve` 等价的本地代理: 默认监听 `127.0.0.1:8080`, 使用 `--host 0.0.0.0` 可对局域网开放.
- 将 `/v1/chat/completions`, `/v1/completions`, `/v1/responses` 原样转发到免费公共 API, 默认上游为 `https://index-translate.bilibili.com/v1`.
- 为浏览器扩展补充 CORS 响应头, 并响应 `/v1/models` 与就绪探测.
- 默认注入 `enable_thinking=false`, 客户端未指定 `temperature` 时固定为 `0`, 避免思维链输出替代译文.
- 上游响应与错误状态码原样透传, 支持 SSE 流式输出.
- `/v1/chat/completions` 一律以上游流式调用: 非流式客户端收到的流会组装回标准响应, 同时避开免费网关约 10 秒静默即 504 的超时限制.
- 上游连接错误, 无数据即断开的响应与可重试状态码 (429/500/502/503/504) 自动重试, 最多 3 次 (共 4 次尝试); 其余 4xx 立即透传.
- 可选 API key 鉴权: 设置 `-key` 或 `API_KEY` 后, 请求需携带 `Authorization: Bearer <key>`; 未配置时不鉴权.

## 快速开始

### 源码运行

要求 Go 1.27 或更新版本.

```sh
go build -o index-translate-proxy .
./index-translate-proxy --serve                 # 监听 127.0.0.1:8080
./index-translate-proxy --serve --host 0.0.0.0  # 对局域网开放
./index-translate-proxy --serve -key your-secret
# 或通过环境变量设置密钥
API_KEY=your-secret ./index-translate-proxy --serve
```

### Docker

```sh
docker run -d --name index-translate-proxy -p 8080:8080 --restart unless-stopped ghcr.io/yeqingky/index-translate-docker:latest
```

### Docker Compose

设置 `API_KEY` 环境变量可为 Compose 服务启用鉴权; 未设置时不鉴权:

```sh
API_KEY=your-secret docker compose up -d
```

### GHCR 镜像与 GitHub Release

GitHub Actions 在分支 push 和 `v*.*.*` tag push 时构建并推送 `linux/amd64` 与 `linux/arm64` 多架构镜像. 普通提交使用 7 位提交短 SHA 作为镜像 tag; 符合 `vX.Y.Z` 的 tag 发布 `X.Y.Z` 与 `latest`. Pull request 只构建验证, 不推送镜像或创建发行版.

`vX.Y.Z` tag 通过验证和镜像发布后, 工作流会创建 GitHub Release, 并附上四种二进制:

- `index-translate-proxy-linux-amd64`
- `index-translate-proxy-linux-arm64`
- `index-translate-proxy-windows-amd64.exe`
- `index-translate-proxy-windows-arm64.exe`

发行说明由 GitHub 自动生成.

### 沉浸式翻译配置

1. 翻译服务选择: 自定义 (OpenAI 兼容).
2. 接口地址 (API URL): `http://127.0.0.1:8080/v1`.
3. 模型 (Model): `Index-Translate-35B-A3B`.
4. API Key: 填写服务端配置的密钥; 未配置时可随意填写 (如 `index`).

代理部署在其他主机时, 将 `127.0.0.1` 替换为对应主机地址.

## 参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--serve` | 关闭 | 启动本地代理 |
| `--host` | `127.0.0.1` | 监听地址, `0.0.0.0` 对局域网开放 |
| `--port` | `8080` | 监听端口 |
| `--api-base` | `https://index-translate.bilibili.com/v1` | 上游 API 地址 |
| `-key` | 空 | API 鉴权密钥; 也可通过 `API_KEY` 环境变量设置, 命令行参数优先 |

## 说明

- 未设置 `-key` 与 `API_KEY` 时不鉴权. 设置密钥后, 除 `OPTIONS` CORS 预检外的请求均需携带 `Authorization: Bearer <key>`.
- 对公网开放时建议设置强随机密钥; Bearer key 经明文 HTTP 传输, 应配合 HTTPS 反向代理.
- 上游是免费公共 API, 使用前请阅读 Index-Translate 项目的说明与条款.
- 免费 API 的网关对约 10 秒内没有响应数据的请求返回 504; 代理对 `/v1/chat/completions` 始终使用流式上游调用, 非流式客户端无需支持流式. 重试只发生在响应提交给客户端之前, 已开始输出的流不会重放.
- 本项目是对 `bilibili/Index-Translate` (Apache-2.0) 中 `call_api.py` 代理行为的 Go 重写, 未包含其代码副本.

## License

MIT
