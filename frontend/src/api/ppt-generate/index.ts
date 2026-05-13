// PPT 生成相关 API：与 WeKnora 后端 /api/v1/ppt-tasks 系列接口对接
import { get, post, del, getDown, getBlob } from '../../utils/request'

/**
 * 任务状态值（与后端常量一致）。
 */
export type PPTGenStatus =
  | 'pending'
  | 'extracting'
  | 'drafting'
  | 'rendering'
  | 'succeeded'
  | 'failed'
  | 'cancelled'

/** 创建任务请求体（所有字段均可选） */
export interface CreatePPTGenRequest {
  /** 自定义提示词，留空时由后端拼装默认提示 */
  instruction?: string
  /** 期望 PPT 张数（0 / 不填表示由模型决定） */
  num_pages?: number
  /** 目标语言：zh / en / auto */
  language?: string
  /** 模板名称（位于 PPTAgent 模板目录） */
  template?: string
  /** PPT 标题；不填时取知识库名 */
  title?: string
  /** 仅包含指定 knowledge ID；不填表示包含全部 */
  include_file_ids?: string[]
  /** 全局设置中的对话模型 ID；不填则使用 Bridge 环境变量 PPTAGENT_LLM_* */
  llm_model_id?: string
  /** 全局设置中的 VLLM 模型 ID；不填则使用 Bridge 默认 */
  vlm_model_id?: string
}

/** 任务详情 */
export interface PPTGenTask {
  task_id: string
  bridge_task_id: string
  knowledge_base_id: string
  tenant_id: number
  user_id: string
  title: string
  instruction: string
  num_pages: number
  template: string
  status: PPTGenStatus
  progress: number
  message: string
  error: string
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

/** 模型连接测试结果（与后端 types.PPTGenModelTestResponse 对齐） */
export interface PPTGenModelTestResult {
  llm_ok?: boolean
  llm_message?: string
  vlm_ok?: boolean
  vlm_message?: string
}

/** 创建 PPT 生成任务 */
export function createPPTGenTask(kbId: string, body: CreatePPTGenRequest = {}) {
  return post(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/ppt-tasks`,
    body,
  ) as Promise<ApiEnvelope<PPTGenTask>>
}

/** 列出知识库的最近 PPT 任务 */
export function listPPTGenTasks(kbId: string, limit = 20) {
  return get(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/ppt-tasks?limit=${limit}`,
  ) as Promise<ApiEnvelope<PPTGenTask[]>>
}

/** 查询单个任务状态 */
export function getPPTGenTask(taskId: string) {
  return get(`/api/v1/ppt-tasks/${encodeURIComponent(taskId)}`) as Promise<
    ApiEnvelope<PPTGenTask>
  >
}

/** 取消任务 */
export function cancelPPTGenTask(taskId: string) {
  return del(`/api/v1/ppt-tasks/${encodeURIComponent(taskId)}`) as Promise<
    ApiEnvelope<PPTGenTask>
  >
}

/** 永久删除任务及远端生成的 PPT 文件（仅终态） */
export function purgePPTGenTask(taskId: string) {
  return del(`/api/v1/ppt-tasks/${encodeURIComponent(taskId)}/permanent`) as Promise<
    unknown
  >
}

/** 测试所选全局模型与 OpenAI 兼容接口的连通性（经 Bridge） */
export function testPPTGenModels(body: {
  llm_model_id?: string
  vlm_model_id?: string
}) {
  return post('/api/v1/ppt-tasks/test-model', body) as Promise<
    ApiEnvelope<PPTGenModelTestResult>
  >
}

/** 健康检查 */
export function pptAgentHealth() {
  return get(`/api/v1/ppt-tasks/health`) as Promise<ApiEnvelope<{ status: string }>>
}

/**
 * 触发浏览器下载结果文件。
 * 使用 getDown（axios responseType=blob）能够拿到二进制并自动触发 Save As。
 */
export function downloadPPTGenResult(taskId: string) {
  return getDown(`/api/v1/ppt-tasks/${encodeURIComponent(taskId)}/download`)
}

/**
 * 拉取 PDF 预览（响应拦截器已解包为 Blob）。
 * LibreOffice 首次转换可能较慢，超时放宽到 3 分钟。
 */
export async function fetchPPTGenPreviewBlob(taskId: string): Promise<Blob> {
  const blob = await getBlob(
    `/api/v1/ppt-tasks/${encodeURIComponent(taskId)}/preview`,
    180000,
  )
  if (!(blob instanceof Blob)) {
    throw new Error('预览响应不是有效的 PDF')
  }
  return blob
}
