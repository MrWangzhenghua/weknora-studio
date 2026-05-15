"""闪卡 Bridge FastAPI：接收知识库 Markdown 与主题，调用 LLM 生成闪卡 JSON。"""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from typing import Optional

from fastapi import Depends, FastAPI, File, Form, HTTPException, UploadFile, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
import httpx

from . import __version__
from .config import settings
from .generator import generate_flashcards_from_context
from .models import GenerateFlashcardMeta, GenerateFlashcardResponse, HealthResponse

logger = logging.getLogger(__name__)
_security = HTTPBearer(auto_error=False)


def _verify_token(credentials: Optional[HTTPAuthorizationCredentials] = Depends(_security)) -> None:
    """与 PPT Master Bridge 一致：配置了 BRIDGE_API_TOKEN 时校验 Bearer。"""
    expected = (settings.api_token or "").strip()
    if not expected:
        return
    if credentials is None or (credentials.credentials or "").strip() != expected:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="invalid or missing bearer token")


@asynccontextmanager
async def lifespan(app: FastAPI):
    logging.basicConfig(
        level=getattr(logging, (settings.log_level or "INFO").upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    logger.info("flashcard_bridge starting version=%s", __version__)
    yield
    logger.info("flashcard_bridge shutdown")


app = FastAPI(title="WeKnora Flashcard Bridge", version=__version__, lifespan=lifespan)


@app.get("/health", response_model=HealthResponse)
async def health() -> HealthResponse:
    return HealthResponse(ok=True, version=__version__)


@app.post(
    "/v1/generate",
    response_model=GenerateFlashcardResponse,
    dependencies=[Depends(_verify_token)],
)
async def generate_flashcards(
    meta: str = Form(..., description="JSON：GenerateFlashcardMeta"),
    files: list[UploadFile] = File(default_factory=list),
) -> GenerateFlashcardResponse:
    """multipart：meta（JSON）+ 若干 Markdown/文本文件，合并为上下文后生成闪卡。"""
    try:
        meta_obj = GenerateFlashcardMeta.model_validate_json(meta)
    except Exception as e:
        raise HTTPException(status_code=400, detail=f"invalid meta json: {e}") from e

    parts: list[str] = []
    for uf in files or []:
        raw = await uf.read()
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            text = raw.decode("utf-8", errors="replace")
        name = uf.filename or "document"
        parts.append(f"## FILE: {name}\n\n{text.strip()}\n")

    merged = "\n\n---\n\n".join(parts) if parts else ""
    max_chars = max(4096, int(settings.context_max_chars))
    if len(merged) > max_chars:
        merged = merged[:max_chars] + "\n\n[... truncated by flashcard bridge ...]"

    try:
        cards, msg = await generate_flashcards_from_context(
            settings=settings,
            topic=meta_obj.topic.strip(),
            count=int(meta_obj.count),
            language=(meta_obj.language or "zh").strip(),
            context_text=merged,
            override_base_url=meta_obj.flash_llm_base_url,
            override_model=meta_obj.flash_llm_model,
            override_api_key=meta_obj.flash_llm_api_key,
            override_timeout=meta_obj.flash_llm_timeout,
        )
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    except httpx.HTTPStatusError as e:
        code = e.response.status_code
        if code == 429:
            raise HTTPException(
                status_code=429,
                detail="上游 LLM 限流(429)：已自动退避重试仍失败，请稍后再试或提升智谱配额。",
            ) from e
        if code in (502, 503):
            raise HTTPException(
                status_code=502,
                detail=f"上游 LLM 暂不可用({code})，请稍后重试。",
            ) from e
        logger.exception("flashcard generate failed")
        raise HTTPException(status_code=500, detail=str(e)) from e
    except Exception as e:
        logger.exception("flashcard generate failed")
        raise HTTPException(status_code=500, detail=str(e)) from e

    citations: dict = {}
    if meta_obj.weknora_kb_id:
        citations["weknora_kb_id"] = meta_obj.weknora_kb_id
    return GenerateFlashcardResponse(
        topic=meta_obj.topic,
        flashcards=cards,
        citations=citations,
        message=msg,
    )


def run() -> None:
    import uvicorn

    uvicorn.run(
        "flashcard_bridge.app:app",
        host=settings.host,
        port=int(settings.port),
        log_level=(settings.log_level or "info").lower(),
    )
