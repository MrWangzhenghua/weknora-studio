<script setup lang="ts">
/**
 * PPT 生成（NotebookLM Studio 风格）：
 * - 左侧：任务列表 + 新建；支持删除终态任务、点击进入详情
 * - 右侧：新建表单（可选全局对话/VLM 模型 + 连接测试）或任务详情（进度 / PDF 预览 / 下载）
 */
import { ref, computed, watch, onUnmounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import {
  cancelPPTGenTask,
  createPPTGenTask,
  downloadPPTGenResult,
  fetchPPTGenPreviewBlob,
  getPPTGenTask,
  listPPTGenTasks,
  purgePPTGenTask,
  testPPTGenModels,
  type PPTGenStatus,
  type PPTGenTask,
} from '@/api/ppt-generate'
import { listModels, type ModelConfig } from '@/api/model'

interface Props {
  visible: boolean
  kbId: string
  kbName?: string
}

const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
}>()

const { t } = useI18n()

type RightPanel = 'idle' | 'form' | 'detail'
const rightPanel = ref<RightPanel>('idle')
const tasks = ref<PPTGenTask[]>([])
const listLoading = ref(false)
const task = ref<PPTGenTask | null>(null)
const submitting = ref(false)
const testingModels = ref(false)
const polling = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

/** 详情子页：进度 或 PDF 预览 */
const detailTab = ref<'progress' | 'preview'>('progress')
const previewUrl = ref<string | null>(null)
const previewLoading = ref(false)

const form = ref({
  instruction: '',
  num_pages: 0,
  template: '',
  language: 'auto',
  llm_model_id: '',
  vlm_model_id: '',
})

const chatModels = ref<ModelConfig[]>([])
const vlmModels = ref<ModelConfig[]>([])

const TERMINAL = new Set<PPTGenStatus>(['succeeded', 'failed', 'cancelled'])

const STATUS_LABEL: Record<PPTGenStatus, string> = {
  pending: '排队中',
  extracting: '解析知识库文件',
  drafting: '生成大纲',
  rendering: '渲染幻灯片',
  succeeded: '生成成功',
  failed: '生成失败',
  cancelled: '已取消',
}

function statusTheme(
  s: PPTGenStatus | undefined,
): 'default' | 'primary' | 'success' | 'danger' | 'warning' {
  if (s === 'succeeded') return 'success'
  if (s === 'failed') return 'danger'
  if (s === 'cancelled') return 'warning'
  return 'primary'
}

const isTerminal = computed(() => {
  const s = task.value?.status
  return s ? TERMINAL.has(s) : false
})

const statusLabel = computed(() => {
  if (!task.value) return ''
  return STATUS_LABEL[task.value.status] || task.value.status
})

const llmSelectOptions = computed(() =>
  chatModels.value
    .filter((m) => m.id)
    .map((m) => ({
      label: m.name + (m.parameters?.base_url ? ` (${m.parameters.base_url})` : ''),
      value: m.id as string,
    })),
)

const vlmSelectOptions = computed(() =>
  vlmModels.value
    .filter((m) => m.id)
    .map((m) => ({
      label: m.name + (m.parameters?.base_url ? ` (${m.parameters.base_url})` : ''),
      value: m.id as string,
    })),
)

function revokePreview() {
  if (previewUrl.value) {
    URL.revokeObjectURL(previewUrl.value)
    previewUrl.value = null
  }
}

async function loadModelLists() {
  try {
    const all = await listModels()
    chatModels.value = all.filter((m) => m.type === 'KnowledgeQA')
    vlmModels.value = all.filter((m) => m.type === 'VLLM')
  } catch {
    chatModels.value = []
    vlmModels.value = []
  }
}

watch(
  () => props.visible,
  (val) => {
    if (val) {
      void loadModelLists()
      void refreshList(true)
    } else {
      stopPolling()
      revokePreview()
    }
  },
)

watch(
  () => props.kbId,
  () => {
    tasks.value = []
    task.value = null
    rightPanel.value = 'idle'
    revokePreview()
    if (props.visible) {
      void refreshList(true)
    }
  },
)

watch(detailTab, (tab) => {
  if (tab === 'preview' && task.value?.status === 'succeeded') {
    void loadPreviewPdf()
  }
})

watch(
  () => task.value?.status,
  (s) => {
    if (s === 'succeeded' && detailTab.value === 'preview') {
      void loadPreviewPdf()
    }
  },
)

