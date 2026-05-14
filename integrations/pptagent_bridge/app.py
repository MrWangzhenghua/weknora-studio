"""PPTAgent Bridge FastAPI 应用入口。

接口约定：
- ``GET  /health``                       健康检查 / 配置探测
- ``POST /v1/generate``                  multipart 上传文件 + JSON meta 创建任务
- ``GET  /v1/tasks/{task_id}``           查询任务状态
- ``DELETE /v1/tasks/{task_id}``         取消任务
- ``GET  /v1/tasks/{task_id}/file``      下载最终 PPT
- ``GET  /v1/tasks``                     列举所有任务（调试用）

鉴权：所有非 health 接口都要求 ``Authorization: Bearer <BRIDGE_API_TOKEN>``，
当 BRIDGE_API_TOKEN 为空时关闭鉴权（仅推荐内网部署）。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import subprocess
from contextlib import asynccontextmanager
from pathlib import Path
from typing import List

import httpx
from fastapi import Depends, FastAPI, File, Form, HTTPException, UploadFile, status
from fastapi.responses import FileResponse, JSONResponse, Response
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from . import __version__
from .config import settings
from .generator import PPTGenerator
from .models import (
    CreateTaskResponse,
    GenerateRequestMetadata,
    HealthResponse,
    LLMTestRequest,
    LLMTestResponse,
    TaskInfoResponse,
)
from .task_manager import TaskManager, TaskNotFoundError, TaskStillRunningError

logger = logging.getLogger(__name__)
_security = HTTPBearer(auto_error=False)


@asynccontextmanager
async def lifespan(app: FastAPI):
    logging.basicConfig(
        level=getattr(logging, settings.log_level.upper(), logging.INFO),
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    settings.ensure_workspace()
    generator = PPTGenerator(settings)
    manager = TaskManager(settings, generator)
    await manager.start()
    app.state.manager = manager
    logger.info(
        "PPTAgent Bridge ready: workspace=%s lm_configured=%s vlm_configured=%s",
        settings.workspace_dir,
        settings.language_model.is_configured,
        settings.vision_model.is_configured,
    )
    try:
        yield
    finally:
        await manager.stop()


app = FastAPI(
    title="PPTAgent Bridge",
    description="WeKnora ↔ PPTAgent 桥接服务",
    version=__version__,
    lifespan=lifespan,
)


def _verify_token(credentials: HTTPAuthorizationCredentials = Depends(_security)) -> None:
    """简单的 Bearer Token 校验。"""

    if not settings.api_token:
        return
    if credentials is None or credentials.credentials != settings.api_token:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="invalid bridge api token",
        )


@app.get("/health", response_model=HealthResponse)
async def health() -> HealthResponse:
    manager: TaskManager = app.state.manager
    return HealthResponse(
        status="ok",
        version=__version__,
        language_model_configured=settings.language_model.is_configured,
        vision_model_configured=settings.vision_model.is_configured,
        workspace=str(settings.workspace_dir),
        queue_running=manager._started,
        active_tasks=manager.active_tasks(),
    )


@app.post(
    "/v1/generate",
    response_model=CreateTaskResponse,
    dependencies=[Depends(_verify_token)],
)
async def create_task(
    meta: str = Form(..., description="JSON 字符串，对应 GenerateRequestMetadata"),
    files: List[UploadFile] = File(default_factory=list),
) -> CreateTaskResponse:
    """创建一个新的 PPT 生成任务。

    入参约定：multipart/form-data，包含字段：
    - ``meta``  ：``GenerateRequestMetadata`` 的 JSON 字符串；
    - ``files`` ：N 个待用于生成的文件（pdf/docx/md/...）。
    """

    if not settings.language_model.is_configured:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="PPTAgent LLM 未配置，请设置 PPTAGENT_LLM_* 环境变量",
        )

    try:
        meta_obj = GenerateRequestMetadata.model_validate_json(meta)
    except Exception as exc:
        raise HTTPException(status_code=400, detail=f"meta JSON 不合法: {exc}")

    manager: TaskManager = app.state.manager
    # 先把上传的文件落盘到一个临时目录，再交给 manager 安排任务。
    staging_dir = settings.workspace_dir / "staging" / os.urandom(8).hex()
    staging_dir.mkdir(parents=True, exist_ok=True)
    saved_files: list[Path] = []
    total_bytes = 0
    max_bytes = settings.max_file_mb * 1024 * 1024

    for upload in files:
        if not upload.filename:
            continue
        target = staging_dir / Path(upload.filename).name
        # 流式落盘，避免一次性占用大量内存
        size = 0
        with target.open("wb") as fd:
            while True:
                chunk = await upload.read(1024 * 1024)
                if not chunk:
                    break
                size += len(chunk)
                if size > max_bytes:
                    fd.close()
                    target.unlink(missing_ok=True)
                    raise HTTPException(
                        status_code=413,
                        detail=f"单文件超过 {settings.max_file_mb}MB 限制: {upload.filename}",
                    )
                fd.write(chunk)
        await upload.close()
        total_bytes += size
        saved_files.append(target)

    if not saved_files and not meta_obj.instruction.strip():
        raise HTTPException(status_code=400, detail="必须至少提供一个输入文件或非空 instruction")

    request_dict = meta_obj.model_dump()
    record = await manager.submit_task(saved_files, request_dict, total_bytes)
    return CreateTaskResponse(
        task_id=record.task_id,
        status=record.status.value,
        message=record.message,
        created_at=record.created_at.isoformat(),
    )


@app.post(
    "/v1/test-llm",
    response_model=LLMTestResponse,
    dependencies=[Depends(_verify_token)],
)
async def test_llm(body: LLMTestRequest) -> LLMTestResponse:
    """探测 OpenAI 兼容 Chat Completions 是否可用（与全局设置「测试连接」行为一致）。"""

    base = body.base_url.rstrip("/")
    url = f"{base}/chat/completions"
    payload = {
        "model": body.model,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 4,
    }
    headers = {"Content-Type": "application/json"}
    if body.api_key:
        headers["Authorization"] = f"Bearer {body.api_key}"
    try:
        async with httpx.AsyncClient(timeout=float(body.timeout)) as client:
            resp = await client.post(url, json=payload, headers=headers)
        if resp.status_code >= 400:
            return LLMTestResponse(
                ok=False,
                message=f"HTTP {resp.status_code}: {resp.text[:500]}",
            )
        return LLMTestResponse(ok=True, message="ok")
    except Exception as exc:  # noqa: BLE001 - 返回给前端展示
        return LLMTestResponse(ok=False, message=str(exc))


@app.get(
    "/v1/tasks/{task_id}/preview",
    dependencies=[Depends(_verify_token)],
)
async def preview_task_result(task_id: str) -> FileResponse:
    """将成功的 pptx 转为 PDF 供浏览器内嵌预览（NotebookLM 风格）。"""

    manager: TaskManager = app.state.manager
    try:
        record = await manager.get_task(task_id)
    except TaskNotFoundError:
        raise HTTPException(status_code=404, detail="task not found")
    if record.status.value != "succeeded":
        raise HTTPException(status_code=409, detail="preview only available for succeeded tasks")
    pptx_path = Path(record.result_path)
    if not pptx_path.exists():
        raise HTTPException(status_code=404, detail="pptx missing")
    out_dir = pptx_path.parent
    pdf_path = out_dir / f"{pptx_path.stem}.pdf"
    # 若 pptx 更新过则重新转换
    if not pdf_path.exists() or pdf_path.stat().st_mtime < pptx_path.stat().st_mtime:

        def _convert() -> None:
            # 使用与镜像一致的 LibreOffice 无头模式
            for cand in ("libreoffice", "soffice"):
                try:
                    subprocess.run(
                        [
                            cand,
                            "--headless",
                            "--nologo",
                            "--nofirststartwizard",
                            "--convert-to",
                            "pdf",
                            "--outdir",
                            str(out_dir),
                            str(pptx_path),
                        ],
                        check=True,
                        timeout=180,
                        capture_output=True,
                    )
                    return
                except (FileNotFoundError, subprocess.CalledProcessError):
                    continue
            raise RuntimeError("LibreOffice 转换失败，请检查容器内是否安装 libreoffice")

        try:
            await asyncio.to_thread(_convert)
        except Exception as exc:
            logger.exception("preview convert failed")
            raise HTTPException(status_code=500, detail=str(exc)) from exc
    if not pdf_path.exists():
        raise HTTPException(status_code=500, detail="pdf not produced")
    return FileResponse(
        str(pdf_path),
        media_type="application/pdf",
        filename=pdf_path.name,
        headers={"Cache-Control": "private, max-age=60"},
    )


@app.delete(
    "/v1/tasks/{task_id}/permanent",
    status_code=status.HTTP_204_NO_CONTENT,
    response_class=Response,
    dependencies=[Depends(_verify_token)],
)
async def purge_task_permanent(task_id: str) -> Response:
    """永久删除任务记录及磁盘上的 PPT 与中间文件（仅终态可删）。"""

    manager: TaskManager = app.state.manager
    try:
        await manager.purge_task(task_id)
    except TaskNotFoundError:
        raise HTTPException(status_code=404, detail="task not found")
    except TaskStillRunningError:
        raise HTTPException(
            status_code=409,
            detail="任务尚未结束，请先取消或等待完成后再删除",
        )
    return Response(status_code=status.HTTP_204_NO_CONTENT)


@app.get(
    "/v1/tasks/{task_id}",
    response_model=TaskInfoResponse,
    dependencies=[Depends(_verify_token)],
)
async def get_task(task_id: str) -> TaskInfoResponse:
    manager: TaskManager = app.state.manager
    try:
        record = await manager.get_task(task_id)
    except TaskNotFoundError:
        raise HTTPException(status_code=404, detail="task not found")
    return TaskInfoResponse(**{
        k: v
        for k, v in record.to_dict().items()
        if k in TaskInfoResponse.model_fields  # type: ignore[attr-defined]
    })


@app.delete(
    "/v1/tasks/{task_id}",
    response_model=TaskInfoResponse,
    dependencies=[Depends(_verify_token)],
)
async def cancel_task(task_id: str) -> TaskInfoResponse:
    manager: TaskManager = app.state.manager
    try:
        record = await manager.cancel_task(task_id)
    except TaskNotFoundError:
        raise HTTPException(status_code=404, detail="task not found")
    return TaskInfoResponse(**{
        k: v
        for k, v in record.to_dict().items()
        if k in TaskInfoResponse.model_fields  # type: ignore[attr-defined]
    })


@app.get(
    "/v1/tasks/{task_id}/file",
    dependencies=[Depends(_verify_token)],
    response_class=FileResponse,
)
async def download_task_result(task_id: str) -> FileResponse:
    manager: TaskManager = app.state.manager
    try:
        record = await manager.get_task(task_id)
    except TaskNotFoundError:
        raise HTTPException(status_code=404, detail="task not found")
    if not record.result_path or not Path(record.result_path).exists():
        raise HTTPException(status_code=409, detail="result not ready")
    return FileResponse(
        record.result_path,
        media_type="application/vnd.openxmlformats-officedocument.presentationml.presentation",
        filename=Path(record.result_path).name,
    )


@app.get(
    "/v1/tasks",
    dependencies=[Depends(_verify_token)],
)
async def list_tasks() -> JSONResponse:
    manager: TaskManager = app.state.manager
    items = await manager.list_tasks()
    return JSONResponse({"items": [r.to_dict() for r in items], "total": len(items)})


def run() -> None:
    """命令行入口：``python -m pptagent_bridge`` 时调用。"""

    import uvicorn

    uvicorn.run(
        "pptagent_bridge.app:app",
        host=settings.host,
        port=settings.port,
        log_level=settings.log_level.lower(),
        proxy_headers=True,
    )


if __name__ == "__main__":  # pragma: no cover
    run()
