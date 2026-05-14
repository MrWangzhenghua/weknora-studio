"""Bridge 配置（环境变量与 [genai-rag](https://github.com/alikomurcu/genai-rag) 的 OpenRouter 用法对齐）。"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _env(name: str, default: str = "") -> str:
    v = os.getenv(name)
    if v is None:
        return default
    return v.strip()


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


@dataclass
class Settings:
    bridge_host: str
    bridge_port: int
    bridge_api_token: str
    openrouter_base_url: str
    openrouter_api_key: str
    openrouter_model: str
    llm_timeout_sec: int
    llm_max_output_tokens: int
    document_max_chars: int


def load_settings() -> Settings:
    return Settings(
        bridge_host=_env("BRIDGE_HOST", "0.0.0.0"),
        bridge_port=_env_int("BRIDGE_PORT", 8091),
        bridge_api_token=_env("BRIDGE_API_TOKEN", ""),
        openrouter_base_url=_env("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1").rstrip("/"),
        openrouter_api_key=_env("OPENROUTER_API_KEY", ""),
        openrouter_model=_env("OPENROUTER_MODEL", "openai/gpt-4o-mini"),
        llm_timeout_sec=_env_int("LLM_TIMEOUT_SEC", 300),
        llm_max_output_tokens=_env_int("LLM_MAX_OUTPUT_TOKENS", 8192),
        document_max_chars=_env_int("DOCUMENT_MAX_CHARS", 120_000),
    )


settings = load_settings()
