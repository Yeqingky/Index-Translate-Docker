# AGENTS.md

## 项目说明

Index-Translate-Docker 是 bilibili/Index-Translate 免费公共 API 的本地桥接代理. 使用 Go 重写 `inference/llm/call_api.py --serve` 的行为, 为浏览器扩展提供 OpenAI 兼容接口, 并提供 Docker 部署方式.

## 目录结构

- `main.go`: 代理实现, 包含参数解析, 请求转发, CORS 响应头与默认值注入.
- `main_test.go`: 代理行为测试, 使用 httptest 伪上游, 不访问真实网络.
- `Dockerfile`: 多阶段构建, 容器内以 `--serve --host 0.0.0.0` 启动.
- `docker-compose.yml`: 本地构建镜像并发布 `8080` 端口.
- `README.md`: 使用说明.

## 常用命令

```sh
go build -o index-translate-proxy .
go vet ./...
go test ./...
go run . --serve --port 18080
```

## 约定

- 默认监听 `127.0.0.1:8080`; 仅在明确需要时使用 `--host 0.0.0.0`.
- 上游地址默认为 `https://index-translate.bilibili.com/v1`, 通过 `--api-base` 覆盖.
- 代理行为需与 `call_api.py --serve` 保持一致: 路径映射, CORS 响应头, `enable_thinking=false` 与 `temperature=0` 默认值, 上游错误状态码与响应体透传.
- 修改代理行为时同步更新 `main_test.go` 与本文件.
