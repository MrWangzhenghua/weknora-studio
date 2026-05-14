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

/** 从知识库生成闪卡（同步，可能耗时数十秒） */
export function generateFlashcards(kbId: string, body: FlashcardGenerateRequest) {
  return post(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/flashcards/generate`,
    body,
  ) as Promise<ApiEnvelope<FlashcardGenerateResult>>
}

/** 闪卡 Bridge 健康检查 */
export function flashcardHealth() {
  return get('/api/v1/flashcards/health') as Promise<ApiEnvelope<{ ok: boolean }>>
}