onUnmounted(() => {
  stopPolling()
  revokePreview()
})

async function refreshList(autoSelect = false) {
  if (!props.kbId) return
  listLoading.value = true
  try {
    const res = await listPPTGenTasks(props.kbId, 30)
    tasks.value = res.data || []
    if (task.value) {
      const fresh = tasks.value.find((t) => t.task_id === task.value!.task_id)
      if (fresh) {
        task.value = fresh
        if (!TERMINAL.has(fresh.status) && rightPanel.value === 'detail') {
          startPolling(fresh.task_id)
        }
      } else {
        // 列表中已无当前任务（可能被删除）
        task.value = null
        rightPanel.value = 'idle'
      }
    } else if (autoSelect && tasks.value.length > 0) {
      const running = tasks.value.find((t) => !TERMINAL.has(t.status))
      if (running) {
        openTask(running)
      }
    }
  } catch (err: any) {
    MessagePlugin.error(`加载任务列表失败: ${err?.message || err}`)
  } finally {
    listLoading.value = false
  }
}

function openTask(t: PPTGenTask) {
  revokePreview()
  detailTab.value = 'progress'
  task.value = t
  rightPanel.value = 'detail'
  if (!TERMINAL.has(t.status)) {
    startPolling(t.task_id)
  } else {
    stopPolling()
  }
}

function gotoForm() {
  stopPolling()
  revokePreview()
  task.value = null
  rightPanel.value = 'form'
  form.value = {
    instruction: '',
    num_pages: 0,
    template: '',
    language: 'auto',
    llm_model_id: '',
    vlm_model_id: '',
  }
}

function close() {
  emit('update:visible', false)
}

