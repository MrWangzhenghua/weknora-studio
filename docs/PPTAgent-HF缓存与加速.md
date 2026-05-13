# PPTAgent 容器：Hugging Face 缓存与加速（简体中文）

本文说明 **WeKnora `pptagent` 服务** 为何会访问 `huggingface.co`、如何 **持久化缓存** 加速二次启动，以及如何配置 **`HF_TOKEN`** 减少限流与告警。

## 一、现象说明

首次生成 PPT 或首次启动容器时，日志可能出现：

- `GET https://huggingface.co/.../julien-c/fasttext-language-id/...`
- `Warning: You are sending unauthenticated requests to the HF Hub. Please set a HF_TOKEN...`

含义：PPTAgent / 其依赖在运行时从 **Hugging Face Hub** 拉取小模型（例如语言识别用的 `lid.176.bin`）。**第一次**需要下载，耗时取决于网络；若容器无持久化缓存，**每次重建容器**都可能重复下载。

## 二、本仓库已做的代码与编排改动

1. **`docker/Dockerfile.pptagent`**  
   - 设置 `HF_HOME=/data/huggingface`  
   - 创建目录 `/data/huggingface`  
   - 声明数据卷：`/data/pptagent` 与 `/data/huggingface`

2. **`docker-compose.yml`（`pptagent` 服务）**  
   - 环境变量：`HF_HOME=/data/huggingface`、`HF_TOKEN=${HF_TOKEN:-}`  
   - 命名卷：`pptagent-hf-cache:/data/huggingface`（与 `pptagent-data` 分离，职责清晰）

3. **`.env.example`**  
   - 增加 `HF_TOKEN` 说明注释（勿把真实 Token 提交到 Git）

## 三、操作步骤（推荐）

### 步骤 1：在 `.env` 中配置 Token（可选但强烈建议）

1. 打开 [Hugging Face Token 设置](https://huggingface.co/settings/tokens)，创建一个 **Read** 权限的 Token。  
2. 在服务器项目根目录的 `.env` 中增加一行（不要把 Token 写进仓库里的示例文件再提交）：

```bash
HF_TOKEN=hf_你的只读Token
```

3. 保存后重启 `pptagent`：

```bash
docker compose --profile pptagent up -d pptagent
```

效果：匿名请求警告通常消失，Hub 限流更宽松，**首次下载往往更稳、更快**。

### 步骤 2：确认缓存卷已挂载（使用本仓库 compose 时默认已有）

执行：

```bash
docker compose --profile pptagent config | grep -A2 pptagent-hf-cache
```

应能看到 `pptagent-hf-cache` 挂载到容器内 `/data/huggingface`。

### 步骤 3：首次预热（可选，减少「第一次点生成 PPT」的等待）

部署完成后执行一次「生成 PPT」，或进入容器触发依赖加载：

```bash
docker compose exec pptagent ls -la /data/huggingface/hub 2>/dev/null || docker compose exec pptagent ls -la /data/huggingface
```

若目录下出现 `models--...` 等子目录，说明缓存已落盘；**之后重启容器**，只要 **未删除命名卷** `pptagent-hf-cache`，一般不会重复大文件下载。

### 步骤 4：不要误删卷

以下命令会删除 HF 缓存卷，下次会重新下载：

```bash
docker compose down -v   # 慎用：-v 会删卷
```

仅重启服务、不删卷：

```bash
docker compose --profile pptagent restart pptagent
```

## 四、国内网络或 HF 访问慢时的思路

- 在合规前提下使用 **企业代理** 或 **镜像站**（需自行调研 `HF_ENDPOINT` 等变量是否与当前 `huggingface_hub` 版本兼容）。  
- 或在可访问 Hub 的机器上 **先拉取缓存**，再将 `pptagent-hf-cache` 卷数据备份到目标环境（运维向，需熟悉 Docker volume 备份）。

## 五、验证清单

| 检查项 | 说明 |
|--------|------|
| `.env` 含 `HF_TOKEN` | 日志中 HF 匿名告警应减少或消失 |
| 存在卷 `pptagent-hf-cache` | `docker volume ls \| grep pptagent-hf-cache` |
| 二次生成不再长时间卡在 HF | 同版本镜像重启后仍保留 `/data/huggingface` 内容 |

## 六、与「生成质量」的关系

缓存与 **`HF_TOKEN`** 主要解决 **下载慢、限流、重复冷启动** 问题；幻灯片内容质量仍取决于 **知识库解析、传入 Bridge 的 Markdown、LLM 与模板**。二者可同时优化。
