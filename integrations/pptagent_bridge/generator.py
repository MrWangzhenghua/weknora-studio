"""调用 PPTAgent 生成 PPT 的核心逻辑。

将 PPTAgent 的 Python API 封装为面向任务的高级方法：
1. 把输入的多个文件统一转成 Markdown（``any2markdown``）；
2. 拼接成一个总的 Markdown 上下文 + 用户 instruction；
3. 通过 ``Document.from_markdown`` 让 PPTAgent 解析章节与素材；
4. 通过 ``PPTAgent.generate_pres`` 渲染幻灯片并保存为 .pptx。

为了让本桥接服务在依赖缺失的环境下也能起到"接口契约"的作用，
在 import PPTAgent 失败时模块仍然能加载，仅在真正调用 ``run_task`` 时抛出可读错误。
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import shutil
from dataclasses import dataclass
from pathlib import Path
import difflib
import typing
from enum import Enum
from typing import Any, Awaitable, Callable, List, Optional, get_args, get_origin

from .config import BridgeSettings, LLMEndpoint
from .models import TaskRecord, TaskStatus

logger = logging.getLogger(__name__)


ProgressCallback = Callable[[TaskStatus, int, str], Awaitable[None]]


# 支持直接转换的扩展名集合（其他类型会尝试用 any2markdown 通用解析）。
_TEXT_EXTENSIONS = {".md", ".markdown", ".txt", ".rst"}
_DOC_EXTENSIONS = {".pdf", ".docx", ".doc", ".pptx", ".ppt", ".html", ".htm", ".xlsx", ".xls", ".csv"}
_PYDANTIC_JSON_REPAIR_INSTALLED = False


def _is_pptagent_available() -> bool:
    try:
        import pptagent  # noqa: F401
        return True
    except Exception:  # pragma: no cover - 仅运行期判断
        return False


@dataclass
class GeneratorContext:
    """单个任务的运行时上下文。"""

    settings: BridgeSettings
    record: TaskRecord
    progress_cb: ProgressCallback


class PPTGenerator:
    """对外暴露的 PPT 生成器。"""

    def __init__(self, settings: BridgeSettings):
        self.settings = settings

    # ----------------------------- 公共入口 -----------------------------
    async def run_task(
        self,
        record: TaskRecord,
        input_files: List[Path],
        progress_cb: ProgressCallback,
    ) -> Path:
        """运行一个完整的 PPT 生成流程。

        Args:
            record: 任务记录，函数内不会修改它，但会通过 progress_cb 推送状态。
            input_files: 输入文件的绝对路径列表。
            progress_cb: 进度回调，签名 ``async (status, progress, message) -> None``。

        Returns:
            生成的 .pptx 文件绝对路径。
        """

        if not _is_pptagent_available():
            raise RuntimeError(
                "PPTAgent 依赖未安装，请确认镜像中已 `pip install -e integrations/PPTAgent`"
            )

        ctx = GeneratorContext(settings=self.settings, record=record, progress_cb=progress_cb)
        await ctx.progress_cb(TaskStatus.EXTRACTING, 5, "正在合并知识库文件")

        workspace = Path(record.workspace)
        workspace.mkdir(parents=True, exist_ok=True)
        markdown_dir = workspace / "markdown"
        markdown_dir.mkdir(exist_ok=True)
        image_dir = workspace / "images"
        image_dir.mkdir(exist_ok=True)

        combined_markdown = await self._to_combined_markdown(ctx, input_files, markdown_dir)
        await ctx.progress_cb(TaskStatus.DRAFTING, 30, "已合并文档，准备生成大纲")

        # 真正调用 PPTAgent
        output_path = await self._run_pptagent(ctx, combined_markdown, image_dir)
        await ctx.progress_cb(TaskStatus.SUCCEEDED, 100, "PPT 生成成功")
        return output_path

    # ----------------------------- Markdown 合并 -----------------------------
    async def _to_combined_markdown(
        self,
        ctx: GeneratorContext,
        files: List[Path],
        markdown_dir: Path,
    ) -> str:
        """把所有输入文件统一转换为 Markdown 文本，并拼接成一份。

        - 文本类（md/txt/rst）直接读取；
        - 文档类（pdf/docx/pptx/...）走 PPTAgent 自带的 ``any2markdown`` 工具；
        - 其它格式（如图片、二进制）只记录文件名占位，避免污染上下文。
        """

        # 延迟导入：保持模块加载阶段不依赖 PPTAgent。
        # 多个 fork 的包名都可能提供 any2markdown，按优先级试一遍。
        any2markdown = None  # type: ignore[assignment]
        for module_name in (
            "pptagent.tools.any2markdown",
            "deeppresenter.tools.any2markdown",
            "pptagent.any2markdown",
        ):
            try:
                module = __import__(module_name, fromlist=["any2markdown"])
                any2markdown = getattr(module, "any2markdown", None)
                if any2markdown is not None:
                    logger.info("any2markdown loaded from %s", module_name)
                    break
            except Exception:
                continue
        if any2markdown is None:
            logger.warning(
                "any2markdown not available in this image; only .md/.txt inputs will be parsed correctly"
            )

        chunks: List[str] = []
        total = max(len(files), 1)
        for idx, file_path in enumerate(files):
            ext = file_path.suffix.lower()
            title = file_path.stem
            placeholder = False
            try:
                if ext in _TEXT_EXTENSIONS:
                    text = file_path.read_text(encoding="utf-8", errors="ignore")
                elif ext in _DOC_EXTENSIONS and any2markdown is not None:
                    text = await asyncio.to_thread(any2markdown, str(file_path))
                    if not text or not text.strip():
                        logger.warning("any2markdown produced empty output for %s", file_path)
                        text = ""
                        placeholder = True
                else:
                    placeholder = True
                    text = ""
            except Exception as exc:
                logger.exception("convert file failed: %s", file_path)
                placeholder = True
                text = ""

            text = (text or "").strip()
            if placeholder or not text:
                # 不再把 "文件未解析" 当作正文，避免 LLM 把元信息当成 PPT 主题。
                # 这里仍然保留一个最小提示，但用 HTML 注释 + 极短文本，
                # 让 LLM 更可能跳过它而不是围绕它生成幻灯片。
                logger.error(
                    "[pptgen-bridge] cannot extract text from %s (ext=%s); "
                    "WeKnora should have provided a pre-parsed .md instead. "
                    "Falling back to filename-only placeholder.",
                    file_path.name,
                    ext,
                )
                text = (
                    f"<!-- 注意：此文件未能在 Bridge 中解析，建议确认 WeKnora 已为其生成 chunk -->\n"
                    f"(无可用正文，仅提供文件名：{file_path.name})"
                )

            chunk = f"# {title}\n\n{text}\n"
            chunks.append(chunk)
            # 落盘单文件 markdown，便于在 workspace 里排查内容是否正确。
            (markdown_dir / f"{idx:03d}_{file_path.stem}.md").write_text(chunk, encoding="utf-8")
            logger.info(
                "[pptgen-bridge] markdown ready: %s -> %d chars (placeholder=%s)",
                file_path.name,
                len(text),
                placeholder,
            )
            progress = 5 + int(20 * (idx + 1) / total)
            await ctx.progress_cb(
                TaskStatus.EXTRACTING,
                progress,
                f"已处理 {idx + 1}/{len(files)} 个文件：{file_path.name}",
            )

        combined = "\n\n---\n\n".join(chunks)
        # 与 PPTAgent 内部 MAX_CONTEXT_SIZE 同量级，避免单次请求撑爆上下文或挤占输出额度导致 JSON 截断
        max_chars = max(4096, self.settings.markdown_max_chars)
        if len(combined) > max_chars:
            logger.warning(
                "[pptgen-bridge] combined markdown len=%d > markdown_max_chars=%d, truncating",
                len(combined),
                max_chars,
            )
            combined = combined[:max_chars] + (
                "\n\n<!-- WeKnora Bridge：合并后的输入过长已截断；可调大环境变量 "
                "PPTAGENT_MARKDOWN_MAX_CHARS，或由 WeKnora 侧使用摘要回退。 -->\n"
            )

        combined_path = markdown_dir / "_combined.md"
        combined_path.write_text(combined, encoding="utf-8")
        logger.info(
            "[pptgen-bridge] combined markdown ready: %d files, %d total chars",
            len(files),
            len(combined),
        )
        return combined

    # ----------------------------- PPTAgent 调用 -----------------------------
    async def _run_pptagent(
        self,
        ctx: GeneratorContext,
        markdown_content: str,
        image_dir: Path,
    ) -> Path:
        """调用 PPTAgent 主流程生成最终 .pptx 文件。"""

        _install_pydantic_json_repair()

        from pptagent import Document, ImageLabler, PPTAgent, Presentation
        from pptagent.utils import Config, package_join

        record = ctx.record
        settings = ctx.settings

        if not settings.language_model.is_configured:
            raise RuntimeError(
                "未配置 PPTAGENT_LLM_BASE_URL / PPTAGENT_LLM_MODEL / PPTAGENT_LLM_API_KEY"
            )

        # 为 AsyncLLM 注入 max_completion_tokens /（MaaS 时）max_tokens，并可选关闭深度思考，
        # 缓解「reasoning 占满 completion 额度 → 正文 JSON 被截断 → parse 失败」。
        llm_max_out = max(0, settings.llm_max_output_tokens)
        vlm_max_out = max(0, settings.vlm_max_output_tokens)
        if vlm_max_out <= 0:
            vlm_max_out = llm_max_out

        language_model = _make_llm(
            _resolve_llm_endpoint(settings, record.request, role="llm"),
            llm_max_out,
            maas_compat=settings.llm_maas_compat,
            disable_thinking=settings.llm_disable_thinking,
        )
        vlm_ep = _resolve_llm_endpoint(settings, record.request, role="vlm")
        if vlm_ep.is_configured:
            vision_model = _make_llm(
                vlm_ep,
                vlm_max_out,
                maas_compat=settings.vlm_maas_compat,
                disable_thinking=settings.vlm_disable_thinking,
            )
        else:
            vision_model = language_model

        # 1) Document.from_markdown 会调用 LLM 切分章节、抽取元信息
        await ctx.progress_cb(TaskStatus.DRAFTING, 40, "解析合并 Markdown 为文档结构")
        document = await Document.from_markdown(
            markdown_content=markdown_content,
            language_model=language_model,
            vision_model=vision_model,
            image_dir=str(image_dir),
        )

        # 2) 加载模板（默认 ``default`` 模板，可通过请求覆盖）
        template_name = ctx.record.request.get("template") or settings.default_template
        templates_dir = Path(package_join("templates"))
        template_folder = templates_dir / template_name
        if not template_folder.exists():
            # 模板缺失时回退到任意一个可用模板，避免直接失败
            candidates = [p for p in templates_dir.iterdir() if p.is_dir()]
            if not candidates:
                raise RuntimeError("PPTAgent 内置模板未找到，请检查镜像构建")
            template_folder = candidates[0]
            logger.warning("template %s missing, fallback to %s", template_name, template_folder.name)

        prs_config = Config(str(template_folder))
        presentation = Presentation.from_file(str(template_folder / "source.pptx"), prs_config)
        image_labler = ImageLabler(presentation, prs_config)
        import json as _json
        image_labler.apply_stats(
            _json.loads((template_folder / "image_stats.json").read_text())
        )
        slide_induction = _json.loads(
            (template_folder / "slide_induction.json").read_text()
        )

        # 3) 初始化 PPTAgent
        await ctx.progress_cb(TaskStatus.DRAFTING, 55, "初始化 PPTAgent 生成器")
        pptagent = PPTAgent(language_model=language_model, vision_model=vision_model)
        pptagent.set_reference(slide_induction=slide_induction, presentation=presentation)

        # 4) 生成幻灯片
        await ctx.progress_cb(TaskStatus.RENDERING, 65, "生成 PPT 大纲与逐张幻灯片")
        num_slides = ctx.record.request.get("num_pages") or None
        try:
            num_slides = int(num_slides) if num_slides else None
        except (TypeError, ValueError):
            num_slides = None

        prs, _history = await pptagent.generate_pres(
            source_doc=document,
            num_slides=num_slides,
            image_dir=str(image_dir),
        )
        if prs is None:
            raise RuntimeError("PPTAgent 未能成功生成幻灯片，请检查 LLM 配置或文档内容")

        # 5) 保存为 pptx 文件
        await ctx.progress_cb(TaskStatus.RENDERING, 90, "保存 PPT 文件")
        output_dir = Path(record.workspace) / "output"
        output_dir.mkdir(exist_ok=True)
        title = ctx.record.request.get("title") or "WeKnora-PPT"
        safe_title = _sanitize_filename(title)
        output_path = output_dir / f"{safe_title}.pptx"
        await asyncio.to_thread(prs.save, str(output_path))
        return output_path


def _install_pydantic_json_repair() -> None:
    """Make PPTAgent tolerant of LLMs that wrap JSON in Markdown fences.

    PPTAgent uses pydantic ``model_validate_json`` in several structured-output
    paths. Some OpenAI-compatible models return valid JSON inside ```json fences
    or with short prose around it, which is semantically fine but rejected by
    Pydantic's strict JSON parser. Patching the BaseModel classmethod keeps this
    compatibility layer local to the bridge process without forking PPTAgent.
    """

    global _PYDANTIC_JSON_REPAIR_INSTALLED
    if _PYDANTIC_JSON_REPAIR_INSTALLED:
        return

    try:
        from pydantic import BaseModel
    except Exception:  # pragma: no cover - pydantic is a runtime dependency
        logger.exception("pydantic is unavailable; JSON repair patch skipped")
        return

    original_validate_json = BaseModel.model_validate_json

    def patched_model_validate_json(cls, json_data, *args, **kwargs):  # type: ignore[no-untyped-def]
        try:
            return original_validate_json.__func__(cls, json_data, *args, **kwargs)
        except Exception as original_error:
            return _repair_and_validate(
                cls,
                json_data,
                original_error,
                strict_validator=lambda c, d: original_validate_json.__func__(c, d, *args, **kwargs),
                dict_validator=lambda c, d: c.model_validate(d),
            )

    BaseModel.model_validate_json = classmethod(patched_model_validate_json)

    # Some PPTAgent code paths route through ``TypeAdapter(SomeModel).validate_json``
    # rather than the model's own classmethod. Patch that entry point too so the
    # bridge applies the same repair/normalize pipeline regardless of how the
    # response is parsed downstream.
    try:
        from pydantic import TypeAdapter

        original_adapter_validate_json = TypeAdapter.validate_json

        def patched_adapter_validate_json(self, data, *args, **kwargs):  # type: ignore[no-untyped-def]
            try:
                return original_adapter_validate_json(self, data, *args, **kwargs)
            except Exception as original_error:
                adapter_type = getattr(self, "_type", type(None))
                return _repair_and_validate(
                    adapter_type,
                    data,
                    original_error,
                    strict_validator=lambda _c, d: original_adapter_validate_json(self, d, *args, **kwargs),
                    dict_validator=lambda _c, d: self.validate_python(d),
                )

        TypeAdapter.validate_json = patched_adapter_validate_json
    except Exception:  # pragma: no cover - TypeAdapter is optional defensive cover
        logger.exception("Failed to patch TypeAdapter.validate_json; continuing")

    _PYDANTIC_JSON_REPAIR_INSTALLED = True


def _repair_and_validate(  # type: ignore[no-untyped-def]
    cls,
    json_data,
    original_error,
    *,
    strict_validator,
    dict_validator,
):
    """Try increasingly aggressive repairs and surface the most useful error.

    PPTAgent's AsyncLLM uses the validation exception text to feed back into the
    LLM on retry. A bare ``Invalid JSON`` failure starves the retry loop of any
    actionable hint, so when the repaired JSON itself produces a richer schema
    error we prefer to re-raise that instead — even after we've already tried
    fuzzy-matching Literal/Enum values.
    """

    repaired = _repair_llm_json(json_data)
    repaired_error: Optional[BaseException] = None
    normalized_error: Optional[BaseException] = None
    cls_name = getattr(cls, "__name__", str(cls))

    if repaired is not None and repaired != json_data:
        try:
            logger.warning(
                "Repaired non-strict JSON response for %s before pydantic validation",
                cls_name,
            )
            return strict_validator(cls, repaired)
        except Exception as exc:
            repaired_error = exc

    if repaired is not None:
        try:
            parsed = json.loads(repaired)
        except Exception:
            parsed = None

        if parsed is not None:
            try:
                normalized = _normalize_llm_json_for_model(cls, parsed)
                if normalized != parsed:
                    logger.warning(
                        "Normalized JSON schema mismatches for %s before pydantic validation",
                        cls_name,
                    )
                return dict_validator(cls, normalized)
            except Exception as exc:
                normalized_error = exc

    # Prefer the most actionable error message for AsyncLLM's retry feedback
    # loop. Order of usefulness:
    #   normalized_error  > repaired_error  > original_error
    # because normalization happened *after* JSON repair, so its remaining
    # error is the most specific schema complaint we have.
    chosen = _choose_most_informative(normalized_error, repaired_error, original_error)
    if chosen is original_error:
        raise original_error
    raise chosen from original_error


def _choose_most_informative(*errors: Optional[BaseException]) -> BaseException:
    """Return the first error that isn't a pure ``Invalid JSON`` complaint.

    Falls back to the last non-None error if every candidate is a JSON parse
    failure, and finally to the first error we were given.
    """
    seen: list[BaseException] = [e for e in errors if e is not None]
    if not seen:
        raise RuntimeError("no error to raise")
    for err in seen:
        text = str(err)
        if "Invalid JSON" not in text and "json_invalid" not in text:
            return err
    return seen[-1]


def _repair_llm_json(value) -> Optional[str]:  # type: ignore[no-untyped-def]
    """Return strict JSON text from common LLM formatting variants.

    Fallbacks intentionally stay conservative:
    - strip BOM/whitespace;
    - unwrap Markdown code fences such as ```json ... ```;
    - extract the first balanced JSON object/array from surrounding prose;
    - accept JSON string literals that themselves contain JSON.
    """

    if isinstance(value, bytes):
        try:
            text = value.decode("utf-8", errors="ignore")
        except Exception:
            return None
    elif isinstance(value, str):
        text = value
    else:
        return None

    candidates: list[str] = []
    stripped = text.strip().lstrip("\ufeff").strip()
    if stripped:
        candidates.append(stripped)

    unfenced = _strip_markdown_fence(stripped)
    if unfenced and unfenced not in candidates:
        candidates.append(unfenced)

    for candidate in list(candidates):
        extracted = _extract_balanced_json(candidate)
        if extracted and extracted not in candidates:
            candidates.append(extracted)

    for candidate in list(candidates):
        unquoted = _unwrap_json_string(candidate)
        if unquoted and unquoted not in candidates:
            candidates.append(unquoted)
            extracted = _extract_balanced_json(unquoted)
            if extracted and extracted not in candidates:
                candidates.append(extracted)

    for candidate in candidates:
        normalized = candidate.strip()
        if not normalized:
            continue
        try:
            json.loads(normalized)
            return normalized
        except Exception:
            continue
    return candidates[-1].strip() if candidates else None


def _normalize_llm_json_for_model(model_cls: type, value: Any) -> Any:
    """Coerce common LLM schema mistakes before Pydantic validation.

    The most common PPTAgent failures we've actually seen in production:

    1. ``metadata: {}`` for a ``list[Metadata]`` field (empty dict vs empty list).
    2. ``metadata: {"title": "...", "publish_date": "..."}`` — LLM treats the
       list-of-key-value-pairs as a flat dict, completely ignoring the inner
       Metadata(name, value) shape.
    3. ``layout: "Explanatory Points"`` instead of one of the allowed Literal
       choices (e.g. ``"Explanatory Points Layout"``).

    This helper rewrites those cases in-place and recurses into nested values
    so the next Pydantic validation pass has a chance to succeed.
    """

    if isinstance(value, list):
        return [_normalize_llm_json_for_model(model_cls, item) for item in value]
    if not isinstance(value, dict):
        return value

    normalized = {
        key: _normalize_llm_json_for_model(model_cls, item)
        for key, item in value.items()
    }
    fields = getattr(model_cls, "model_fields", {}) or {}
    for name, field in fields.items():
        if name not in normalized:
            continue
        item = normalized[name]

        if _field_expects_list(field) and isinstance(item, dict):
            converted = _convert_dict_for_list_field(field, item)
            if converted != item:
                logger.warning(
                    "Coerced dict -> list for %s.%s (size=%d)",
                    getattr(model_cls, "__name__", str(model_cls)),
                    name,
                    len(converted),
                )
                normalized[name] = converted
            continue

        # Literal/Enum fuzzy mapping: when the LLM picks a value that's close to
        # but not exactly in the allowed set (e.g. ``"Explanatory Points"`` when
        # the schema only permits a curated list of layout names), snap it to
        # the closest allowed option so the validation pass actually succeeds
        # instead of bouncing back to the LLM.
        allowed = _literal_or_enum_values(field)
        if allowed and isinstance(item, str) and item not in allowed:
            matched = _fuzzy_match_choice(item, allowed)
            if matched is not None and matched != item:
                logger.warning(
                    "Mapped LLM choice '%s' -> '%s' for %s.%s",
                    item,
                    matched,
                    getattr(model_cls, "__name__", str(model_cls)),
                    name,
                )
                normalized[name] = matched
    return normalized


def _field_expects_list(field: Any) -> bool:
    annotation = getattr(field, "annotation", None)
    return _annotation_expects_list(annotation)


def _annotation_expects_list(annotation: Any) -> bool:
    """True if the annotation is ``list`` (possibly wrapped in Optional/Union)."""
    if annotation is None:
        return False
    if annotation is list:
        return True
    if str(annotation).startswith("typing.List"):
        return True
    origin = get_origin(annotation)
    if origin is list:
        return True
    if origin is typing.Union:
        return any(_annotation_expects_list(arg) for arg in get_args(annotation))
    return False


def _list_inner_type(annotation: Any) -> Any:
    """Extract ``X`` from ``list[X]`` / ``Optional[list[X]]`` / ``Union[list[X], ...]``."""
    if annotation is None:
        return None
    origin = get_origin(annotation)
    if origin is list:
        args = get_args(annotation)
        return args[0] if args else None
    if origin is typing.Union:
        for arg in get_args(annotation):
            inner = _list_inner_type(arg)
            if inner is not None:
                return inner
    return None


# Inner-model field names that look "key-ish" or "value-ish" — used to map a
# plain {"k": "v"} dict onto a list of {name, value}-shaped Pydantic items.
_KEY_FIELD_HINTS = ("name", "key", "label", "field", "property", "title", "id")
_VALUE_FIELD_HINTS = ("value", "val", "content", "text", "data")


def _convert_dict_for_list_field(field: Any, value: dict[str, Any]) -> list[Any]:
    """Turn a dict that *should* be a list into the right list shape.

    Tries, in order:
      1. Unwrap common container keys (``items``/``values``/``data``/...).
      2. If the inner type is a 2-field BaseModel with key-ish + value-ish names
         (e.g. ``Metadata(name, value)``), explode the dict's entries into one
         item per key/value pair.
      3. If the dict's keys overlap with the inner model's fields, treat it as
         a single item and wrap it in ``[value]``.
      4. Last resort: wrap the whole dict in a single-element list — never lose
         data by returning ``[]``.
    """
    if not value:
        return []

    for key in ("items", "values", "data", "list", "metadata", "results", "entries"):
        nested = value.get(key)
        if isinstance(nested, list):
            return nested

    annotation = getattr(field, "annotation", None)
    inner = _list_inner_type(annotation)
    inner_fields: dict[str, Any] = {}
    if inner is not None:
        inner_fields = dict(getattr(inner, "model_fields", {}) or {})

    if inner_fields:
        key_field = _pick_field(inner_fields, _KEY_FIELD_HINTS)
        val_field = _pick_field(inner_fields, _VALUE_FIELD_HINTS)
        # Pattern 2: the LLM flattened a list[KV] into a {k: v} dict.
        if (
            key_field
            and val_field
            and key_field != val_field
            and len(inner_fields) <= 3
            and all(not isinstance(v, (list, dict)) for v in value.values())
        ):
            return [
                {key_field: str(k), val_field: _stringify_scalar(v)}
                for k, v in value.items()
            ]
        # Pattern 3: dict keys look like the model's own fields.
        if set(value.keys()) & set(inner_fields.keys()):
            return [value]

    return [value]


def _pick_field(fields: dict[str, Any], hints: tuple[str, ...]) -> Optional[str]:
    """Return the first field whose name matches a hint (case-insensitive)."""
    lowered = {name.lower(): name for name in fields}
    for hint in hints:
        if hint in lowered:
            return lowered[hint]
    for hint in hints:
        for low, original in lowered.items():
            if hint in low:
                return original
    return None


def _stringify_scalar(value: Any) -> str:
    if isinstance(value, str):
        return value
    if value is None:
        return ""
    return str(value)


def _literal_or_enum_values(field: Any) -> Optional[List[Any]]:
    """Return the allowed values when a field is a Literal/Enum (incl. Optional)."""

    annotation = getattr(field, "annotation", None)
    if annotation is None:
        return None
    return _literal_or_enum_values_from_annotation(annotation)


def _literal_or_enum_values_from_annotation(annotation: Any) -> Optional[List[Any]]:
    if annotation is None:
        return None
    if annotation is typing.Literal:  # pragma: no cover - never the bare form
        return None
    origin = get_origin(annotation)
    if origin is typing.Literal:
        return list(get_args(annotation))
    try:
        if isinstance(annotation, type) and issubclass(annotation, Enum):
            return [member.value for member in annotation]
    except TypeError:
        pass
    if origin is typing.Union:
        for arg in get_args(annotation):
            nested = _literal_or_enum_values_from_annotation(arg)
            if nested:
                return nested
    return None


def _fuzzy_match_choice(value: str, allowed: List[Any]) -> Optional[Any]:
    """Pick the closest allowed value via difflib + case-insensitive substring match."""

    candidates_str = [str(opt) for opt in allowed]
    matches = difflib.get_close_matches(value, candidates_str, n=1, cutoff=0.5)
    if matches:
        return allowed[candidates_str.index(matches[0])]
    value_lower = value.lower()
    for idx, opt_str in enumerate(candidates_str):
        opt_lower = opt_str.lower()
        if not opt_lower:
            continue
        if opt_lower in value_lower or value_lower in opt_lower:
            return allowed[idx]
    return None




def _strip_markdown_fence(text: str) -> str:
    """Remove a single Markdown code fence wrapper if present."""

    match = re.match(r"^\s*```(?:json|JSON|javascript|js)?\s*(.*?)\s*```\s*$", text, re.DOTALL)
    if match:
        return match.group(1).strip()
    if text.startswith("```"):
        lines = text.splitlines()
        if lines:
            lines = lines[1:]
        if lines and lines[-1].strip() == "```":
            lines = lines[:-1]
        return "\n".join(lines).strip()
    return text