function stopPolling() {
  polling.value = false
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPolling(taskId: string) {
  stopPolling()
  polling.value = true
  pollTimer = setInterval(async () => {
    try {
      const res = await getPPTGenTask(taskId)
      task.value = res.data
      const idx = tasks.value.findIndex((t) => t.task_id === res.data.task_id)
      if (idx >= 0) {
        tasks.value[idx] = res.data
      }
      if (TERMINAL.has(res.data.status)) {
        stopPolling()
      }
    } catch (err: any) {
      stopPolling()
      MessagePlugin.error(`查询任务状态失败: ${err?.message || err}`)
    }
  }, 3000)
}

async function testModels() {
  if (!form.value.llm_model_id && !form.value.vlm_model_id) {
    MessagePlugin.warning(t('pptGen.testNeedModel'))
    return
  }
  testingModels.value = true
  try {
    const res = await testPPTGenModels({
      llm_model_id: form.value.llm_model_id || undefined,
      vlm_model_id: form.value.vlm_model_id || undefined,
    })
    const d = res.data
    const parts: string[] = []
    if (d.llm_ok !== undefined) {
      parts.push(
        d.llm_ok
          ? `${t('pptGen.llmLabel')}: ${t('pptGen.testOk')}`
          : `${t('pptGen.llmLabel')}: ${t('pptGen.testFail')} ${d.llm_message || ''}`,
      )
    }
    if (d.vlm_ok !== undefined) {
      parts.push(
        d.vlm_ok
          ? `${t('pptGen.vlmLabel')}: ${t('pptGen.testOk')}`
          : `${t('pptGen.vlmLabel')}: ${t('pptGen.testFail')} ${d.vlm_message || ''}`,
      )
    }
    MessagePlugin.info(parts.join('；') || t('pptGen.testDone'))
  } catch (err: any) {
    MessagePlugin.error(`${t('pptGen.testFail')}: ${err?.message || err}`)
  } finally {
    testingModels.value = false
  }
}

async function submit() {
  if (!props.kbId) return
  submitting.value = true
  try {
    const payload = {
      instruction: form.value.instruction?.trim() || undefined,
      num_pages: form.value.num_pages > 0 ? form.value.num_pages : undefined,
      template: form.value.template?.trim() || undefined,
      language: form.value.language === 'auto' ? undefined : form.value.language,
      title: props.kbName,
      llm_model_id: form.value.llm_model_id || undefined,
      vlm_model_id: form.value.vlm_model_id || undefined,
    }
    const res = await createPPTGenTask(props.kbId, payload)
    MessagePlugin.success('PPT 生成任务已创建，请稍候')
    tasks.value = [res.data, ...tasks.value.filter((t) => t.task_id !== res.data.task_id)]
    openTask(res.data)
  } catch (err: any) {
    MessagePlugin.error(`创建任务失败: ${err?.message || err}`)
  } finally {
    submitting.value = false
  }
}

async function cancel() {
  if (!task.value) return
  try {
    const res = await cancelPPTGenTask(task.value.task_id)
    task.value = res.data
    const idx = tasks.value.findIndex((t) => t.task_id === res.data.task_id)
    if (idx >= 0) tasks.value[idx] = res.data
    stopPolling()
    MessagePlugin.info('任务已取消')
  } catch (err: any) {
    MessagePlugin.error(`取消任务失败: ${err?.message || err}`)
  }
}

async function confirmPurge(taskItem: PPTGenTask, e?: Event) {
  e?.stopPropagation?.()
  if (!TERMINAL.has(taskItem.status)) {
    MessagePlugin.warning(t('pptGen.deleteNeedTerminal'))
    return
  }
  const dlg = DialogPlugin.confirm({
    header: t('pptGen.deleteConfirmTitle'),
    body: t('pptGen.deleteConfirmBody'),
    confirmBtn: t('common.confirm'),
    cancelBtn: t('common.cancel'),
    onConfirm: async () => {
      dlg.hide()
      try {
        await purgePPTGenTask(taskItem.task_id)
        MessagePlugin.success(t('pptGen.deleteSuccess'))
        if (task.value?.task_id === taskItem.task_id) {
          task.value = null
          rightPanel.value = 'idle'
          revokePreview()
        }
        tasks.value = tasks.value.filter((x) => x.task_id !== taskItem.task_id)
        await refreshList(false)
      } catch (err: any) {
        MessagePlugin.error(`${t('pptGen.deleteFail')}: ${err?.message || err}`)
      }
    },
  })
}

async function loadPreviewPdf() {
  if (!task.value || task.value.status !== 'succeeded') return
  previewLoading.value = true
  revokePreview()
  try {
    const blob = await fetchPPTGenPreviewBlob(task.value.task_id)
    previewUrl.value = URL.createObjectURL(blob)
  } catch (err: any) {
    MessagePlugin.error(`预览加载失败: ${err?.message || err}`)
  } finally {
    previewLoading.value = false
  }
}

async function download() {
  if (!task.value) return
  try {
    const res = await downloadPPTGenResult(task.value.task_id)
    const blob: Blob = res instanceof Blob ? res : (res as any).data
    if (!(blob instanceof Blob)) {
      throw new Error('响应不是有效的 PPT 文件')
    }
    const headers = (res as any).headers || {}
    const cd: string = headers['content-disposition'] || ''
    let filename = `${task.value.title || 'WeKnora-PPT'}.pptx`
    const match = /filename\*=UTF-8''([^;]+)|filename="?([^";]+)"?/i.exec(cd)
    if (match) {
      filename = decodeURIComponent(match[1] || match[2] || filename)
    }
    const url = window.URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = filename
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
    window.URL.revokeObjectURL(url)
  } catch (err: any) {
    MessagePlugin.error(`下载失败: ${err?.message || err}`)
  }
}

function formatTime(iso?: string) {
  if (!iso) return ''
  try {
    const d = new Date(iso)
    if (Number.isNaN(d.getTime())) return iso
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
  } catch {
    return iso
  }
}

function shortId(id: string) {
  return id?.slice(0, 8) || ''
}
</script>

