"""基于 PPT Master 导出链路的 PPT 生成。"""

流程概要：
1. 将知识库上传文件转为 Markdown（优先调用 ppt-master 仓库内 ``source_to_md`` 脚本）；
2. 使用 OpenAI 兼容 Chat Completions 生成结构化大纲（页数、主题色、配图检索词等）；
3. 可选调用 ``image_search.py`` 拉取免版权素材到 ``images/``；
4. 逐页请求模型输出符合 DrawingML 转换子集的单页 SVG，写入 ``svg_final/``；
5. 通过 ppt-master 的 ``create_pptx_with_native_svg`` 组装为原生可编辑 ``.pptx``。

环境变量 ``PPTMASTER_REPO_ROOT`` 指向 ppt-master 仓库根目录（Dockerfile 默认为 ``/opt/ppt-master``）。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Awaitable, Callable, List, Optional

import httpx

from .config import BridgeSettings, LLMEndpoint
from .models import TaskRecord, TaskStatus

logger = logging.getLogger(__name__)

ProgressCallback = Callable[[TaskStatus, int, str], Awaitable[None]]

_TEXT_EXTENSIONS = {".md", ".markdown", ".txt", ".rst"}
_DOC_EXTENSIONS = {
    ".pdf",
    ".docx",
    ".doc",
    ".odt",
    ".rtf",
    ".pptx",
    ".ppt",
    ".html",
    ".htm",
    ".xlsx",
    ".xls",
    ".xlsm",
    ".csv",
    ".epub",
    ".ipynb",
    ".tex",
    ".latex",
    ".rst",
    ".org",
    ".typ",
}
_IMAGE_EXTENSIONS = {".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".svg"}


def _repo_root() -> Path:
    return Path(os.environ.get("PPTMASTER_REPO_ROOT", "/opt/ppt-master")).resolve()


def _scripts_dir() -> Path:
    return _repo_root() / "skills" / "ppt-master" / "scripts"


def _ensure_sys_path_for_export() -> None:
    sd = str(_scripts_dir())
    if sd not in sys.path:
        sys.path.insert(0, sd)


def _is_export_available() -> bool:
    try:
        _ensure_sys_path_for_export()
        from svg_to_pptx.pptx_builder import create_pptx_with_native_svg  # noqa: F401

        return True
    except Exception:
        return False


@dataclass
class GeneratorContext:
    settings: BridgeSettings
    record: TaskRecord
    progress_cb: ProgressCallback


class PPTGenerator:
    """使用 PPT Master 导出栈的生成器（类名保持 ``PPTGenerator`` 以兼容 task_manager）。"""

    def __init__(self, settings: BridgeSettings):
        self.settings = settings

    async def run_task(
        self,
        record: TaskRecord,
        input_files: List[Path],
        progress_cb: ProgressCallback,
    ) -> Path:
        if not _is_export_available():
            raise RuntimeError(
                "未找到 PPT Master 仓库或 svg_to_pptx 依赖，请确认镜像已克隆 "
                "https://github.com/hugohe3/ppt-master 并完成 pip install -r skills/ppt-master/requirements.txt"
            )

        ctx = GeneratorContext(settings=self.settings, record=record, progress_cb=progress_cb)
        await ctx.progress_cb(TaskStatus.EXTRACTING, 5, "正在合并知识库素材")

        workspace = Path(record.workspace)
        workspace.mkdir(parents=True, exist_ok=True)
        markdown_dir = workspace / "markdown"
        markdown_dir.mkdir(exist_ok=True)
        images_dir = workspace / "images"
        images_dir.mkdir(exist_ok=True)
        svg_dir = workspace / "svg_final"
        svg_dir.mkdir(exist_ok=True)
        notes_dir = workspace / "notes"
        notes_dir.mkdir(exist_ok=True)
        exports_dir = workspace / "exports"
        exports_dir.mkdir(exist_ok=True)

        for f in input_files:
            if f.suffix.lower() in _IMAGE_EXTENSIONS and f.is_file():
                try:
                    shutil.copy2(f, images_dir / f.name)
                except OSError:
                    logger.warning("copy image asset failed: %s", f)

        combined = await self._to_combined_markdown(ctx, input_files, markdown_dir)
        await ctx.progress_cb(TaskStatus.DRAFTING, 25, "已整理正文，正在生成演示结构")

        req = record.request or {}
        endpoint = _resolve_llm_endpoint(self.settings, req, role="llm")
        if not endpoint.is_configured:
            raise RuntimeError(
                "未配置 PPTMASTER_LLM_BASE_URL / PPTMASTER_LLM_MODEL / PPTMASTER_LLM_API_KEY "
                "（沿用原环境变量名，无需改 WeKnora 配置）"
            )

        title = (req.get("title") or "知识库演示").strip()
        num_pages = req.get("num_pages")
        try:
            num_pages_i = int(num_pages) if num_pages is not None else 0
        except (TypeError, ValueError):
            num_pages_i = 0
        instruction = (req.get("instruction") or "").strip()
        language = (req.get("language") or "").strip()

        outline = await self._llm_outline(
            ctx,
            endpoint,
            combined,
            title=title,
            target_pages=num_pages_i,
            instruction=instruction,
            language=language,
        )
        slides = outline.get("slides") or []
        if not isinstance(slides, list) or len(slides) < 1:
            raise RuntimeError("模型返回的大纲无效（slides 为空）")

        await ctx.progress_cb(TaskStatus.DRAFTING, 40, "检索配图（可选）")
        await self._fetch_slide_images(slides, images_dir, workspace)

        await ctx.progress_cb(TaskStatus.RENDERING, 45, "逐页生成 SVG")
        svg_paths: list[Path] = []
        total = len(slides)
        theme = outline.get("theme") if isinstance(outline.get("theme"), dict) else {}
        for i, slide in enumerate(slides):
            if not isinstance(slide, dict):
                continue
            stem = _slide_stem(i, slide)
            svg_text = await self._llm_one_slide_svg(
                ctx,
                endpoint,
                combined=combined,
                title=title,
                theme=theme,
                slide=slide,
                slide_index=i + 1,
                slide_total=total,
                images_dir=images_dir,
            )
            path = svg_dir / f"{stem}.svg"
            path.write_text(svg_text, encoding="utf-8")
            svg_paths.append(path)
            notes = (slide.get("speaker_notes") or slide.get("notes") or "").strip()
            if notes:
                (notes_dir / f"{stem}.md").write_text(notes, encoding="utf-8")
            pct = 45 + int(40 * (i + 1) / max(total, 1))
            await ctx.progress_cb(TaskStatus.RENDERING, min(pct, 85), f"已生成第 {i + 1}/{total} 页")

        if not svg_paths:
            raise RuntimeError("未生成任何 SVG 页面")

        _ensure_sys_path_for_export()
        from svg_to_pptx.pptx_builder import create_pptx_with_native_svg

        out_name = _sanitize_filename(title) + ".pptx"
        out_path = exports_dir / out_name
        await ctx.progress_cb(TaskStatus.RENDERING, 88, "正在导出 PPTX（PPT Master 原生形状）")

        def _export_pptx() -> bool:
            return create_pptx_with_native_svg(
                svg_paths,
                out_path,
                canvas_format="ppt169",
                verbose=False,
                transition="fade",
                transition_duration=0.5,
                use_compat_mode=False,
                enable_notes=False,
                use_native_shapes=True,
                animation="fade",
                animation_duration=0.35,
                animation_stagger=0.35,
                animation_trigger="after-previous",
            )

        ok = await asyncio.to_thread(_export_pptx)
        if not ok or not out_path.exists():
            raise RuntimeError("PPT Master svg_to_pptx 导出失败，请检查依赖（svglib/reportlab 等）与 SVG 合法性")

        await ctx.progress_cb(TaskStatus.SUCCEEDED, 100, "PPT 生成成功")
        return out_path

    async def _to_combined_markdown(
        self,
        ctx: GeneratorContext,
        files: List[Path],
        markdown_dir: Path,
    ) -> str:
        repo = _repo_root()
        scripts = _scripts_dir()
        chunks: List[str] = []
        total = max(len(files), 1)
        for idx, file_path in enumerate(files):
            ext = file_path.suffix.lower()
            title = file_path.stem
            text, placeholder = await asyncio.to_thread(
                self._file_to_markdown_sync, file_path, ext, markdown_dir, idx, repo, scripts
            )
            text = (text or "").strip()
            if placeholder or not text:
                logger.error(
                    "[ppt-master-bridge] cannot extract text from %s (ext=%s)",
                    file_path.name,
                    ext,
                )
                text = (
                    f"<!-- 此文件未解析出正文 -->\n"
                    f"(无可用正文，仅文件名：{file_path.name})"
                )
            chunk = f"# {title}\n\n{text}\n"
            chunks.append(chunk)
            (markdown_dir / f"{idx:03d}_{title}.md").write_text(chunk, encoding="utf-8")
            progress = 5 + int(15 * (idx + 1) / total)
            await ctx.progress_cb(
                TaskStatus.EXTRACTING,
                progress,
                f"已处理 {idx + 1}/{len(files)} 个文件：{file_path.name}",
            )
        combined = "\n\n---\n\n".join(chunks)
        max_chars = max(4096, self.settings.markdown_max_chars)
        if len(combined) > max_chars:
            logger.warning(
                "combined markdown len=%d > markdown_max_chars=%d, truncating",
                len(combined),
                max_chars,
            )
            combined = combined[:max_chars] + (
                "\n\n<!-- 输入过长已截断；可调大 PPTMASTER_MARKDOWN_MAX_CHARS -->\n"
            )
        (markdown_dir / "_combined.md").write_text(combined, encoding="utf-8")
        return combined

    def _file_to_markdown_sync(
        self,
        file_path: Path,
        ext: str,
        markdown_dir: Path,
        idx: int,
        repo: Path,
        scripts: Path,
    ) -> tuple[str, bool]:
        placeholder = False
        try:
            if ext in _TEXT_EXTENSIONS:
                return file_path.read_text(encoding="utf-8", errors="ignore"), False
            if ext == ".csv" or ext == ".tsv":
                return file_path.read_text(encoding="utf-8", errors="ignore"), False
            work_copy = markdown_dir / f"_{idx:03d}_{file_path.name}"
            shutil.copy2(file_path, work_copy)
            out_md = work_copy.with_suffix(".md")
            script: Optional[Path] = None
            if ext == ".pdf":
                script = scripts / "source_to_md" / "pdf_to_md.py"
            elif ext in {".pptx", ".ppt", ".pptm", ".ppsx"}:
                script = scripts / "source_to_md" / "ppt_to_md.py"
            elif ext in {".xlsx", ".xlsm"} or ext == ".xls":
                script = scripts / "source_to_md" / "excel_to_md.py"
            elif ext in _DOC_EXTENSIONS:
                script = scripts / "source_to_md" / "doc_to_md.py"
            if script is None or not script.is_file():
                placeholder = True
                return "", placeholder
            cmd = [
                sys.executable,
                str(script),
                str(work_copy),
                "-o",
                str(out_md),
            ]
            r = subprocess.run(
                cmd,
                cwd=str(repo),
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=600,
            )
            if r.returncode != 0 or not out_md.is_file():
                logger.warning(
                    "converter failed for %s: rc=%s stderr=%s",
                    file_path.name,
                    r.returncode,
                    (r.stderr or "")[:500],
                )
                placeholder = True
                return "", placeholder
            return out_md.read_text(encoding="utf-8", errors="ignore"), False
        except Exception:
            logger.exception("markdown conversion error: %s", file_path)
            return "", True

    async def _llm_outline(
        self,
        ctx: GeneratorContext,
        endpoint: LLMEndpoint,
        combined: str,
        *,
        title: str,
        target_pages: int,
        instruction: str,
        language: str,
    ) -> dict[str, Any]:
        hint_pages = target_pages if 3 <= target_pages <= 40 else 12
        user_extra = ""
        if instruction:
            user_extra += f"\n附加说明（可选）: {instruction}\n"
        if language:
            user_extra += f"\n目标语言代码: {language}\n"
        user = (
            f"演示标题: {title}\n"
            f"建议页数: 约 {hint_pages} 页（可适当增减）\n"
            f"{user_extra}\n"
            "以下为知识库合并正文，请据此规划幻灯片：\n-----\n"
            f"{combined[: min(len(combined), 24000)]}\n"
        )
        sys_msg = (
            "你是演示文稿策划。只输出一个 JSON 对象，不要 Markdown 围栏。"
            "字段: theme: {primary, accent} 两个主色十六进制;"
            "slides: 数组，每项含 n(序号从1), role(cover|content|section|closing),"
            " title, bullets(字符串数组), speaker_notes, image_query(可空,英文关键词用于配图检索)。"
            "内容语言与素材一致。封面含标题与副标题要点。"
        )
        raw = await _chat_text(
            endpoint,
            self.settings,
            [{"role": "system", "content": sys_msg}, {"role": "user", "content": user}],
            json_mode=True,
        )
        data = _parse_json_object(raw)
        if not isinstance(data, dict):
            raise RuntimeError("大纲 JSON 解析失败")
        return data

    async def _fetch_slide_images(self, slides: list[Any], images_dir: Path, workspace: Path) -> None:
        script = _scripts_dir() / "image_search.py"
        if not script.is_file():
            return
        for i, slide in enumerate(slides):
            if not isinstance(slide, dict):
                continue
            q = slide.get("image_query")
            if not q or not str(q).strip():
                continue
            stem = _slide_stem(i, slide)
            fname = f"{stem}_photo.jpg"
            cmd = [
                sys.executable,
                str(script),
                str(q).strip(),
                "--filename",
                fname,
                "-o",
                str(images_dir),
                "--orientation",
                "landscape",
                "--slide",
                stem,
                "--min-width",
                "800",
                "--min-height",
                "600",
            ]
            try:
                r = await asyncio.to_thread(
                    subprocess.run,
                    cmd,
                    cwd=str(_repo_root()),
                    capture_output=True,
                    text=True,
                    encoding="utf-8",
                    errors="replace",
                    timeout=120,
                )
                if r.returncode != 0:
                    logger.info("image_search skip slide %s: %s", stem, (r.stderr or "")[:200])
            except Exception:
                logger.exception("image_search failed for %s", stem)

    async def _llm_one_slide_svg(
        self,
        ctx: GeneratorContext,
        endpoint: LLMEndpoint,
        *,
        combined: str,
        title: str,
        theme: dict[str, Any],
        slide: dict[str, Any],
        slide_index: int,
        slide_total: int,
        images_dir: Path,
    ) -> str:
        primary = str(theme.get("primary") or "#1e3a5f")
        accent = str(theme.get("accent") or "#f97316")
        bullets = slide.get("bullets") or []
        if isinstance(bullets, str):
            bullets = [bullets]
        elif not isinstance(bullets, list):
            bullets = []
        btxt = "\n".join(f"- {b}" for b in bullets if str(b).strip())
        available_images = sorted(p.name for p in images_dir.iterdir() if p.is_file())[:40]
        sys_msg = (
            "你是 SVG 幻灯片设计师。输出**仅**一个完整的 SVG 文档根元素，"
            "不要任何解释文字。viewBox 必须为 \"0 0 1280 720\"，"
            "xmlns=\"http://www.w3.org/2000/svg\"。\n"
            "使用可转换为基础 DrawingML 的子集：<rect> <circle> <text> <image> <line> <path> <g>。\n"
            "整页背景请放在带 id 含 background 的 <g> 内（全屏 rect）。\n"
            "文字用 <text>，设置 font-family=\"Arial\" 或 \"Noto Sans SC\"，fill 对比度足够。\n"
            "若需引用本地配图，仅使用相对路径 images/文件名（目录已存在）。\n"
            f"主题色: primary={primary}, accent={accent}。\n"
            f"当前第 {slide_index}/{slide_total} 页，角色: {slide.get('role','content')}。"
        )
        user = (
            f"整套演示标题: {title}\n"
            f"本页标题: {slide.get('title')}\n"
            f"要点:\n{btxt}\n\n"
            f"可用图片文件: {available_images}\n\n"
            "知识摘录（供事实引用，勿逐字堆砌）：\n"
            f"{combined[:8000]}\n"
        )
        raw = await _chat_text(
            endpoint,
            self.settings,
            [{"role": "system", "content": sys_msg}, {"role": "user", "content": user}],
            json_mode=False,
        )
        svg = _extract_svg_document(raw)
        if not svg:
            raise RuntimeError(f"第 {slide_index} 页未获得合法 SVG")
        return svg


def _slide_stem(idx: int, slide: dict[str, Any]) -> str:
    sn = slide.get("n")
    try:
        n = int(sn)
    except (TypeError, ValueError):
        n = idx + 1
    slug = re.sub(r"[^0-9a-zA-Z\u4e00-\u9fff]+", "_", str(slide.get("title") or "slide")).strip("_")
    slug = (slug[:40] or "slide").lower()
    return f"{n:02d}_{slug}"


def _parse_json_object(text: str) -> Any:
    text = text.strip()
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    m = re.search(r"\{[\s\S]*\}\s*$", text)
    if m:
        try:
            return json.loads(m.group(0))
        except json.JSONDecodeError:
            pass
    fence = re.search(r"```(?:json)?\s*([\s\S]*?)```", text, re.I)
    if fence:
        try:
            return json.loads(fence.group(1).strip())
        except json.JSONDecodeError:
            pass
    return None


def _extract_svg_document(text: str) -> str:
    t = text.strip()
    if t.startswith("<svg"):
        return t
    m = re.search(r"(<svg[\s\S]*?</svg>)", t, re.I)
    if m:
        return m.group(1).strip()
    fence = re.search(r"```(?:svg)?\s*([\s\S]*?)```", t, re.I)
    if fence and "<svg" in fence.group(1):
        inner = fence.group(1).strip()
        m2 = re.search(r"(<svg[\s\S]*?</svg>)", inner, re.I)
        return (m2.group(1) if m2 else inner).strip()
    return ""


_MAAS_MIN_COMPLETION_TOKENS = 65536


async def _chat_text(
    endpoint: LLMEndpoint,
    settings: BridgeSettings,
    messages: list[dict[str, str]],
    *,
    json_mode: bool,
) -> str:
    base = endpoint.base_url.rstrip("/")
    url = f"{base}/chat/completions"
    cap = max(0, int(settings.llm_max_output_tokens))
    if settings.llm_maas_compat:
        cap = max(cap, _MAAS_MIN_COMPLETION_TOKENS)
    payload: dict[str, Any] = {
        "model": endpoint.model,
        "messages": messages,
    }
    if json_mode:
        payload["response_format"] = {"type": "json_object"}
    if cap > 0:
        if settings.llm_maas_compat:
            payload["max_tokens"] = cap
            payload["max_completion_tokens"] = cap
        else:
            payload["max_completion_tokens"] = cap
    if settings.llm_maas_compat and settings.llm_disable_thinking:
        extra = {"chat_template_kwargs": {"enable_thinking": False}}
        payload["extra_body"] = extra
    headers = {"Content-Type": "application/json"}
    if endpoint.api_key:
        headers["Authorization"] = f"Bearer {endpoint.api_key}"
    timeout = httpx.Timeout(endpoint.timeout)
    async with httpx.AsyncClient(timeout=timeout) as client:
        resp = await client.post(url, json=payload, headers=headers)
    if resp.status_code >= 400:
        raise RuntimeError(f"LLM HTTP {resp.status_code}: {resp.text[:800]}")
    data = resp.json()
    try:
        return str(data["choices"][0]["message"]["content"] or "")
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError(f"LLM 响应结构异常: {repr(data)[:800]}") from exc


def _resolve_llm_endpoint(
    settings: BridgeSettings,
    request: dict[str, Any],
    *,
    role: str,
) -> LLMEndpoint:
    if role == "vlm":
        base = (request.get("ppt_vlm_base_url") or "").strip()
        model = (request.get("ppt_vlm_model") or "").strip()
        key = request.get("ppt_vlm_api_key")
        raw_to = request.get("ppt_vlm_timeout")
    else:
        base = (request.get("ppt_llm_base_url") or "").strip()
        model = (request.get("ppt_llm_model") or "").strip()
        key = request.get("ppt_llm_api_key")
        raw_to = request.get("ppt_llm_timeout")
    if key is None:
        key = ""
    else:
        key = str(key)
    if base and model:
        timeout = settings.language_model.timeout
        try:
            if raw_to is not None:
                timeout = int(raw_to)
        except (TypeError, ValueError):
            pass
        timeout = max(10, min(timeout, 3600))
        logger.info("using per-task %s override: base=%s model=%s", role, base, model)
        return LLMEndpoint(base_url=base, model=model, api_key=key, timeout=timeout)
    if role == "vlm":
        return settings.vision_model
    return settings.language_model


def _sanitize_filename(name: str) -> str:
    bad = '<>:"/\\|?*\n\r\t'
    cleaned = "".join("_" if ch in bad else ch for ch in name).strip(" .")
    return cleaned or "presentation"


def cleanup_workspace(workspace: Path) -> None:
    try:
        if workspace.exists():
            shutil.rmtree(workspace, ignore_errors=True)
    except Exception:  # pragma: no cover
        logger.exception("cleanup workspace failed: %s", workspace)