def _unwrap_json_string(text: str) -> Optional[str]:
    """Handle responses like '"{\"title\": ...}"'."""

    try:
        parsed = json.loads(text)
    except Exception:
        return None
    if isinstance(parsed, str):
        return parsed.strip()
    return None


def _extract_balanced_json(text: str) -> Optional[str]:
    """Extract the first balanced JSON object or array from a string."""

    starts = [idx for idx in (text.find("{"), text.find("[")) if idx >= 0]
    if not starts:
        return None
    start = min(starts)
    opener = text[start]
    closer = "}" if opener == "{" else "]"
    stack = [closer]
    in_string = False
    escape = False

    for pos in range(start + 1, len(text)):
        ch = text[pos]
        if in_string:
            if escape:
                escape = False
            elif ch == "\\":
                escape = True
            elif ch == '"':
                in_string = False
            continue

        if ch == '"':
            in_string = True
        elif ch == "{":
            stack.append("}")
        elif ch == "[":
            stack.append("]")
        elif ch in ("}", "]"):
            if not stack or ch != stack[-1]:
                return None
            stack.pop()
            if not stack:
                return text[start : pos + 1].strip()
    return None


# 华为 MaaS 等网关文档：深度思考时 reasoning 与 content 共享输出 token 预算；结构化 parse 需要足够上限。
_MAAS_MIN_COMPLETION_TOKENS = 65536