<template>
  <t-dialog
    :visible="props.visible"
    :on-close="close"
    :header="$t('pptGen.title')"
    :footer="null"
    width="960"
    :close-on-overlay-click="false"
    class="ppt-gen-dialog"
  >
    <div class="ppt-studio">
      <!-- 左侧：任务列表（NotebookLM 式时间线列表） -->
      <aside class="ppt-studio-sidebar">
        <div class="ppt-studio-sidebar-head">
          <span class="ppt-studio-sidebar-title">{{ $t('pptGen.history') }}</span>
          <t-button size="small" variant="text" :loading="listLoading" @click="refreshList(false)">
            {{ $t('pptGen.refresh') }}
          </t-button>
        </div>
        <t-button block theme="primary" variant="outline" class="ppt-studio-new" @click="gotoForm">
          + {{ $t('pptGen.newTask') }}
        </t-button>
        <t-loading :loading="listLoading" size="small">
          <div v-if="!tasks.length && !listLoading" class="ppt-studio-empty">
            {{ $t('pptGen.historyEmpty') }}
          </div>
          <div
            v-for="t in tasks"
            :key="t.task_id"
            class="ppt-studio-item"
            :class="{ active: task?.task_id === t.task_id && rightPanel === 'detail' }"
            @click="openTask(t)"
          >
            <div class="ppt-studio-item-title">{{ t.title || $t('pptGen.untitled') }}</div>
            <div class="ppt-studio-item-meta">
              {{ formatTime(t.created_at) }} · {{ shortId(t.task_id) }}
            </div>
            <div class="ppt-studio-item-row">
              <t-tag :theme="statusTheme(t.status)" size="small">
                {{ STATUS_LABEL[t.status] || t.status }}
              </t-tag>
              <t-button
                v-if="TERMINAL.has(t.status)"
                shape="square"
                variant="text"
                theme="danger"
                size="small"
                @click="confirmPurge(t, $event)"
              >
                <template #icon><t-icon name="delete" /></template>
              </t-button>
            </div>
          </div>
        </t-loading>
      </aside>

      <!-- 右侧：表单 / 详情 / 空状态 -->
      <main class="ppt-studio-main">
        <div v-if="rightPanel === 'idle'" class="ppt-studio-placeholder">
          {{ $t('pptGen.pickOrCreate') }}
        </div>

        <div v-else-if="rightPanel === 'form'" class="ppt-gen-form">
          <p class="ppt-gen-hint">{{ $t('pptGen.hint') }}</p>

          <!-- 模型：与全局设置同一数据源，由后端解析凭据后下发 Bridge -->
          <t-form label-align="top">
            <t-form-item :label="$t('pptGen.llmModel')">
              <t-select
                v-model="form.llm_model_id"
                clearable
                filterable
                :placeholder="$t('pptGen.llmPlaceholder')"
                :options="llmSelectOptions"
              />
            </t-form-item>
            <t-form-item :label="$t('pptGen.vlmModel')">
              <t-select
                v-model="form.vlm_model_id"
                clearable
                filterable
                :placeholder="$t('pptGen.vlmPlaceholder')"
                :options="vlmSelectOptions"
              />
            </t-form-item>
            <div class="ppt-gen-model-actions">
              <t-button variant="outline" size="small" :loading="testingModels" @click="testModels">
                {{ $t('pptGen.testModels') }}
              </t-button>
              <span class="ppt-gen-form-help">{{ $t('pptGen.modelHelp') }}</span>
            </div>

            <t-form-item :label="$t('pptGen.instruction')">
              <t-textarea
                v-model="form.instruction"
                :placeholder="$t('pptGen.instructionPlaceholder')"
                :autosize="{ minRows: 3, maxRows: 6 }"
              />
            </t-form-item>
            <t-form-item :label="$t('pptGen.numPages')">
              <t-input-number v-model="form.num_pages" :min="0" :max="60" theme="normal" />
              <span class="ppt-gen-form-help">{{ $t('pptGen.numPagesHelp') }}</span>
            </t-form-item>
            <t-form-item :label="$t('pptGen.language')">
              <t-radio-group v-model="form.language">
                <t-radio value="auto">{{ $t('pptGen.languageAuto') }}</t-radio>
                <t-radio value="zh">中文</t-radio>
                <t-radio value="en">English</t-radio>
              </t-radio-group>
            </t-form-item>
            <t-form-item :label="$t('pptGen.template')">
              <t-input v-model="form.template" :placeholder="$t('pptGen.templatePlaceholder')" />
            </t-form-item>
          </t-form>

          <div class="ppt-gen-actions">
            <t-button theme="default" @click="rightPanel = 'idle'">{{ $t('common.cancel') }}</t-button>
            <t-button theme="primary" :loading="submitting" @click="submit">
              {{ $t('pptGen.submit') }}
            </t-button>
          </div>
        </div>

        <div v-else-if="rightPanel === 'detail' && task" class="ppt-gen-detail">
          <t-tabs v-model="detailTab">
            <t-tab-panel value="progress" :label="$t('pptGen.tabProgress')">
              <div class="ppt-gen-progress">
                <div class="ppt-gen-progress-header">
                  <t-tag :theme="statusTheme(task.status)" size="medium">{{ statusLabel }}</t-tag>
                  <span class="ppt-gen-progress-message">{{ task.message }}</span>
                </div>
                <t-progress
                  :percentage="task.progress"
                  :status="task.status === 'failed' ? 'error' : 'active'"
                />
                <ul class="ppt-gen-progress-meta">
                  <li>{{ $t('pptGen.filesIncluded') }}：{{ task.files_included }}</li>
                  <li v-if="task.files_skipped">{{ $t('pptGen.filesSkipped') }}：{{ task.files_skipped }}</li>
                  <li>{{ $t('pptGen.taskId') }}：{{ task.task_id }}</li>
                  <li>{{ $t('pptGen.createdAt') }}：{{ formatTime(task.created_at) }}</li>
                  <li v-if="task.error" class="ppt-gen-error">{{ task.error }}</li>
                </ul>
                <div class="ppt-gen-actions">
                  <t-button v-if="!isTerminal" theme="default" @click="cancel">
                    {{ $t('pptGen.cancelTask') }}
                  </t-button>
                  <t-button v-if="task.status === 'succeeded'" theme="primary" @click="download">
                    {{ $t('pptGen.download') }}
                  </t-button>
                  <t-button v-if="isTerminal" theme="default" @click="gotoForm">
                    {{ $t('pptGen.restart') }}
                  </t-button>
                  <t-button theme="default" @click="confirmPurge(task)">
                    {{ $t('pptGen.deleteTask') }}
                  </t-button>
                </div>
              </div>
            </t-tab-panel>
            <t-tab-panel
              v-if="task.status === 'succeeded'"
              value="preview"
              :label="$t('pptGen.tabPreview')"
            >
              <t-loading :loading="previewLoading" size="medium">
                <iframe
                  v-if="previewUrl"
                  class="ppt-preview-iframe"
                  title="ppt-preview"
                  :src="previewUrl"
                />
                <p v-else class="ppt-preview-hint">{{ $t('pptGen.previewHint') }}</p>
              </t-loading>
            </t-tab-panel>
          </t-tabs>
        </div>
      </main>
    </div>
  </t-dialog>
