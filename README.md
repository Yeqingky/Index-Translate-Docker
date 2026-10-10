# Index-Translate-Docker

基于 [bilibili/Index-Translate](https://github.com/bilibili/Index-Translate) 免费公共 API 的本地桥接代理. 使用 Go 重写其 `inference/llm/call_api.py --serve` 的行为, 为沉浸式翻译等浏览器扩展提供 OpenAI 兼容接口, 并支持 Docker 部署.

## 功能

- 与 `python call_api.py --serve` 等价的本地代理: 默认监听 `127.0.0.1:8080`, 使用 `--host 0.0.0.0` 可对局域网开放.
- 将 `/v1/chat/completions`, `/v1/completions`, `/v1/responses` 原样转发到免费公共 API, 默认上游为 `https://index-translate.bilibili.com/v1`.
- 为浏览器扩展补充 CORS 响应头, 并响应 `/v1/models` 与就绪探测.
- 默认注入 `enable_thinking=false`, 客户端未指定 `temperature` 时固定为 `0`, 避免思维链输出替代译文.
- 上游响应与错误状态码原样透传, 支持 SSE 流式输出.

## 快速开始

### 源码运行

要求 Go 1.27 或更新版本.

```sh
go build -o index-translate-proxy .
./index-translate-proxy --serve                 # 监听 127.0.0.1:8080
./index-translate-proxy --serve --host 0.0.0.0  # 对局域网开放
```

### Docker

```sh
docker build -t index-translate-proxy .
docker run -d --name index-translate-proxy -p 8080:8080 --restart unless-stopped index-translate-proxy
```

### Docker Compose

```sh
docker compose up -d --build
```

### 沉浸式翻译配置

1. 翻译服务选择: 自定义 (OpenAI 兼容).
2. 接口地址 (API URL): `http://127.0.0.1:8080/v1`.
3. 模型 (Model): `Index-Translate-35B-A3B`.
4. API Key: 随意填写 (如 `index`).

代理部署在其他主机时, 将 `127.0.0.1` 替换为对应主机地址.

## 参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--serve` | 关闭 | 启动本地代理 |
| `--host` | `127.0.0.1` | 监听地址, `0.0.0.0` 对局域网开放 |
| `--port` | `8080` | 监听端口 |
| `--api-base` | `https://index-translate.bilibili.com/v1` | 上游 API 地址 |

## 说明

- 代理不鉴权, 默认仅监听回环地址. 确认需要对局域网或公网开放时再使用 `--host 0.0.0.0`, 并自行限制来源.
- 上游是免费公共 API, 使用前请阅读 Index-Translate 项目的说明与条款.
- 本项目是对 `bilibili/Index-Translate` (Apache-2.0) 中 `call_api.py` 代理行为的 Go 重写, 未包含其代码副本.

## License

MIT
