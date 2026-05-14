"""与 FastAPI 请求/响应对齐的 Pydantic 模型。"""

from __future__ import annotations

from typing import Any, List, Optional

from pydantic import BaseModel, Field


class FlashcardItem(BaseModel):
    """单张闪卡：正面为问题/提示，背面为答案/详解。"""

    front: str = Field(..., description="闪卡正面（问题或关键词）")
    back: str = Field(..., description="闪卡背面（答案或解释）")


class GenerateFlashcardMeta(BaseModel):
    """multipart 中 meta 字段的 JSON 结构。"""

    topic: str = Field(..., min_length=1, description="生成主题，用于聚焦检索语义")
    count: int = Field(default=10, ge=1, le=50, description="期望闪卡数量")
    language: str = Field(default="zh", description="输出语言，如 zh / en")
    weknora_tenant_id: Optional[int] = None
    weknora_kb_id: Optional[str] = None
    weknora_user_id: Optional[str] = None
    # 单次请求覆盖的 OpenAI 兼容端点（由 WeKnora 从租户模型解析后下发）
    flash_llm_base_url: Optional[str] = None
    flash_llm_model: Optional[str] = None
    flash_llm_api_key: Optional[str] = None
    flash_llm_timeout: Optional[int] = None


class GenerateFlashcardResponse(BaseModel):
    """生成结果：与 genai-rag 返回字段风格接近，便于前端统一处理。"""

    topic: str
    flashcards: List[FlashcardItem] = Field(default_factory=list)
    citations: dict[str, Any] = Field(default_factory=dict)
    message: str = ""


class HealthResponse(BaseModel):
    ok: bool = True
    service: str = "flashcard_bridge"
    version: str = ""