</template>

<style scoped>
.ppt-studio {
  display: flex;
  gap: 16px;
  min-height: 420px;
  max-height: 70vh;
}

.ppt-studio-sidebar {
  width: 260px;
  flex-shrink: 0;
  border-right: 1px solid var(--td-border-level-1-color);
  padding-right: 12px;
  overflow-y: auto;
}

.ppt-studio-sidebar-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.ppt-studio-sidebar-title {
  font-weight: 600;
  font-size: 14px;
}

.ppt-studio-new {
  margin-bottom: 12px;
}

.ppt-studio-empty {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
  padding: 16px 0;
  text-align: center;
}

.ppt-studio-item {
  padding: 10px 10px;
  border-radius: 8px;
  border: 1px solid var(--td-border-level-1-color);
  margin-bottom: 8px;
  cursor: pointer;
  transition: background 0.15s;
}

.ppt-studio-item:hover {
  background: var(--td-bg-color-container-hover);
}

.ppt-studio-item.active {
  border-color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.ppt-studio-item-title {
  font-size: 13px;
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.ppt-studio-item-meta {
  font-size: 11px;
  color: var(--td-text-color-placeholder);
  margin-top: 4px;
}

.ppt-studio-item-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}

.ppt-studio-main {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
}

.ppt-studio-placeholder {
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  padding: 48px 16px;
  text-align: center;
}

.ppt-gen-form,
.ppt-gen-detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.ppt-gen-hint {
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.6;
}

.ppt-gen-model-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 8px;
}

.ppt-gen-form-help {
  margin-left: 8px;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.ppt-gen-actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 12px;
}

.ppt-gen-progress-header {
  display: flex;
  align-items: center;
  gap: 12px;
}

.ppt-gen-progress-message {
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.ppt-gen-progress-meta {
  list-style: none;
  padding: 0;
  margin: 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.8;
}

.ppt-gen-progress-meta li.ppt-gen-error {
  color: var(--td-error-color);
  word-break: break-all;
}

.ppt-preview-iframe {
  width: 100%;
  min-height: 480px;
  border: 1px solid var(--td-border-level-1-color);
  border-radius: 8px;
  background: var(--td-bg-color-page);
}

.ppt-preview-hint {
  font-size: 13px;
  color: var(--td-text-color-placeholder);
  padding: 24px;
  text-align: center;
}
</style>
