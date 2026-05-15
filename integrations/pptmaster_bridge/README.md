# PPT Master Bridge（Python 包 `pptmaster_bridge`）

将 [hugohe3/ppt-master](https://github.com/hugohe3/ppt-master) 的 **svg_to_pptx 原生形状导出** 与 WeKnora **REST 契约** 对接。环境变量统一为 **`PPTMASTER_*`**（与 Go 后端 `PPTMASTER_BRIDGE_*` 一致）。

Docker 镜像构建时浅克隆 ppt-master，构建参数：`PPTMASTER_GIT_REPO` / `PPTMASTER_GIT_REF`（见根目录 `.env.example`）。

## 启动（Compose）

```bash
# 在仓库根目录，至少配置 .env 中的 PPTMASTER_LLM_* 与 PPTMASTER_BRIDGE_TOKEN
docker compose --profile pptmaster up -d pptmaster
curl http://localhost:8090/health
```

`--profile full` 会一并拉起 `pptmaster` 服务。

## 本地开发（不进 Docker）

```bash
git clone https://github.com/hugohe3/ppt-master.git
cd ppt-master && pip install -r skills/ppt-master/requirements.txt

export PPTMASTER_REPO_ROOT=/path/to/ppt-master
export PYTHONPATH=$PPTMASTER_REPO_ROOT/skills/ppt-master/scripts
pip install -r integrations/pptmaster_bridge/requirements.txt

export PPTMASTER_LLM_BASE_URL=https://api.openai.com/v1
export PPTMASTER_LLM_MODEL=gpt-4o-mini
export PPTMASTER_LLM_API_KEY=sk-xxxx
export BRIDGE_API_TOKEN=dev-token   # 可选

python -m pptmaster_bridge
```

## 目录

```
pptmaster_bridge/
├── app.py / generator.py / task_manager.py / config.py / models.py
└── ...
```

## 排障

- `language_model_configured=false`：检查 `PPTMASTER_LLM_*`。
- 导出失败：确认 `skills/ppt-master/requirements.txt` 已安装；查看任务 workspace 下 `svg_final/*.svg`。
- 配图：可选配置 `PEXELS_API_KEY` / `PIXABAY_API_KEY`（见 ppt-master 文档）。
