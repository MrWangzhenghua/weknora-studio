"""Bridge 配置：从环境变量读取，与 PPTAgent Bridge 风格对齐。"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _env(name: str, default: str | None = None) -> str | None:
    v = os.getenv(name)
    if v is None:
        return default
    v = v.strip()
    return v if v else default


def _env_int(name: str, default: int) -> int:
    raw = _env(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    raw = _env(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


@dataclass(frozen=True)
class FlashcardBridgeSettings:
    """闪卡 Bridge 运行时参数。"""

    api_token: str = ""
    host: str = "0.0.0.0"
    port: int = 8091
    log_level: str = "INFO"
    # 合并多文件后的上下文最大字符数（近似控制 token）
    context_max_chars: int = 120000
    # 默认 LLM（可被请求 meta 中的 flash_llm_* 覆盖）
    llm_base_url: str = ""
    llm_model: str = ""
    llm_api_key: str = ""
    llm_timeout: int = 120
    llm_max_output_tokens: int = 8192
    # 上游 429/503 时重试（智谱等限流）
    llm_max_retries: int = 6
    llm_retry_base_delay_sec: float = 2.0
    llm_retry_max_sleep_sec: float = 60.0

    @classmethod
    def from_env(cls) -> "FlashcardBridgeSettings":
        return cls(
            api_token=_env("BRIDGE_API_TOKEN", "") or "",
            host=_env("BRIDGE_HOST", "0.0.0.0") or "0.0.0.0",
            port=_env_int("BRIDGE_PORT", 8091),
            log_level=_env("BRIDGE_LOG_LEVEL", "INFO") or "INFO",
            context_max_chars=_env_int("FLASHCARD_CONTEXT_MAX_CHARS", 120000),
            llm_base_url=_env("FLASHCARD_LLM_BASE_URL", "") or "",
            llm_model=_env("FLASHCARD_LLM_MODEL", "") or "",
            llm_api_key=_env("FLASHCARD_LLM_API_KEY", "") or "",
            llm_timeout=_env_int("FLASHCARD_LLM_TIMEOUT", 120),
            llm_max_output_tokens=_env_int("FLASHCARD_LLM_MAX_OUTPUT_TOKENS", 8192),
            llm_max_retries=_env_int("FLASHCARD_LLM_MAX_RETRIES", 6),
            llm_retry_base_delay_sec=_env_float("FLASHCARD_LLM_RETRY_BASE_DELAY_SEC", 2.0),
            llm_retry_max_sleep_sec=_env_float("FLASHCARD_LLM_RETRY_MAX_SLEEP_SEC", 60.0),
        )


settings = FlashcardBridgeSettings.from_env()
