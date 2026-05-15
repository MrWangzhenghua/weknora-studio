"""桥接服务用到的 Pydantic / dataclass 模型。"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

from pydantic import BaseModel, Field


class TaskStatus(str, enum.Enum):
    """PPT 生成任务的状态机。"""

    PENDING = "pending"          # 等待 worker 处理
    EXTRACTING = "extracting"    # 解析输入文件 / 整合素材
    DRAFTING = "drafting"        # 生成大纲与章节内容
    RENDERING = "rendering"      # 渲染幻灯片
    SUCCEEDED = "succeeded"      # 成功，结果文件已就绪
    FAILED = "failed"            # 失败
    CANCELLED = "cancelled"      # 用户主动取消

    def is_terminal(self) -> bool:
        return self in {TaskStatus.SUCCEEDED, TaskStatus.FAILED, TaskStatus.CANCELLED}


@dataclass
class TaskRecord:
    """单个 PPT 生成任务的运行时记录。

    数据保存在内存里（生产环境可替换为 Redis / DB），重启即丢失。
    """

    task_id: str
    status: TaskStatus = TaskStatus.PENDING
    progress: int = 0
    message: str = ""
    error: str = ""
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    # 任务的工作目录，存放输入文件 / 中间产物 / 最终 pptx。
    workspace: str = ""
    # 最终结果（pptx）相对 workspace 的路径。
    result_path: str = ""
    # 用户原始请求参数。
    request: Dict[str, Any] = field(default_factory=dict)
    # 输入文件数量与总字节数（用于上层统计）。
    input_files: int = 0
    input_bytes: int = 0

    def touch(self) -> None:
        self.updated_at = datetime.now(timezone.utc)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "task_id": self.task_id,
            "status": self.status.value,
            "progress": self.progress,
            "message": self.message,
            "error": self.error,
            "created_at": self.created_at.isoformat(),
            "updated_at": self.updated_at.isoformat(),
            "input_files": self.input_files,
            "input_bytes": self.input_bytes,
            "has_result": bool(self.result_path),
            "request": self.request,
        }


# ============== HTTP 请求 / 响应模型 ==============


class GenerateRequestMetadata(BaseModel):
    """``POST /v1/generate`` 表单中的 ``meta`` 字段（JSON 字符串）。"""

    instruction: str = Field("", description="生成 PPT 的用户提示词")
    num_pages: Optional[int] = Field(
        None, ge=1, le=80, description="期望生成的幻灯片张数；为空表示由模型自动决定"
    )
    language: Optional[str] = Field(
        None, description="目标语言：zh / en；默认根据输入内容自动识别"
    )
    template: Optional[str] = Field(
        None, description="历史字段；PPT Master 路径下不使用固定 .pptx 模板"
    )
    title: Optional[str] = Field(None, description="知识库名 / 演示主题，用作 PPT 标题")
    # 透传字段，便于 WeKnora 关联任务上下文。
    weknora_tenant_id: Optional[int] = Field(None, description="租户 ID")
    weknora_kb_id: Optional[str] = Field(None, description="知识库 ID")
    weknora_user_id: Optional[str] = Field(None, description="发起生成的用户 ID")
    # 透传任意扩展字段。
    extra: Dict[str, Any] = Field(default_factory=dict)
    # ---------- 单次任务覆盖的 LLM / VLM（由 WeKnora 从全局模型配置解析后下发）----------
    # 若四项齐全则本任务使用此处配置，否则回退到环境变量 PPTMASTER_LLM_* / PPTMASTER_VLM_*。
    ppt_llm_base_url: Optional[str] = Field(None, description="OpenAI 兼容 Chat Completions 根 URL")
    ppt_llm_model: Optional[str] = Field(None, description="模型名称")
    ppt_llm_api_key: Optional[str] = Field(None, description="API Key")
    ppt_llm_timeout: Optional[int] = Field(None, ge=10, le=3600, description="请求超时秒数")
    ppt_vlm_base_url: Optional[str] = None
    ppt_vlm_model: Optional[str] = None
    ppt_vlm_api_key: Optional[str] = None
    ppt_vlm_timeout: Optional[int] = Field(None, ge=10, le=3600)


class CreateTaskResponse(BaseModel):
    task_id: str
    status: str
    message: str
    created_at: str


class TaskInfoResponse(BaseModel):
    task_id: str
    status: str
    progress: int
    message: str
    error: str
    created_at: str
    updated_at: str
    input_files: int
    input_bytes: int
    has_result: bool


class LLMTestRequest(BaseModel):
    """测试 OpenAI 兼容接口是否可用。"""

    base_url: str = Field(..., description="例如 https://api.openai.com/v1")
    model: str = Field(..., description="模型 id")
    api_key: str = Field("", description="可为空（少数内网网关不需要）")
    timeout: int = Field(30, ge=5, le=120)


class LLMTestResponse(BaseModel):
    ok: bool
    message: str = ""


class HealthResponse(BaseModel):
    status: str
    version: str
    language_model_configured: bool
    vision_model_configured: bool
    workspace: str
    queue_running: bool
    active_tasks: int
