"""Bridge 运行配置：集中读取环境变量，提供给 FastAPI 应用与生成器使用。

设计目标：
- 单一来源：所有可调参数集中在 ``BridgeSettings`` 中，避免散落到各处的 ``os.getenv`` 调用。
- 友好默认：未配置时给出能开箱运行 / 抛出友好错误的默认值。
- 易于覆盖：所有字段均支持通过环境变量覆盖，方便 Docker / Compose 部署。
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


def _env(name: str, default: Optional[str] = None) -> Optional[str]:
    """读取环境变量并去除两端空白，未设置时返回默认值。"""

    value = os.getenv(name)
    if value is None:
        return default
    value = value.strip()
    if value == "":
        return default
    return value


def _env_int(name: str, default: int) -> int:
    raw = _env(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    raw = _env(name)
    if raw is None:
        return default
    return raw.lower() in {"1", "true", "yes", "y", "on"}


def _is_modelarts_maas_url(url: Optional[str]) -> bool:
    """华为云 ModelArts MaaS OpenAI 兼容网关（含 v1/v2 路径）。"""

    if not url:
        return False
    return "modelarts-maas.com" in url.lower()


@dataclass(frozen=True)
class LLMEndpoint:
    """单个 LLM 端点配置（OpenAI 兼容协议）。"""

    base_url: str
    model: str
    api_key: str
    timeout: int = 600

    @property
    def is_configured(self) -> bool:
        return bool(self.base_url) and bool(self.model) and bool(self.api_key)


@dataclass(frozen=True)
class BridgeSettings:
    """PPTAgent Bridge 全部运行时配置。"""

    # ===== 鉴权 =====
    # 与 WeKnora 共享的服务间 Token，避免 PPTAgent 被外部直接调用。
    api_token: str = ""

    # ===== 服务运行 =====
    host: str = "0.0.0.0"
    port: int = 8090
    workspace_dir: Path = Path("/data/pptagent")
    log_level: str = "INFO"
    # 任务最长生存时间（小时），超期清理结果与临时文件。
    task_ttl_hours: int = 24
    # 每个生成任务的最长允许时间（秒），超时自动失败。
    task_timeout: int = 1800
    # 同时运行的最大生成任务数（worker 并发上限）。
    max_concurrency: int = 1
    # 单次任务最大输入文件大小（MB），用于服务侧二次保护。
    max_file_mb: int = 200

    # ===== LLM / VLM 配置 =====
    # 语言模型（必填）：用于生成大纲、章节内容等。
    language_model: LLMEndpoint = field(default_factory=lambda: LLMEndpoint("", "", ""))
    # 视觉模型（可选）：用于图片描述与视觉理解，未配置时回退到语言模型。
    vision_model: LLMEndpoint = field(default_factory=lambda: LLMEndpoint("", "", ""))
    # 生成幻灯片所使用的模板名称（位于 pptagent/templates 目录下）。
    default_template: str = "default"
    # 单次 Chat 补全的最大输出 token（写入 max_completion_tokens），减轻结构化 JSON 被截断无法解析。
    # 设为 0 表示不注入，完全沿用模型网关默认行为。
    llm_max_output_tokens: int = 0
    # VLM 单独上限；为 0 时回退为与 llm_max_output_tokens 相同。
    vlm_max_output_tokens: int = 0
    # 对华为 ModelArts MaaS 等网关做兼容：同时传 max_tokens、提高下限，并可选关闭「深度思考」以保留 JSON 输出额度。
    llm_maas_compat: bool = False
    llm_disable_thinking: bool = False
    vlm_maas_compat: bool = False
    vlm_disable_thinking: bool = False
    # 多文件合并后的 Markdown 最大字符数（Python 3 的 len 为 Unicode 码点数），与上游 PPTAgent 的预警尺度对齐。
    markdown_max_chars: int = 28000

    @classmethod
    def from_env(cls) -> "BridgeSettings":
        """从环境变量构建 ``BridgeSettings``。

        变量约定：
        - ``BRIDGE_*``：网关自身行为控制。
        - ``PPTAGENT_LLM_*``：主语言模型。
        - ``PPTAGENT_VLM_*``：视觉模型，可选。
        """

        language = LLMEndpoint(
            base_url=_env("PPTAGENT_LLM_BASE_URL", "") or "",
            model=_env("PPTAGENT_LLM_MODEL", "") or "",
            api_key=_env("PPTAGENT_LLM_API_KEY", "") or "",
            timeout=_env_int("PPTAGENT_LLM_TIMEOUT", 600),
        )
        vision = LLMEndpoint(
            base_url=_env("PPTAGENT_VLM_BASE_URL", "") or "",
            model=_env("PPTAGENT_VLM_MODEL", "") or "",
            api_key=_env("PPTAGENT_VLM_API_KEY", "") or "",
            timeout=_env_int("PPTAGENT_VLM_TIMEOUT", 600),
        )
        workspace = Path(_env("BRIDGE_WORKSPACE", "/data/pptagent") or "/data/pptagent")
        # 推理模型 + 结构化 JSON 需要更大 completion 上限；MaaS 下 bridge 还会再抬下限。
        llm_max_out = _env_int("PPTAGENT_LLM_MAX_OUTPUT_TOKENS", 65536)
        vlm_max_out = _env_int("PPTAGENT_VLM_MAX_OUTPUT_TOKENS", 0)
        md_max = _env_int("PPTAGENT_MARKDOWN_MAX_CHARS", 28000)

        llm_maas_compat = _env_bool(
            "PPTAGENT_LLM_MAAS_COMPAT", _is_modelarts_maas_url(language.base_url)
        )
        vlm_maas_compat = _env_bool(
            "PPTAGENT_VLM_MAAS_COMPAT",
            _is_modelarts_maas_url(vision.base_url) if vision.is_configured else False,
        )

        # 未显式配置时：仅在对 MaaS 端点时默认关闭深度思考，避免 reasoning 占满额度导致 parse 失败。
        raw_llm_dt = os.getenv("PPTAGENT_LLM_DISABLE_THINKING")
        if raw_llm_dt is None or str(raw_llm_dt).strip() == "":
            llm_disable_thinking = llm_maas_compat
        else:
            llm_disable_thinking = _env_bool("PPTAGENT_LLM_DISABLE_THINKING", False)

        raw_vlm_dt = os.getenv("PPTAGENT_VLM_DISABLE_THINKING")
        if raw_vlm_dt is None or str(raw_vlm_dt).strip() == "":
            vlm_disable_thinking = vlm_maas_compat and vision.is_configured
        else:
            vlm_disable_thinking = _env_bool("PPTAGENT_VLM_DISABLE_THINKING", False)

        return cls(
            api_token=_env("BRIDGE_API_TOKEN", "") or "",
            host=_env("BRIDGE_HOST", "0.0.0.0") or "0.0.0.0",
            port=_env_int("BRIDGE_PORT", 8090),
            workspace_dir=workspace,
            log_level=_env("BRIDGE_LOG_LEVEL", "INFO") or "INFO",
            task_ttl_hours=_env_int("BRIDGE_TASK_TTL_HOURS", 24),
            task_timeout=_env_int("BRIDGE_TASK_TIMEOUT", 1800),
            max_concurrency=_env_int("BRIDGE_MAX_CONCURRENCY", 1),
            max_file_mb=_env_int("BRIDGE_MAX_FILE_MB", 200),
            language_model=language,
            vision_model=vision,
            default_template=_env("PPTAGENT_DEFAULT_TEMPLATE", "default") or "default",
            llm_max_output_tokens=llm_max_out,
            vlm_max_output_tokens=vlm_max_out,
            markdown_max_chars=md_max,
            llm_maas_compat=llm_maas_compat,
            llm_disable_thinking=llm_disable_thinking,
            vlm_maas_compat=vlm_maas_compat,
            vlm_disable_thinking=vlm_disable_thinking,
        )

    def ensure_workspace(self) -> None:
        """确保 workspace 目录存在，缺失时自动创建。"""

        self.workspace_dir.mkdir(parents=True, exist_ok=True)


# 模块级单例，import 时即从环境读取（FastAPI 启动后再校验 LLM 配置）。
settings = BridgeSettings.from_env()