def _make_llm(
    endpoint: LLMEndpoint,
    max_output_tokens: int = 0,
    *,
    maas_compat: bool = False,
    disable_thinking: bool = False,
):
    """构造 PPTAgent 期望的 ``AsyncLLM``。

    - 默认：``max_output_tokens > 0`` 时注入 ``max_completion_tokens``（OpenAI 系）。
    - **MaaS 兼容**（``maas_compat=True``）：同时注入 ``max_tokens``（部分网关文档以此限制
      content+reasoning 总长），并将有效上限抬到至少 ``_MAAS_MIN_COMPLETION_TOKENS``；
      可选通过 ``extra_body.chat_template_kwargs`` 关闭深度思考，把额度留给 JSON 正文。
    - 若不需要任何注入且非 MaaS，返回未包装的 ``AsyncLLM``。
    """

    from pptagent import AsyncLLM

    llm = AsyncLLM(
        model=endpoint.model,
        base_url=endpoint.base_url,
        api_key=endpoint.api_key,
        timeout=endpoint.timeout,
    )

    cap = max(0, int(max_output_tokens))
    if maas_compat:
        cap = max(cap, _MAAS_MIN_COMPLETION_TOKENS)

    need_wrap = cap > 0 or maas_compat
    if not need_wrap:
        return llm

    _orig_call = AsyncLLM.__call__

    async def _wrapped(self, *args, **kwargs):  # type: ignore[no-untyped-def]
        if cap > 0:
            if maas_compat:
                if "max_tokens" not in kwargs:
                    kwargs["max_tokens"] = cap
                if "max_completion_tokens" not in kwargs:
                    kwargs["max_completion_tokens"] = cap
            else:
                if "max_tokens" not in kwargs and "max_completion_tokens" not in kwargs:
                    kwargs["max_completion_tokens"] = cap

        if maas_compat and disable_thinking:
            extra = dict(kwargs.get("extra_body") or {})
            ctk = dict(extra.get("chat_template_kwargs") or {})
            if "enable_thinking" not in ctk and "thinking" not in ctk:
                # Qwen3 等文档使用 enable_thinking；未显式配置时再关闭，避免覆盖用户自定义。
                ctk["enable_thinking"] = False
            extra["chat_template_kwargs"] = ctk
            kwargs["extra_body"] = extra

        return await _orig_call(self, *args, **kwargs)

    llm.__call__ = _wrapped.__get__(llm, AsyncLLM)  # type: ignore[method-assign]
    return llm


