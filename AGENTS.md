# AGENTS.md

## 项目说明

Index-Translate-Docker 是 bilibili/Index-Translate 免费公共 API 的本地桥接代理. 使用 Go 重写 `inference/llm/call_api.py --serve` 的行为, 为浏览器扩展提供 OpenAI 兼容接口, 并提供 Docker 部署方式.

## 目录结构

- `main.go`: 代理实现, 包含参数解析, 请求转发, CORS 响应头与默认值注入.
- `main_test.go`: 代理行为测试, 使用 httptest 伪上游, 不访问真实网络.
- `Dockerfile`: 多阶段构建, 容器内以 `--serve --host 0.0.0.0` 启动.
- `docker-compose.yml`: 本地构建镜像并发布 `8080` 端口.
- `README.md`: 使用说明.
- `.github/workflows/docker.yml`: 构建并推送 GHCR 多架构镜像, 标签规则见 "镜像发布".

## 常用命令

```sh
go build -o index-translate-proxy .
go vet ./...
go test ./...
go run . --serve --port 18080
```

## 镜像发布

- 镜像 `ghcr.io/yeqingky/index-translate-docker` 由 GitHub Actions 构建, 平台为 `linux/amd64` 与 `linux/arm64`.
- 分支 push 使用 7 位提交短 hash 作为镜像 tag; `vX.Y.Z` tag 发布 `X.Y.Z` 与 `latest`.
- Pull request 只构建验证, 不推送镜像; 工作流不创建 GitHub Release.

## 约定

- 默认监听 `127.0.0.1:8080`; 仅在明确需要时使用 `--host 0.0.0.0`.
- 上游地址默认为 `https://index-translate.bilibili.com/v1`, 通过 `--api-base` 覆盖.
- 代理行为与 `call_api.py --serve` 保持一致: 路径映射, CORS 响应头, `enable_thinking=false` 与 `temperature=0` 默认值, 上游错误状态码与响应体透传.
- 额外能力: 非流式 `/v1/chat/completions` 收到上游 504 时以流式重试并组装响应; 上游网关对约 10 秒静默的请求返回 504.
- 修改代理行为时同步更新 `main_test.go` 与本文件.
