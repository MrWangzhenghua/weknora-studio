"""调用 OpenAI 兼容 Chat Completions，根据知识库摘录与主题生成 JSON 闪卡。"""

from __future__ import annotations

import json
import logging
import re
from typing import Any

import httpx

from .config import FlashcardBridgeSettings
from .models import FlashcardItem

logger = logging.getLogger(__name__)


def _normalize_base_url(url: str) -> str:
    u = (url or "").strip().rstrip("/")
    return u


def _chat_completions_url(base_url: str) -> str:
    b = _normalize_base_url(base_url)
    if not b:
        return ""
    if "/v1" in b:
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
        payload["max_tokens"] = settings.llm_max_output_tokens

    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }

    async with httpx.AsyncClient(timeout=timeout) as client:
        r = await client.post(url, headers=headers, json=payload)
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
