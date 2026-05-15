// 闪卡生成 API：对接后端 /api/v1/knowledge-bases/:id/flashcards/generate
import { post, get } from '../../utils/request'

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

export interface ApiEnvelope<T> {
  success: boolean
  data: T
}

/** 闪卡生成走 LLM，可能远超默认 axios 30s；与后端 FLASHCARD_BRIDGE_TIMEOUT_SEC（默认 180）对齐并留余量 */
const FLASHCARD_GENERATE_TIMEOUT_MS = 300_000

/** 从知识库生成闪卡（同步，可能耗时数十秒至数分钟） */
export function generateFlashcards(kbId: string, body: FlashcardGenerateRequest) {
  return post(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/flashcards/generate`,
    body,
    { timeout: FLASHCARD_GENERATE_TIMEOUT_MS },
  ) as Promise<ApiEnvelope<FlashcardGenerateResult>>
}

/** 闪卡 Bridge 健康检查 */
export function flashcardHealth() {
  return get('/api/v1/flashcards/health') as Promise<ApiEnvelope<{ ok: boolean }>>
}
