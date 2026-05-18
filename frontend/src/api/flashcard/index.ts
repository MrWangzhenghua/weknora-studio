// 闪卡生成 API：任务历史与预览（对齐 PPT 生成）
import { get, post, del } from '../../utils/request'

export interface FlashcardItem {
  front: string
  back: string
}

export interface FlashcardGenerateRequest {
  topic: string
  count?: number
  language?: string
  include_file_ids?: string[]
  llm_model_id?: string
}

export interface FlashcardGenerateResult {
  topic: string
  flashcards: FlashcardItem[]
  citations?: Record<string, unknown>
  message?: string
}

export type FlashcardGenStatus = 'pending' | 'generating' | 'succeeded' | 'failed'

export interface FlashcardGenTask {
  task_id: string
  knowledge_base_id: string
  tenant_id: number
  user_id: string
  topic: string
  count: number
  language: string
  llm_model_id?: string
  status: FlashcardGenStatus
  progress: number
  message: string
  error: string
  flashcards?: FlashcardItem[]
  citations?: Record<string, unknown>
  files_included: number
  files_skipped: number
  created_at: string
  updated_at: string
  completed_at?: string
}

export interface ApiEnvelope<T> {
  success: boolean
  data: T
}

const FLASHCARD_GENERATE_TIMEOUT_MS = 300_000

/** 创建闪卡生成任务（异步，202） */
export function createFlashcardGenTask(kbId: string, body: FlashcardGenerateRequest) {
  return post(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/flashcard-tasks`,
    body,
    { timeout: 60_000 },
  ) as Promise<ApiEnvelope<FlashcardGenTask>>
}

/** 列出知识库最近的闪卡任务 */
export function listFlashcardGenTasks(kbId: string, limit = 30) {
  return get(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/flashcard-tasks?limit=${limit}`,
  ) as Promise<ApiEnvelope<FlashcardGenTask[]>>
}

/** 查询单个任务（含闪卡 JSON） */
export function getFlashcardGenTask(taskId: string) {
  return get(`/api/v1/flashcard-tasks/${encodeURIComponent(taskId)}`) as Promise<
    ApiEnvelope<FlashcardGenTask>
  >
}

/** 永久删除任务记录（仅终态） */
export function purgeFlashcardGenTask(taskId: string) {
  return del(`/api/v1/flashcard-tasks/${encodeURIComponent(taskId)}/permanent`) as Promise<unknown>
}

/** 闪卡 Bridge 健康检查 */
export function flashcardHealth() {
  return get('/api/v1/flashcard-tasks/health') as Promise<ApiEnvelope<{ ok: boolean }>>
}

/** 同步生成（兼容旧接口，一般不推荐） */
export function generateFlashcards(kbId: string, body: FlashcardGenerateRequest) {
  return post(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/flashcards/generate`,
    body,
    { timeout: FLASHCARD_GENERATE_TIMEOUT_MS },
  ) as Promise<ApiEnvelope<FlashcardGenerateResult>>
}