def _resolve_llm_endpoint(
    settings: BridgeSettings,
    request: dict[str, Any],
    *,
    role: str,
) -> LLMEndpoint:
    """合并单次任务下发的模型覆盖与 Bridge 环境默认配置。

    role 为 ``llm`` 时读取 ``ppt_llm_*``；为 ``vlm`` 时读取 ``ppt_vlm_*``。
    仅当 base_url、model、api_key（允许为空串）三项中至少 base+model 非空且
    api_key 存在键时才视为覆盖（api_key 可为空字符串以适配无密钥内网）。
    """

    if role == "vlm":
        base = (request.get("ppt_vlm_base_url") or "").strip()
        model = (request.get("ppt_vlm_model") or "").strip()
        key = request.get("ppt_vlm_api_key")
        if key is None:
            key = ""
        else:
            key = str(key)
        raw_to = request.get("ppt_vlm_timeout")
    else:
        base = (request.get("ppt_llm_base_url") or "").strip()
        model = (request.get("ppt_llm_model") or "").strip()
        key = request.get("ppt_llm_api_key")
        if key is None:
            key = ""
        else:
            key = str(key)
        raw_to = request.get("ppt_llm_timeout")

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
    """简单的文件名清洗，避免路径穿越与非法字符。"""

    bad = '<>:"/\\|?*\n\r\t'
    cleaned = "".join("_" if ch in bad else ch for ch in name).strip(" .")
    return cleaned or "presentation"


def cleanup_workspace(workspace: Path) -> None:
    """删除任务 workspace（容错处理）。"""

    try:
        if workspace.exists():
            shutil.rmtree(workspace, ignore_errors=True)
    except Exception:  # pragma: no cover
        logger.exception("cleanup workspace failed: %s", workspace)
