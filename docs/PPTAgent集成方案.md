# WeKnora × PPTAgent 集成方案与开发说明

> 目标：在 WeKnora 知识库页面提供「生成 PPT」入口，将当前知识库的所有文件作为输入，调用 [icip-cas/PPTAgent](https://github.com/icip-cas/PPTAgent) 自动生成高质量演示文稿，并把结果回传给用户下载。
>
> 本文档同时是开发交接物：包含架构、协议、接口规范、技术选型、部署步骤、异常处理与回归验证。

---

## 1. 系统架构

### 1.1 端到端时序图

```text
┌──────────┐     1. 点击「生成 PPT」     ┌────────────┐
│ Frontend │ ─────────────────────────▶ │ WeKnora-Go │
│   Vue    │                            │  App (8080)│
└────┬─────┘                            └─────┬──────┘
     │ 3 秒轮询任务状态                       │ 2. 查询 KB 内所有 knowledge
     │ /api/v1/ppt-tasks/{id}                 │ 3. KnowledgeService.GetKnowledgeFile → io.ReadCloser
     │                                        │ 4. multipart 流式上传至 Bridge
     │                                        ▼
     │                                ┌────────────────────┐
     │                                │ PPTAgent Bridge    │
     │                                │ FastAPI (Python)   │
     │                                │ 端口 8090          │
     │                                └─────┬──────────────┘
     │                                      │ 5. any2markdown / Document.from_markdown
     │                                      │ 6. PPTAgent.generate_pres
     │                                      │ 7. presentation.save(*.pptx)
     │                                      ▼
     │                                ┌────────────────────┐
     │                                │ /data/pptagent/    │  ← 生成产物 / 中间文件
     │                                │   ${task_id}/      │
     │                                │     output/*.pptx  │
     │                                └─────┬──────────────┘
     │ 8. 任务 status=succeeded               │
     │   GET /v1/tasks/{id}/file              │
     ▼                                        ▼
┌──────────┐    9. 浏览器另存为     ┌─────────────────────┐
│ Frontend │ ◀───────────────────── │  *.pptx (octet)     │
└──────────┘                        └─────────────────────┘
```

### 1.2 组件分工

| 组件                    | 语言/技术                | 职责                                                       |
| ----------------------- | ------------------------ | ---------------------------------------------------------- |
| Frontend (`frontend/`)  | Vue 3 + tdesign-vue-next | 入口按钮、参数表单、3 秒轮询任务、下载结果文件             |
| WeKnora App (`internal/`) | Go + Gin + dig         | REST 接口、任务路由、收集知识库文件、状态聚合              |
| PPTAgent Bridge         | Python 3.11 + FastAPI    | 接收 multipart 文件，调用 PPTAgent，维护任务进度与下载     |
| PPTAgent                | Python（上游开源）       | 真正的 PPT 生成核心，基于模板与多 Agent 流程               |

### 1.3 通信协议与数据格式

| 链路                       | 协议      | 数据格式                    | 鉴权                                       |
| -------------------------- | --------- | --------------------------- | ------------------------------------------ |
| Browser ↔ WeKnora App      | HTTP/JSON | application/json + 二进制流 | WeKnora 既有 JWT / API Key 鉴权（沿用）    |
| WeKnora App ↔ Bridge       | HTTP      | multipart/form-data + JSON  | `Authorization: Bearer <BRIDGE_API_TOKEN>` |
| Bridge ↔ PPTAgent          | 进程内调用 | Python 对象                 | -                                          |
| Bridge ↔ LLM/VLM           | HTTPS     | OpenAI Chat Completions     | API Key (per endpoint)                     |

**为什么选 HTTP/JSON + multipart 而不是 gRPC？**

- PPT 生成是低 QPS、高耗时（数十秒到数分钟）的任务，HTTP 的异步轮询模式开发成本最低、可观测性最好。
- 上传文件数量不固定（一个 KB 可能有几十份文档），multipart 比 Protobuf `bytes` 流更适合，且服务端 FastAPI 原生支持。
- Bridge 暴露 OpenAPI 文档（Swagger），方便后续接入其他客户端。

### 1.4 安全机制

1. **网络隔离**：`pptagent` 服务挂在 `WeKnora-network` 上，不直接暴露公网；`PPTAGENT_BRIDGE_PORT` 默认仅供调试。
2. **共享 Token**：WeKnora 后端在 `Authorization` 头携带 `PPTAGENT_BRIDGE_TOKEN`，Bridge 端通过 `BRIDGE_API_TOKEN` 校验。Token 通过 `.env` 注入，**生产环境必须替换为强随机字符串**。
3. **租户隔离**：WeKnora 端在 `meta.weknora_tenant_id / weknora_kb_id / weknora_user_id` 中透传上下文；Bridge 仅按 `task_id` 隔离文件存储，租户合规依赖 WeKnora 既有 RBAC。
4. **加密**：链路在 Docker 网络内部默认明文（同主机 bridge 网络），若跨主机部署 Bridge，应在 WeKnora App 与 Bridge 之间放置 TLS 终端（推荐 Caddy / Nginx + Let's Encrypt 或服务网格）。
5. **文件大小限制**：双层防护——WeKnora 后端 `PPTAGENT_BRIDGE_MAX_FILE_MB`、Bridge 端 `BRIDGE_MAX_FILE_MB`。
6. **任务 TTL**：Bridge 默认 24 小时后自动清理 `task_id` 对应的工作目录。

---

## 2. 功能实现步骤与里程碑

| 阶段 | 工作内容                                                                 | 交付物                                                            | 验收标准                                                                |
| ---- | ------------------------------------------------------------------------ | ----------------------------------------------------------------- | ----------------------------------------------------------------------- |
| A    | 前端入口：在 KB 页面新增「生成 PPT」按钮 + 参数表单 + 任务进度面板       | `frontend/src/views/knowledge/components/PPTGenerateDialog.vue`   | 任意 KB 页面可见按钮，禁用与权限保持一致                                |
| B    | 知识库文件提取：复用 `KnowledgeService.GetKnowledgeFile` 流式拉取        | `internal/application/service/pptgen_service.go::collectFiles`    | 跳过 FAQ / 未完成解析 / 已禁用的条目，并在响应中报告 `files_skipped`    |
| C    | 安全传输：multipart 流式 + Bearer Token                                  | `internal/application/service/pptgen_client.go`                   | `curl` 不带 Token 返回 401；大文件 100MB 上传成功                       |
| D    | PPTAgent 调用：Bridge HTTP 网关 + 任务队列                               | `integrations/pptagent_bridge/app.py` + `task_manager.py`         | `POST /v1/generate` 返回 task_id；轮询能拿到从 0% → 100% 的进度         |
| E    | 状态跟踪：WeKnora 暴露 `/ppt-tasks/{id}` 状态接口；前端 3s 轮询          | `internal/handler/pptgen.go::GetPPTGenTask`                       | 失败时 `error` 字段非空；成功时 `progress=100`，`status=succeeded`      |
| F    | 结果返回：流式下载 .pptx                                                 | `internal/handler/pptgen.go::DownloadPPTGenResult`                | PPTX 文件能在 PowerPoint / Keynote / WPS 打开，模板样式保留             |

### 测试用例摘要

| 用例 ID | 场景                                       | 预期结果                                            |
| ------- | ------------------------------------------ | --------------------------------------------------- |
| TC-01   | KB 有 1 个 markdown                        | 成功，PPT >= 5 页                                   |
| TC-02   | KB 有 1 个 PDF + 1 个 docx                 | 成功，PPT 包含来自两份文档的章节                    |
| TC-03   | KB 含 manual 知识                          | manual.metadata.content 作为文本输入                |
| TC-04   | KB 只有 FAQ 条目                           | 接口返回 400 `no eligible documents`                |
| TC-05   | 单文件超过 `PPTAGENT_BRIDGE_MAX_FILE_MB`   | Bridge 返回 413，WeKnora 透传 4xx 给前端            |
| TC-06   | Bridge 容器宕机                            | WeKnora 返回 500，前端展示错误并可重试              |
| TC-07   | LLM 配置错误                               | Bridge `/health` 返回 `language_model_configured=false`；创建任务返回 503 |
| TC-08   | 任务中途取消                               | `DELETE /v1/tasks/{id}` 立即返回 `cancelled` 状态   |
| TC-09   | 任务超过 `BRIDGE_TASK_TIMEOUT`             | 自动 `status=failed`，error 信息含 `任务超时`       |
| TC-10   | 容器重启                                   | 进行中的任务丢失，前端轮询返回 `task not found`，提示重试 |

---

## 3. 技术选型说明

| 关键环节         | 选型                                                | 选型理由 / 适用场景                                                          |
| ---------------- | --------------------------------------------------- | ---------------------------------------------------------------------------- |
| PPT 生成核心     | icip-cas/PPTAgent v1（模板模式）                    | 已经支持 PDF/DOCX/MD/HTML 等格式；模板生成稳定，输出标准 .pptx               |
| 文档解析         | PPTAgent 内置 `any2markdown`（基于 MarkItDown）     | 支持 PDF/Word/PPT/Excel/HTML 等主流格式，与 PPTAgent 自身管道完全对齐         |
| API 通信         | HTTP/1.1 + JSON / multipart-form                    | 协议门槛低、Swagger 自动生成、易于穿透 Nginx；长耗时任务用异步轮询模式      |
| 服务发现         | Docker Compose 服务名 + 内网 DNS                    | 与现有部署方式一致，零额外组件依赖                                          |
| 任务队列         | FastAPI 进程内 asyncio.Queue + 单 worker            | PPT 生成单任务即占用大量 CPU/LLM；先用最小实现，未来可替换 RQ / Celery       |
| 数据存储         | Bridge workspace 命名卷（`pptagent-data`）         | 中间产物大、临时性强；WeKnora 后端无需落库，任务记录保留在内存即可          |
| 鉴权             | Bearer Token + WeKnora 既有 JWT/API Key             | Token 短期共享密钥简单可靠；前端复用 WeKnora 登录态                          |
| 前端框架         | Vue 3 + TypeScript + tdesign-vue-next               | 与 WeKnora 既有前端栈一致，组件、样式开箱即用                                |
| 国际化           | vue-i18n（项目内既有）                              | 与现有页面无缝对齐                                                          |

> **关于是否引入 DeepPresenter？** DeepPresenter（PPTAgent V2）依赖 Playwright + html2pptx + Docker-in-Docker 沙箱，对宿主环境要求很高。本方案默认接入 PPTAgent V1 模板生成模式以保证可部署性；如确需引入，请参考 `integrations/PPTAgent/AGENTS.md` 单独构建 `deeppresenter-sandbox` 镜像并启用。

---

## 4. 接口规范（REST API）

> 接口均挂在 WeKnora 既有的 `/api/v1` 路由组下，自动继承 JWT / API Key 认证、租户校验、CORS 等中间件。

### 4.1 POST `/api/v1/knowledge-bases/{kb_id}/ppt-tasks` ｜ 创建任务

请求 Body（`application/json`）：

| 字段              | 类型     | 必填 | 约束 / 默认                                                                  | 说明                            |
| ----------------- | -------- | ---- | ---------------------------------------------------------------------------- | ------------------------------- |
| `instruction`     | string   | 否   | 长度 ≤ 4000                                                                  | 自定义提示词，留空使用默认指令  |
| `num_pages`       | integer  | 否   | 0 ≤ N ≤ 60；0 表示由模型决定                                                 | 期望幻灯片张数                  |
| `language`        | string   | 否   | `zh` / `en` / `auto`                                                         | 目标语言；不填走 auto           |
| `template`        | string   | 否   | 内置：`default` / `cip` / `hit` / `thu` / `ucas`                              | 模板名称                        |
| `title`           | string   | 否   | 默认 KB.name                                                                 | PPT 标题                        |
| `include_file_ids`| string[] | 否   | 知识 ID 数组；不传或空数组表示包含全部                                       | 仅指定文件时使用                |

成功响应（`HTTP 202`）：

```json
{
  "success": true,
  "data": {
    "task_id": "5b7a2c0e-...",
    "bridge_task_id": "8f4d...",
    "knowledge_base_id": "kb-001",
    "tenant_id": 1,
    "user_id": "u-abc",
    "title": "产品白皮书",
    "instruction": "",
    "num_pages": 12,
    "template": "default",
    "status": "pending",
    "progress": 0,
    "message": "已创建任务",
    "error": "",
    "files_included": 7,
    "files_skipped": 1,
    "created_at": "2026-05-12T15:31:25+08:00",
    "updated_at": "2026-05-12T15:31:25+08:00"
  }
}
```

错误码：

| HTTP | code                    | 触发条件                                                |
| ---- | ----------------------- | ------------------------------------------------------- |
| 400  | `bad_request`           | KB 内无可用文档；JSON 不合法                            |
| 401  | `unauthorized`          | 未登录 / 无 KB 访问权限                                 |
| 404  | `not_found`             | KB 不存在                                               |
| 413  | `payload_too_large`     | 单文件超过 `PPTAGENT_BRIDGE_MAX_FILE_MB`                |
| 500  | `internal_server_error` | Bridge 异常 / LLM 失败                                  |
| 503  | `service_unavailable`   | Bridge 健康检查失败（LLM 未配置等）                     |

### 4.2 GET `/api/v1/ppt-tasks/{task_id}` ｜ 查询任务

响应字段同 4.1 中的 `data`；后端会主动从 Bridge 同步进度。

### 4.3 DELETE `/api/v1/ppt-tasks/{task_id}` ｜ 取消任务

进入终态后再次取消是 no-op，返回任务最新状态。

### 4.4 GET `/api/v1/ppt-tasks/{task_id}/download` ｜ 下载结果

- 仅当 `status=succeeded` 时返回 `application/vnd.openxmlformats-officedocument.presentationml.presentation`
- Header 含 `Content-Disposition: attachment; filename="<title>.pptx"`

### 4.5 GET `/api/v1/knowledge-bases/{kb_id}/ppt-tasks` ｜ 列出最近任务

Query：`limit`（默认 20）。结果按创建时间倒序。

### 4.6 GET `/api/v1/ppt-tasks/health` ｜ Bridge 健康检查

返回 `{"success": true, "data": {"status": "ok"}}` 表示 Bridge 可用；否则 500。

### 4.7 Bridge 原始接口（仅供内部 / 调试）

| Path                              | Method | 说明                                |
| --------------------------------- | ------ | ----------------------------------- |
| `/health`                         | GET    | 健康检查 + LLM 配置状态             |
| `/v1/generate`                    | POST   | multipart：meta(JSON) + files       |
| `/v1/tasks/{task_id}`             | GET    | 查询单任务                          |
| `/v1/tasks/{task_id}`             | DELETE | 取消任务                            |
| `/v1/tasks/{task_id}/file`        | GET    | 下载 .pptx                          |
| `/v1/tasks`                       | GET    | 列举所有任务（调试）                |

完整 Schema 见 Bridge 启动后 `http://localhost:8090/docs`（FastAPI 自动生成的 Swagger）。

---

## 5. 部署与运维

### 5.1 环境要求

| 项目                  | 要求                                                                                                |
| --------------------- | --------------------------------------------------------------------------------------------------- |
| 操作系统              | Linux（推荐 Ubuntu 22.04+）；Windows 用户请使用 WSL2 + Docker Desktop                              |
| Docker / Docker Compose | Docker 24.0+，Compose v2                                                                          |
| CPU / RAM             | 推荐 8 vCPU / 16GB（PPT 生成多并发时建议加大；单 LLM 调用占用主要内存来自 Python 进程 + libreoffice） |
| 磁盘                  | 至少 10 GB（PPT 工作区 + 模板）                                                                    |
| 网络                  | 出网访问 LLM 提供方（OpenAI / Azure / Qwen 等）；推荐放在内网，仅暴露 Frontend 端口                |

### 5.2 启动步骤

```bash
# 1) 准备配置
cp .env.example .env
vim .env       # 至少填好 DB/Redis 密码、PPTAGENT_LLM_* 以及 PPTAGENT_BRIDGE_TOKEN

# 2) 启动核心服务 + PPTAgent
docker compose --profile pptagent up -d

# 3) 等待健康检查通过
docker compose ps
docker compose logs -f pptagent

# 4) 验证后端能调通 Bridge
curl -s http://localhost:8080/api/v1/ppt-tasks/health \
  -H "Authorization: Bearer <你的 WeKnora JWT>" | jq

# 5) 浏览器进入任意知识库，点击右上角「生成 PPT」
open http://localhost
```

> **WSL2/Mac 用户提示**：如果选择本地直跑（不进 Docker），可以 `pip install -e integrations/PPTAgent && pip install -r integrations/pptagent_bridge/requirements.txt && python -m pptagent_bridge`。

### 5.3 关键环境变量速查

| 变量                                  | 用途                                       | 默认                       |
| ------------------------------------- | ------------------------------------------ | -------------------------- |
| `PPTAGENT_BRIDGE_URL`                 | WeKnora App 调用 Bridge 的 URL              | `http://pptagent:8090`     |
| `PPTAGENT_BRIDGE_TOKEN`               | 双向共享 Token，必须与 Bridge 端一致         | 空（关闭鉴权，仅内网用）   |
| `PPTAGENT_BRIDGE_TIMEOUT_SEC`         | App 调 Bridge 单次请求超时（秒）            | 60                         |
| `PPTAGENT_BRIDGE_MAX_FILE_MB`         | 上传给 Bridge 的单文件上限（MB）            | 200                        |
| `PPTAGENT_LLM_BASE_URL` / `_MODEL` / `_API_KEY` | PPTAgent 主语言模型               | 必填，无默认               |
| `PPTAGENT_VLM_BASE_URL` / `_MODEL` / `_API_KEY` | PPTAgent 视觉模型                 | 留空回退到 LLM             |
| `PPTAGENT_TASK_TIMEOUT`               | 单任务最长执行秒数                          | 1800                       |
| `PPTAGENT_TASK_TTL_HOURS`             | 任务结果保留时长（小时）                    | 24                         |
| `PPTAGENT_MAX_CONCURRENCY`            | Bridge 内并发 worker 数                     | 1                          |
| `PPTAGENT_DEFAULT_TEMPLATE`           | 默认 PPT 模板名                            | `default`                  |
| `PPTAGENT_BRIDGE_PORT`                | 宿主机暴露的 Bridge 端口                    | 8090                       |

### 5.4 健康检查与故障排查

1. **容器健康检查**：`docker compose ps` 应显示 `pptagent` 为 `healthy`。
2. **检查 LLM 配置**：`curl http://localhost:8090/health` 必须返回 `language_model_configured: true`。
3. **检查 WeKnora 一侧**：`curl http://localhost:8080/api/v1/ppt-tasks/health -H "Authorization: Bearer $TOKEN"`。
4. **日志位置**：
   - WeKnora App：`docker compose logs app`
   - Bridge：`docker compose logs pptagent`
   - 单个任务的中间产物：容器内 `/data/pptagent/<task_id>/`，宿主机 `pptagent-data` 卷
5. **常见问题**：
   - 卡在 `drafting`：通常是 LLM 限流或网络抖动，检查 LLM 提供方控制台。
   - `language_model_configured=false`：检查 `PPTAGENT_LLM_BASE_URL`、`PPTAGENT_LLM_MODEL`、`PPTAGENT_LLM_API_KEY` 是否注入容器（`docker compose exec pptagent env | grep PPTAGENT`）。
   - PPTX 打开报错：确认模板 `source.pptx` 没被破坏；重置 `PPTAGENT_DEFAULT_TEMPLATE=default`。

### 5.5 性能测试方案

| 指标             | 工具                              | 目标                                  |
| ---------------- | --------------------------------- | ------------------------------------- |
| 单任务时延       | 计时 `task_id` 从创建到 succeeded | 5 份 markdown / 8000 字 ≤ 90s（GPT-4o）|
| 并发吞吐         | k6 / hey 调用 `/ppt-tasks`        | 默认配置下 1 任务/分钟（受 LLM 限速）  |
| 内存占用         | `docker stats`                    | pptagent 容器稳态 ≤ 2 GB              |
| 大文件上传       | `curl --upload-file`              | 200MB 单文件能稳定上传到 Bridge        |

调优建议：调大 `PPTAGENT_MAX_CONCURRENCY` 时同步提升 LLM 配额；模板/Markdown 越精简越快；中文/英文跨语言对齐场景考虑预翻译素材。

---

## 6. 异常处理矩阵

| 类别           | 触发条件                                       | 处理策略                                                                                | 用户反馈                              |
| -------------- | ---------------------------------------------- | --------------------------------------------------------------------------------------- | ------------------------------------- |
| 文件过大       | 单文件 > `PPTAGENT_BRIDGE_MAX_FILE_MB`         | WeKnora 端在 collectFiles 阶段拒收；用户可在 KB 编辑器开启 `extract_config` 拆分大文档  | toast：`单文件超过 200MB 限制`        |
| 格式不兼容     | 文件后缀未在 PPTAgent 支持表中                 | Bridge generator 回退为 "占位说明 + 文件名"，不会中断整批；前端 `files_skipped` 计数提示 | 详情面板提示 `已跳过 N 个文件`         |
| Bridge 不可用  | 任意 HTTP 调用返回 5xx / 网络错误              | WeKnora 端将错误包成 `internal_server_error`；前端弹错误提示，提供"重试"按钮             | 错误对话框 + 重试按钮                 |
| 服务健康检查失败 | `/ppt-tasks/health` 返回 5xx                   | 前端按钮长按 tooltip 显示 "PPT 服务暂不可用"；同时关闭入口可点击                          | tooltip + 灰化按钮                    |
| 网络异常 / 超时 | App → Bridge 60s 内无响应；或任务超时          | WeKnora 端使用 ctx 超时控制；Bridge 端 `BRIDGE_TASK_TIMEOUT` 自动标记失败                | 状态 `failed`，error 含具体描述       |
| 任务取消       | 用户点击「取消任务」按钮                       | WeKnora 调 Bridge `DELETE`，Bridge 将状态置为 `cancelled`，已生成的中间文件交由 GC 清理   | 状态 `cancelled`                      |
| 内容不足/质量差 | LLM 输出空 / 生成的 PPT < 3 张                  | Bridge generator 直接抛 `RuntimeError("PPTAgent 未能成功生成幻灯片")`                   | 失败状态 + 建议增补素材               |
| 鉴权失败       | Token 错误                                     | Bridge 返回 401；WeKnora 透传 500（避免泄露密钥）                                       | 系统排查项；不直接面向终端用户        |
| 容器重启       | 进行中的任务丢失                              | Bridge 内存任务表清空，状态查询返回 404；WeKnora 端把 task 标记为 failed 并提示重试       | toast：`PPTAgent 重启，请重试`        |
| 长任务保活     | 浏览器 tab 退出后再回来                        | 后端任务仍在执行；前端进入页面时通过 `ListTasksForKB` 拉回最新任务列表                   | 历史任务列表（如需，后续迭代展示）    |

---

## 7. 代码索引

| 路径                                                                       | 角色                                          |
| -------------------------------------------------------------------------- | --------------------------------------------- |
| `integrations/PPTAgent/`                                                   | 上游 PPTAgent 源码（保持不变）                |
| `integrations/pptagent_bridge/app.py`                                      | FastAPI 入口，对外暴露 REST                   |
| `integrations/pptagent_bridge/generator.py`                                | 调用 PPTAgent 进行 PPT 生成                   |
| `integrations/pptagent_bridge/task_manager.py`                             | 任务队列、状态机、GC                          |
| `integrations/pptagent_bridge/config.py`                                   | Bridge 环境变量读取                           |
| `docker/Dockerfile.pptagent`                                               | Bridge 镜像构建                               |
| `internal/types/pptgen.go`                                                 | Go 端任务结构体 / 状态常量                    |
| `internal/types/interfaces/pptgen.go`                                      | `PPTGenService` 接口定义                      |
| `internal/application/service/pptgen_client.go`                            | WeKnora → Bridge 的 HTTP 客户端               |
| `internal/application/service/pptgen_service.go`                           | PPTGen 业务服务实现                           |
| `internal/handler/pptgen.go`                                               | HTTP Handler                                  |
| `internal/router/router.go::RegisterPPTGenRoutes`                          | 路由注册                                      |
| `internal/container/container.go`                                          | dig 依赖注入                                  |
| `frontend/src/api/ppt-generate/index.ts`                                   | 前端 API 封装                                 |
| `frontend/src/views/knowledge/components/PPTGenerateDialog.vue`            | 对话框组件                                    |
| `frontend/src/views/knowledge/KnowledgeBase.vue`                           | 入口按钮 + 弹窗挂载                           |
| `docker-compose.yml`                                                       | `pptagent` 服务 + 命名卷 + profile            |
| `.env.example`                                                             | 集成相关环境变量示例                          |
| `docs/PPTAgent集成方案.md`                                                 | 本设计文档                                    |

---

## 8. 后续可演进方向

1. **持久化任务**：把 PPTGen 任务落 PostgreSQL，前端历史区展示。
2. **接入 DeepPresenter**：作为高级模式，开启视觉反思与 Free-form 生成，质量更优但部署复杂。
3. **接入 Asynq**：替换内存队列，实现多副本水平扩展、失败重试与可观测性。
4. **多模板预览**：前端在创建任务前预览模板缩略图，提升用户感知。
5. **流式进度**：从轮询升级为 SSE / WebSocket，进一步降低进度延迟。
