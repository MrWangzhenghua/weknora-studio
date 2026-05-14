# PPTAgent Bridge

把 [icip-cas/PPTAgent](https://github.com/icip-cas/PPTAgent) 包装成 HTTP 服务，供 WeKnora 后端通过 REST 调用。

Docker 镜像构建时会在 Dockerfile 内 **浅克隆** 上述仓库（无需在宿主仓库里再放一份 `integrations/PPTAgent`）；构建环境需能访问 Git。可通过环境变量 `PPTAGENT_GIT_REPO` / `PPTAGENT_GIT_REF` 覆盖（见根目录 `.env.example`）。

> 完整的集成方案、接口规范、部署指南请阅读：[`../../docs/PPTAgent集成方案.md`](../../docs/PPTAgent集成方案.md)。

## 一句话用法

```bash
# 仅启动 Bridge（前后端独立测试）
cp ../../.env.example ../../.env
# 至少填好 PPTAGENT_LLM_BASE_URL / _MODEL / _API_KEY
docker compose -f ../../docker-compose.yml --profile pptagent up -d pptagent

# 探活
curl http://localhost:8090/health
```

## 接口一览

| Path                          | Method | 鉴权                | 说明                       |
| ----------------------------- | ------ | ------------------- | -------------------------- |
| `/health`                     | GET    | 无                  | 健康检查 + 配置探测        |
| `/v1/generate`                | POST   | Bearer Token        | multipart 上传文件创建任务 |
| `/v1/tasks/{task_id}`         | GET    | Bearer Token        | 查询任务状态               |
| `/v1/tasks/{task_id}`         | DELETE | Bearer Token        | 取消任务                   |
| `/v1/tasks/{task_id}/file`    | GET    | Bearer Token        | 下载 PPT 结果              |
| `/v1/tasks`                   | GET    | Bearer Token        | 列举任务（调试用）         |

详细 Schema 参见 `http://localhost:8090/docs`（FastAPI 自动生成的 Swagger）。

## 目录说明

```
pptagent_bridge/
├── __init__.py        # 包元信息
├── __main__.py        # `python -m pptagent_bridge` 启动入口
├── app.py             # FastAPI 应用 + 路由
├── config.py          # 环境变量集中读取
├── generator.py       # 调用 PPTAgent 的核心逻辑
├── models.py          # 数据模型（任务记录 + 请求 / 响应）
├── task_manager.py    # 任务队列 / 状态机 / GC
├── requirements.txt   # Python 依赖（Bridge 自身）
└── README.md          # 本文件
```

## 配置（环境变量）

| 变量                              | 默认值                | 说明                                                  |
| --------------------------------- | --------------------- | ----------------------------------------------------- |
| `BRIDGE_HOST`                     | `0.0.0.0`             | 监听地址                                              |
| `BRIDGE_PORT`                     | `8090`                | 监听端口                                              |
| `BRIDGE_WORKSPACE`                | `/data/pptagent`      | 工作区目录                                            |
| `BRIDGE_LOG_LEVEL`                | `INFO`                | 日志级别                                              |
| `BRIDGE_API_TOKEN`                | 空                    | 调用方共享 Token；空表示关闭鉴权                      |
| `BRIDGE_TASK_TIMEOUT`             | `1800`                | 单任务最长执行秒数                                    |
| `BRIDGE_TASK_TTL_HOURS`           | `24`                  | 任务结果保留时长（小时）                              |
| `BRIDGE_MAX_CONCURRENCY`          | `1`                   | 同时执行的任务数                                      |
| `BRIDGE_MAX_FILE_MB`              | `200`                 | 单文件上限                                            |
| `PPTAGENT_LLM_BASE_URL`           | 空（必填）            | 主语言模型 OpenAI 兼容 base url                       |
| `PPTAGENT_LLM_MODEL`              | 空（必填）            | 主语言模型名称                                        |
| `PPTAGENT_LLM_API_KEY`            | 空（必填）            | 主语言模型 API Key                                    |
| `PPTAGENT_LLM_TIMEOUT`            | `600`                 | LLM 请求超时                                          |
| `PPTAGENT_LLM_MAX_OUTPUT_TOKENS`  | `65536`               | 单次补全输出上限；MaaS 下还会注入 `max_tokens` 并保底 |
| `PPTAGENT_LLM_MAAS_COMPAT`        | URL 含 `modelarts-maas.com` 时为 `true` | 华为 MaaS OpenAI 兼容模式 |
| `PPTAGENT_LLM_DISABLE_THINKING`   | MaaS 时默认 `true`    | `extra_body.chat_template_kwargs.enable_thinking=false`，减少 reasoning 占满额度导致 `parse` 失败 |
| `PPTAGENT_VLM_*` / `PPTAGENT_VLM_MAAS_COMPAT` / `PPTAGENT_VLM_DISABLE_THINKING` | 同上逻辑 | 视觉模型（可选） |
| `PPTAGENT_VLM_BASE_URL` / 等       | 空                    | 视觉模型（可选；未配置时回退到 LLM）                  |
| `PPTAGENT_DEFAULT_TEMPLATE`       | `default`             | 默认 PPT 模板                                         |

## 本地开发（不进 Docker）

```bash
# 需 Python 3.11+
cd integrations
pip install -e PPTAgent
pip install -r pptagent_bridge/requirements.txt

export PPTAGENT_LLM_BASE_URL=https://api.openai.com/v1
export PPTAGENT_LLM_MODEL=gpt-4o-mini
export PPTAGENT_LLM_API_KEY=sk-xxxx
export BRIDGE_API_TOKEN=dev-token

python -m pptagent_bridge
```

之后可访问 `http://localhost:8090/docs` 进行交互测试。

## 故障排查

- `language_model_configured=false`：检查 `PPTAGENT_LLM_*` 环境变量是否注入容器。
- 任务卡在 `drafting`：通常是 LLM 限流或网络抖动，先看 LLM 提供方控制台。
- **`Could not parse response content as the length limit was reached`**（华为 MaaS / 深度思考模型常见）：思维链 `reasoning_tokens` 与可见 `content` 共享输出上限，PPTAgent 使用 `chat.completions.parse` 时 JSON 被截断会触发该错误。处理：保持 **`PPTAGENT_LLM_MAAS_COMPAT=true`**（对 `modelarts-maas.com` 自动开启），并默认 **`PPTAGENT_LLM_DISABLE_THINKING=true`**；仍失败时提高 **`PPTAGENT_LLM_MAX_OUTPUT_TOKENS`**（如 `131072`）。**DeepSeek-R1** 等若网关不支持关闭思考，只能依赖更大输出上限。
- 输出 PPT 打不开：检查模板 `source.pptx` 是否完好；尝试切换 `PPTAGENT_DEFAULT_TEMPLATE=default`。
