"""任务管理器：负责任务的入队、调度、状态广播与清理。

本实现使用进程内队列 + asyncio worker。生产环境若需多副本，请替换为 Redis Streams /
RQ / Celery 等持久化队列。
"""

from __future__ import annotations

import asyncio
import logging
import time
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Dict, List, Optional

from .config import BridgeSettings
from .generator import PPTGenerator, cleanup_workspace
from .models import TaskRecord, TaskStatus

logger = logging.getLogger(__name__)


class TaskNotFoundError(Exception):
    pass


class TaskStillRunningError(Exception):
    """仅允许在终态任务上执行物理删除，避免与 worker 并发写文件冲突。"""

    pass


class TaskManager:
    """单实例任务管理器。"""

    def __init__(self, settings: BridgeSettings, generator: PPTGenerator):
        self.settings = settings
        self.generator = generator
        self._tasks: Dict[str, TaskRecord] = {}
        self._queue: asyncio.Queue[str] = asyncio.Queue()
        self._workers: List[asyncio.Task[None]] = []
        self._lock = asyncio.Lock()
        self._stop_event = asyncio.Event()
        self._gc_task: Optional[asyncio.Task[None]] = None
        self._started = False

    # --------------------------- 生命周期 ---------------------------
    async def start(self) -> None:
        if self._started:
            return
        self.settings.ensure_workspace()
        for i in range(max(1, self.settings.max_concurrency)):
            self._workers.append(asyncio.create_task(self._worker_loop(i)))
        self._gc_task = asyncio.create_task(self._gc_loop())
        self._started = True
        logger.info("TaskManager started: workers=%d", len(self._workers))

    async def stop(self) -> None:
        self._stop_event.set()
        for w in self._workers:
            w.cancel()
        if self._gc_task is not None:
            self._gc_task.cancel()
        await asyncio.gather(*self._workers, return_exceptions=True)
        if self._gc_task is not None:
            await asyncio.gather(self._gc_task, return_exceptions=True)
        self._workers.clear()
        self._gc_task = None
        self._started = False
        logger.info("TaskManager stopped")

    # --------------------------- 任务 CRUD ---------------------------
    async def submit_task(
        self,
        input_files: List[Path],
        request: dict,
        input_bytes: int,
    ) -> TaskRecord:
        """新建任务并入队。"""

        task_id = uuid.uuid4().hex
        workspace = self.settings.workspace_dir / task_id
        workspace.mkdir(parents=True, exist_ok=True)
        record = TaskRecord(
            task_id=task_id,
            status=TaskStatus.PENDING,
            progress=0,
            message="排队中",
            workspace=str(workspace),
            request=request,
            input_files=len(input_files),
            input_bytes=input_bytes,
        )
        # 把已收到的文件原样放入 workspace/inputs 中（generator 会读这里的文件）
        # 注意：generator 接收的是绝对路径列表，这里只保存原始路径用于备份记录。
        record.request["_input_paths"] = [str(p) for p in input_files]
        async with self._lock:
            self._tasks[task_id] = record
        await self._queue.put(task_id)
        logger.info("submit task: %s files=%d bytes=%d", task_id, len(input_files), input_bytes)
        return record

    async def get_task(self, task_id: str) -> TaskRecord:
        async with self._lock:
            record = self._tasks.get(task_id)
            if record is None:
                raise TaskNotFoundError(task_id)
            return record

    async def cancel_task(self, task_id: str) -> TaskRecord:
        async with self._lock:
            record = self._tasks.get(task_id)
            if record is None:
                raise TaskNotFoundError(task_id)
            if record.status.is_terminal():
                return record
            record.status = TaskStatus.CANCELLED
            record.message = "已取消"
            record.touch()
            return record

    async def purge_task(self, task_id: str) -> None:
        """从内存移除任务并删除 workspace（含生成的 pptx）。仅允许终态。"""

        async with self._lock:
            record = self._tasks.get(task_id)
            if record is None:
                raise TaskNotFoundError(task_id)
            if not record.status.is_terminal():
                raise TaskStillRunningError(task_id)
            self._tasks.pop(task_id, None)
            workspace = Path(record.workspace)
        cleanup_workspace(workspace)
        logger.info("purged task workspace: %s", task_id)

    async def list_tasks(self) -> List[TaskRecord]:
        async with self._lock:
            return list(self._tasks.values())

    def active_tasks(self) -> int:
        return sum(
            1
            for r in self._tasks.values()
            if not r.status.is_terminal() and r.status != TaskStatus.PENDING
        )

    # --------------------------- worker / 调度 ---------------------------
    async def _worker_loop(self, worker_id: int) -> None:
        logger.info("worker %d started", worker_id)
        while not self._stop_event.is_set():
            try:
                task_id = await asyncio.wait_for(self._queue.get(), timeout=1.0)
            except asyncio.TimeoutError:
                continue
            except asyncio.CancelledError:
                break

            try:
                await self._run_task(task_id, worker_id)
            except Exception:  # pragma: no cover - 兜底，正常路径已处理
                logger.exception("worker %d crashed while running %s", worker_id, task_id)

    async def _run_task(self, task_id: str, worker_id: int) -> None:
        async with self._lock:
            record = self._tasks.get(task_id)
        if record is None:
            return
        if record.status == TaskStatus.CANCELLED:
            return

        async def progress_cb(status: TaskStatus, progress: int, message: str) -> None:
            async with self._lock:
                if record.status == TaskStatus.CANCELLED:
                    return
                record.status = status
                record.progress = progress
                record.message = message
                record.touch()

        await progress_cb(TaskStatus.EXTRACTING, 1, f"worker-{worker_id} 开始处理")
        input_files = [Path(p) for p in record.request.get("_input_paths", [])]
        try:
            output_path = await asyncio.wait_for(
                self.generator.run_task(record, input_files, progress_cb),
                timeout=self.settings.task_timeout,
            )
        except asyncio.TimeoutError:
            async with self._lock:
                record.status = TaskStatus.FAILED
                record.error = f"任务超时（>{self.settings.task_timeout}s）"
                record.message = record.error
                record.touch()
            return
        except Exception as exc:
            logger.exception("task %s failed", task_id)
            async with self._lock:
                record.status = TaskStatus.FAILED
                record.error = str(exc)
                record.message = "任务失败"
                record.touch()
            return

        async with self._lock:
            record.status = TaskStatus.SUCCEEDED
            record.progress = 100
            record.result_path = str(output_path)
            record.message = "生成成功"
            record.error = ""
            record.touch()

    # --------------------------- GC ---------------------------
    async def _gc_loop(self) -> None:
        ttl = timedelta(hours=self.settings.task_ttl_hours)
        while not self._stop_event.is_set():
            try:
                await asyncio.sleep(300)
            except asyncio.CancelledError:
                break
            now = datetime.now(timezone.utc)
            to_remove: List[str] = []
            async with self._lock:
                for tid, record in self._tasks.items():
                    if not record.status.is_terminal():
                        continue
                    if now - record.updated_at > ttl:
                        to_remove.append(tid)
            for tid in to_remove:
                async with self._lock:
                    record = self._tasks.pop(tid, None)
                if record is not None:
                    cleanup_workspace(Path(record.workspace))
                    logger.info("gc task: %s", tid)
