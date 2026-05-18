"""调用 OpenAI 兼容 Chat Completions，根据知识库摘录与主题生成 JSON 闪卡。"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from email.utils import parsedate_to_datetime
from typing import Any

import httpx

from .config import FlashcardBridgeSettings
from .models import FlashcardItem

logger = logging.getLogger(__name__)


def _retry_after_seconds(response: httpx.Response) -> float | None:
    """解析 Retry-After：秒数或 HTTP-date。"""
    raw = (response.headers.get("retry-after") or "").strip()
    if not raw:
        return None
    try:
        return max(0.5, float(raw))
    except ValueError:
        pass
    try:
        dt = parsedate_to_datetime(raw)
        if dt is None:
            return None
        return max(0.5, dt.timestamp() - time.time())
    except Exception:
        return None


async def _post_chat_with_retries(
    *,
    client: httpx.AsyncClient,
    url: str,
    headers: dict[str, str],
    payload: dict[str, Any],
    max_retries: int,
    base_delay: float,
    max_sleep: float,
) -> httpx.Response:
    """对 429 / 503 / 502 做指数退避重试，遵守 Retry-After。"""
    attempt = 0
    while True:
        r = await client.post(url, headers=headers, json=payload)
        if r.status_code not in (429, 502, 503):
            return r
        if attempt >= max_retries:
            return r
        ra = _retry_after_seconds(r)
        if ra is not None:
            wait = min(max_sleep, ra)
        else:
            wait = min(max_sleep, base_delay * (2**attempt))
        logger.warning(
            "flashcard LLM %s, sleeping %.1fs then retry (%d/%d)",
            r.status_code,
            wait,
            attempt + 1,
            max_retries,
        )
        await asyncio.sleep(wait)
        attempt += 1


def _normalize_base_url(url: str) -> str:
    u = (url or "").strip().rstrip("/")
    return u


def _is_maas_openai_base(base_url: str) -> bool:
    low = (base_url or "").lower()
    return "modelarts-maas.com" in low or "/api/paas/" in low or "open.bigmodel.cn" in low


def _chat_completions_url(base_url: str) -> str:
    """拼接 Chat Completions URL（与 pptmaster_bridge 一致：版本号已在 base 内时只加 /chat/completions）。"""
    b = _normalize_base_url(base_url)
    if not b:
        return ""
    low = b.lower()
    if "modelarts-maas.com" in low:
        # 常见误配 .../v2/v1 -> 规范为 .../v2
        b = re.sub(r"/v1/?$", "", b)
    if (
        "open.bigmodel.cn" in low
        or "/api/paas/" in low
        or "modelarts-maas.com" in low
        or re.search(r"/v\d+$", b)
    ):
        return f"{b}/chat/completions"
    return f"{b}/v1/chat/completions"


def _parse_json_cards(raw: str) -> list[FlashcardItem]:
    """从模型输出中解析 [{"front":"...","back":"..."}, ...]。"""
    text = (raw or "").strip()
    if not text:
        return []
    # 尝试从 markdown 代码块中取出 JSON
    m = re.search(r"```(?:json)?\s*([\s\S]*?)\s*```", text, re.IGNORECASE)
    if m:
        text = m.group(1).strip()
    # 若模型返回带前缀的对象包裹数组
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        # 再尝试截取第一个 [ 到最后一个 ]
        i, j = text.find("["), text.rfind("]")
        if i >= 0 and j > i:
            data = json.loads(text[i : j + 1])
        else:
            logger.warning("flashcard json parse failed, snippet=%s", text[:400])
            return []

    if isinstance(data, dict):
        if "flashcards" in data and isinstance(data["flashcards"], list):
            data = data["flashcards"]
        elif "cards" in data and isinstance(data["cards"], list):
            data = data["cards"]
        else:
            return []

    if not isinstance(data, list):
        return []

    out: list[FlashcardItem] = []
    for item in data:
        if not isinstance(item, dict):
            continue
        front = str(item.get("front") or item.get("question") or item.get("q") or "").strip()
        back = str(item.get("back") or item.get("answer") or item.get("a") or "").strip()
        if front and back:
            out.append(FlashcardItem(front=front, back=back))
    return out


async def generate_flashcards_from_context(
    *,
    settings: FlashcardBridgeSettings,
    topic: str,
    count: int,
    language: str,
    context_text: str,
    override_base_url: str | None,
    override_model: str | None,
    override_api_key: str | None,
    override_timeout: int | None,
) -> tuple[list[FlashcardItem], str]:
    """基于合并后的知识库上下文与主题，调用 LLM 生成闪卡列表。"""

    base = (override_base_url or settings.llm_base_url or "").strip()
    model = (override_model or settings.llm_model or "").strip()
    api_key = (override_api_key or settings.llm_api_key or "").strip()
    timeout = float(override_timeout or settings.llm_timeout or 120)

    if not base or not model or not api_key:
        raise ValueError("未配置有效的 OpenAI 兼容 LLM（请设置 FLASHCARD_LLM_* 或在请求 meta 中传入 flash_llm_*）")

    url = _chat_completions_url(base)
    if not url:
        raise ValueError("无效的 LLM base_url")
    logger.info("flashcard LLM url=%s model=%s", url, model)

    lang_hint = "请全部使用简体中文。" if language.lower().startswith("zh") else "Please answer in English."

    system = (
        "你是学习助手，根据用户提供的「知识库摘录」围绕「用户主题」制作闪卡。"
        "必须只输出一个 JSON 数组，元素为对象，键名固定为 front 与 back，不要其它说明文字。"
        f"{lang_hint}"
    )
    user = (
        f"用户主题：{topic}\n"
        f"需要大约 {count} 张闪卡（可略多略少，但不要超过 {count + 5} 张）。\n\n"
        "知识库摘录如下（可能经截断）：\n---\n"
        f"{context_text}\n---\n"
        '输出示例：[{"front":"问题","back":"答案"}]'
    )

    payload: dict[str, Any] = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
        "temperature": 0.4,
    }
    if settings.llm_max_output_tokens > 0:
        cap = settings.llm_max_output_tokens
        if _is_maas_openai_base(base):
            cap = max(cap, 8192)
        low_base = base.lower()
        if "modelarts-maas.com" in low_base:
            payload["max_tokens"] = cap
        elif _is_maas_openai_base(base):
            payload["max_completion_tokens"] = cap
        else:
            payload["max_completion_tokens"] = cap

    if _is_maas_openai_base(base):
        payload["extra_body"] = {"chat_template_kwargs": {"enable_thinking": False}}

    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }

    async with httpx.AsyncClient(timeout=timeout) as client:
        r = await _post_chat_with_retries(
            client=client,
            url=url,
            headers=headers,
            payload=payload,
            max_retries=max(0, settings.llm_max_retries),
            base_delay=max(0.5, settings.llm_retry_base_delay_sec),
            max_sleep=max(1.0, settings.llm_retry_max_sleep_sec),
        )
        r.raise_for_status()
        body = r.json()

    try:
        content = body["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as e:
        logger.exception("unexpected chat response: %s", body)
        raise ValueError("LLM 响应格式异常") from e

    cards = _parse_json_cards(content if isinstance(content, str) else str(content))
    if len(cards) > count + 5:
        cards = cards[: count + 5]
    msg = f"已生成 {len(cards)} 张闪卡"
    return cards, msg
