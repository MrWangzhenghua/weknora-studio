<script setup lang="ts">
/**
 * 闪卡生成（与 PPT 对话框一致的 Studio 布局）：
 * - 左侧：任务历史；右侧：新建表单 / 任务详情（进度 + 闪卡预览）
 */
import { ref, computed, watch, onUnmounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import {
  createFlashcardGenTask,
  flashcardHealth,
  getFlashcardGenTask,
  listFlashcardGenTasks,
  purgeFlashcardGenTask,
  type FlashcardGenStatus,
  type FlashcardGenTask,
  type FlashcardItem,
} from '@/api/flashcard'
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
const tasks = ref<FlashcardGenTask[]>([])
const listLoading = ref(false)
const task = ref<FlashcardGenTask | null>(null)
const submitting = ref(false)
const polling = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

const detailTab = ref<'progress' | 'preview'>('progress')
const flipped = ref<Record<number, boolean>>({})

const form = ref({
  topic: '',
  count: 10,
  language: 'zh',
  llm_model_id: '',
})

const chatModels = ref<ModelConfig[]>([])
const llmOptions = computed(() =>
  chatModels.value
    .filter((m) => m.id)
    .map((m) => ({
      label: m.name + (m.parameters?.base_url ? ` (${m.parameters.base_url})` : ''),
      value: m.id as string,
    })),
)

const TERMINAL = new Set<FlashcardGenStatus>(['succeeded', 'failed'])

const STATUS_LABEL: Record<FlashcardGenStatus, string> = {
  pending: '排队中',
  generating: '正在生成',
  succeeded: '生成成功',
  failed: '生成失败',
}

function statusTheme(
  s: FlashcardGenStatus | undefined,
): 'default' | 'primary' | 'success' | 'danger' | 'warning' {
  if (s === 'succeeded') return 'success'
  if (s === 'failed') return 'danger'
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

const previewCards = computed((): FlashcardItem[] => task.value?.flashcards || [])

async function loadModels() {
  try {
    const list = await listModels()
    chatModels.value = (list || []).filter((m) => m.type === 'KnowledgeQA')
  } catch {
    chatModels.value = []
  }
}

watch(
  () => props.visible,
  (v) => {
    if (v) {
      void loadModels()
      void refreshList(true)
    } else {
      stopPolling()
    }
  },
)

watch(
  () => props.kbId,
  () => {
    tasks.value = []
    task.value = null
    rightPanel.value = 'idle'
    flipped.value = {}
    if (props.visible) {
      void refreshList(true)
    }
  },
)

watch(
  () => task.value?.status,
  (s) => {
    if (s === 'succeeded') {
      detailTab.value = 'preview'
    }
  },
)

onUnmounted(() => {
  stopPolling()
})

async function refreshList(autoSelect = false) {
  if (!props.kbId) return
  listLoading.value = true
  try {
    const res = await listFlashcardGenTasks(props.kbId, 30)
    tasks.value = res.data || []
    if (task.value) {
      const fresh = tasks.value.find((x) => x.task_id === task.value!.task_id)
      if (fresh) {
        task.value = fresh
        if (!TERMINAL.has(fresh.status) && rightPanel.value === 'detail') {
          startPolling(fresh.task_id)
        }
      } else {
        task.value = null
        rightPanel.value = 'idle'
      }
    } else if (autoSelect && tasks.value.length > 0) {
      const running = tasks.value.find((x) => !TERMINAL.has(x.status))
      if (running) {
        openTask(running)
      }
    }
  } catch (err: any) {
    MessagePlugin.error(`${t('flashcardGen.listFail')}: ${err?.message || err}`)
  } finally {
    listLoading.value = false
  }
}

function openTask(t: FlashcardGenTask) {
  flipped.value = {}
  detailTab.value = t.status === 'succeeded' ? 'preview' : 'progress'
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
  task.value = null
  rightPanel.value = 'form'
  form.value = {
    topic: '',
    count: 10,
    language: 'zh',
    llm_model_id: '',
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
      const res = await getFlashcardGenTask(taskId)
      task.value = res.data
      const idx = tasks.value.findIndex((x) => x.task_id === res.data.task_id)
      if (idx >= 0) {
        tasks.value[idx] = res.data
      }
      if (TERMINAL.has(res.data.status)) {
        stopPolling()
        if (res.data.status === 'succeeded') {
          detailTab.value = 'preview'
        }
      }
    } catch (err: any) {
      stopPolling()
      MessagePlugin.error(`${t('flashcardGen.pollFail')}: ${err?.message || err}`)
    }
  }, 2000)
}

function toggleFlip(i: number) {
  flipped.value = { ...flipped.value, [i]: !flipped.value[i] }
}

async function submit() {
  if (!props.kbId) return
  const tp = form.value.topic.trim()
  if (!tp) {
    MessagePlugin.warning(t('flashcardGen.topicPlaceholder'))
    return
  }
  submitting.value = true
  try {
    await flashcardHealth()
  } catch {
    MessagePlugin.error(t('flashcardGen.healthFail'))
    submitting.value = false
    return
  }
  try {
    const res = await createFlashcardGenTask(props.kbId, {
      topic: tp,
      count: form.value.count,
      language: form.value.language,
      llm_model_id: form.value.llm_model_id || undefined,
    })
    MessagePlugin.success(t('flashcardGen.taskCreated'))
    tasks.value = [res.data, ...tasks.value.filter((x) => x.task_id !== res.data.task_id)]
    openTask(res.data)
  } catch (err: any) {
    MessagePlugin.error(`${t('flashcardGen.error')}: ${err?.message || err}`)
  } finally {
    submitting.value = false
  }
}

function confirmPurge(t: FlashcardGenTask, e?: Event) {
  e?.stopPropagation?.()
  if (!TERMINAL.has(t.status)) {
    MessagePlugin.warning(t('flashcardGen.deleteNeedTerminal'))
    return
  }
  const dlg = DialogPlugin.confirm({
    header: t('flashcardGen.deleteConfirmTitle'),
    body: t('flashcardGen.deleteConfirmBody'),
    confirmBtn: t('common.confirm'),
    cancelBtn: t('common.cancel'),
    onConfirm: async () => {
      dlg.hide()
      try {
        await purgeFlashcardGenTask(t.task_id)
        MessagePlugin.success(t('flashcardGen.deleteSuccess'))
        if (task.value?.task_id === t.task_id) {
          task.value = null
          rightPanel.value = 'idle'
          stopPolling()
        }
        tasks.value = tasks.value.filter((x) => x.task_id !== t.task_id)
      } catch (err: any) {
        MessagePlugin.error(`${t('flashcardGen.deleteFail')}: ${err?.message || err}`)
      }
    },
  })
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
    :header="t('flashcardGen.title')"
    :footer="null"
    width="960"
    :close-on-overlay-click="false"
    class="fc-gen-dialog"
  >
    <div class="fc-studio">
      <aside class="fc-studio-sidebar">
        <div class="fc-studio-sidebar-head">
          <span class="fc-studio-sidebar-title">{{ t('flashcardGen.history') }}</span>
          <t-button size="small" variant="text" :loading="listLoading" @click="refreshList(false)">
            {{ t('flashcardGen.refresh') }}
          </t-button>
        </div>
        <t-button block theme="primary" variant="outline" class="fc-studio-new" @click="gotoForm">
          + {{ t('flashcardGen.newTask') }}
        </t-button>
        <t-loading :loading="listLoading" size="small">
          <div v-if="!tasks.length && !listLoading" class="fc-studio-empty">
            {{ t('flashcardGen.historyEmpty') }}
          </div>
          <div
            v-for="item in tasks"
            :key="item.task_id"
            class="fc-studio-item"
            :class="{ active: task?.task_id === item.task_id && rightPanel === 'detail' }"
            @click="openTask(item)"
          >
            <div class="fc-studio-item-title">{{ item.topic || t('flashcardGen.untitled') }}</div>
            <div class="fc-studio-item-meta">
              {{ formatTime(item.created_at) }} · {{ shortId(item.task_id) }}
            </div>
            <div class="fc-studio-item-row">
              <t-tag :theme="statusTheme(item.status)" size="small">
                {{ STATUS_LABEL[item.status] || item.status }}
              </t-tag>
              <t-button
                v-if="TERMINAL.has(item.status)"
                shape="square"
                variant="text"
                theme="danger"
                size="small"
                @click="confirmPurge(item, $event)"
              >
                <template #icon><t-icon name="delete" /></template>
              </t-button>
            </div>
          </div>
        </t-loading>
      </aside>

      <main class="fc-studio-main">
        <div v-if="rightPanel === 'idle'" class="fc-studio-placeholder">
          {{ t('flashcardGen.pickOrCreate') }}
        </div>

        <div v-else-if="rightPanel === 'form'" class="fc-gen-form">
          <p class="fc-hint">
            {{ kbName ? `「${kbName}」` : '' }}{{ t('flashcardGen.entryTooltip') }}
          </p>
          <t-form label-align="top">
            <t-form-item :label="t('flashcardGen.topic')" required>
              <t-input v-model="form.topic" :placeholder="t('flashcardGen.topicPlaceholder')" />
            </t-form-item>
            <t-row :gutter="16">
              <t-col :span="4">
                <t-form-item :label="t('flashcardGen.count')">
                  <t-input-number v-model="form.count" :min="1" :max="50" theme="column" />
                </t-form-item>
              </t-col>
              <t-col :span="4">
                <t-form-item :label="t('flashcardGen.language')">
                  <t-select
                    v-model="form.language"
                    :options="[
                      { label: '中文', value: 'zh' },
                      { label: 'English', value: 'en' },
                    ]"
                  />
                </t-form-item>
              </t-col>
              <t-col :span="12">
                <t-form-item :label="t('flashcardGen.llmModel')">
                  <t-select
                    v-model="form.llm_model_id"
                    clearable
                    filterable
                    :placeholder="t('flashcardGen.llmPlaceholder')"
                    :options="llmOptions"
                  />
                </t-form-item>
              </t-col>
            </t-row>
          </t-form>
          <div class="fc-gen-actions">
            <t-button theme="default" @click="rightPanel = 'idle'">{{ t('common.cancel') }}</t-button>
            <t-button theme="primary" :loading="submitting" @click="submit">
              {{ submitting ? t('flashcardGen.generating') : t('flashcardGen.submit') }}
            </t-button>
          </div>
        </div>

        <div v-else-if="rightPanel === 'detail' && task" class="fc-gen-detail">
          <t-tabs v-model="detailTab">
            <t-tab-panel value="progress" :label="t('flashcardGen.tabProgress')">
              <div class="fc-gen-progress">
                <div class="fc-gen-progress-header">
                  <t-tag :theme="statusTheme(task.status)" size="medium">{{ statusLabel }}</t-tag>
                  <span class="fc-gen-progress-message">{{ task.message }}</span>
                </div>
                <t-progress
                  :percentage="task.progress"
                  :status="task.status === 'failed' ? 'error' : 'active'"
                />
                <ul class="fc-gen-progress-meta">
                  <li>{{ t('flashcardGen.topic') }}：{{ task.topic }}</li>
                  <li>{{ t('flashcardGen.count') }}：{{ task.count }}</li>
                  <li v-if="task.files_included">
                    {{ t('flashcardGen.filesIncluded') }}：{{ task.files_included }}
                  </li>
                  <li v-if="task.files_skipped">
                    {{ t('flashcardGen.filesSkipped') }}：{{ task.files_skipped }}
                  </li>
                  <li>{{ t('flashcardGen.taskId') }}：{{ task.task_id }}</li>
                  <li>{{ t('flashcardGen.createdAt') }}：{{ formatTime(task.created_at) }}</li>
                  <li v-if="task.error" class="fc-gen-error">{{ task.error }}</li>
                </ul>
                <div class="fc-gen-actions">
                  <t-button v-if="task.status === 'succeeded'" theme="primary" @click="detailTab = 'preview'">
                    {{ t('flashcardGen.viewCards') }}
                  </t-button>
                  <t-button v-if="isTerminal" theme="default" @click="gotoForm">
                    {{ t('flashcardGen.restart') }}
                  </t-button>
                  <t-button v-if="isTerminal" theme="default" @click="confirmPurge(task)">
                    {{ t('flashcardGen.deleteTask') }}
                  </t-button>
                </div>
              </div>
            </t-tab-panel>
            <t-tab-panel
              v-if="task.status === 'succeeded'"
              value="preview"
              :label="t('flashcardGen.tabPreview')"
            >
              <div v-if="previewCards.length" class="fc-grid">
                <div
                  v-for="(card, i) in previewCards"
                  :key="i"
                  class="fc-card"
                  role="button"
                  tabindex="0"
                  @click="toggleFlip(i)"
                  @keydown.enter.prevent="toggleFlip(i)"
                >
                  <div class="fc-card-inner" :class="{ flipped: flipped[i] }">
                    <div class="fc-face fc-front">
                      <div class="fc-label">{{ t('flashcardGen.front') }}</div>
                      <div class="fc-text">{{ card.front }}</div>
                    </div>
                    <div class="fc-face fc-back">
                      <div class="fc-label">{{ t('flashcardGen.back') }}</div>
                      <div class="fc-text">{{ card.back }}</div>
                    </div>
                  </div>
                  <div class="fc-flip-hint">{{ t('flashcardGen.flip') }}</div>
                </div>
              </div>
              <div v-else class="fc-empty">{{ t('flashcardGen.empty') }}</div>
            </t-tab-panel>
          </t-tabs>
        </div>
      </main>
    </div>
  </t-dialog>
</template>

<style scoped lang="less">
.fc-studio {
  display: flex;
  gap: 16px;
  min-height: 420px;
  max-height: 70vh;
}

.fc-studio-sidebar {
  width: 260px;
  flex-shrink: 0;
  border-right: 1px solid var(--td-border-level-1-color);
  padding-right: 12px;
  overflow-y: auto;
}

.fc-studio-sidebar-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.fc-studio-sidebar-title {
  font-weight: 600;
  font-size: 14px;
}

.fc-studio-new {
  margin-bottom: 12px;
}

.fc-studio-empty,
.fc-studio-placeholder {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
  padding: 16px 0;
  text-align: center;
}

.fc-studio-placeholder {
  padding: 48px 16px;
  font-size: 13px;
}

.fc-studio-item {
  padding: 10px;
  border-radius: 8px;
  border: 1px solid var(--td-border-level-1-color);
  margin-bottom: 8px;
  cursor: pointer;
  transition: background 0.15s;

  &:hover {
    background: var(--td-bg-color-container-hover);
  }

  &.active {
    border-color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }
}

.fc-studio-item-title {
  font-size: 13px;
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.fc-studio-item-meta {
  font-size: 11px;
  color: var(--td-text-color-placeholder);
  margin-top: 4px;
}

.fc-studio-item-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}

.fc-studio-main {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
}

.fc-hint {
  color: var(--td-text-color-secondary);
  font-size: 13px;
  margin: 0 0 16px;
}

.fc-gen-form,
.fc-gen-detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.fc-gen-actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 12px;
}

.fc-gen-progress-header {
  display: flex;
  align-items: center;
  gap: 12px;
}

.fc-gen-progress-message {
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.fc-gen-progress-meta {
  list-style: none;
  padding: 0;
  margin: 12px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.8;

  li.fc-gen-error {
    color: var(--td-error-color);
    word-break: break-all;
  }
}

.fc-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 16px;
  margin-top: 8px;
  max-height: 52vh;
  overflow-y: auto;
}

.fc-card {
  perspective: 900px;
  cursor: pointer;
}

.fc-card-inner {
  position: relative;
  min-height: 160px;
  transform-style: preserve-3d;
  transition: transform 0.45s ease;

  &.flipped {
    transform: rotateY(180deg);
  }
}

.fc-face {
  position: absolute;
  inset: 0;
  backface-visibility: hidden;
  border-radius: 8px;
  padding: 12px;
  box-sizing: border-box;
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-container);
  overflow: auto;
}

.fc-back {
  transform: rotateY(180deg);
  background: var(--td-bg-color-secondarycontainer);
}

.fc-label {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
  margin-bottom: 8px;
}

.fc-text {
  font-size: 14px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-word;
}

.fc-flip-hint {
  text-align: center;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
  margin-top: 6px;
}

.fc-empty {
  text-align: center;
  color: var(--td-text-color-secondary);
  padding: 24px;
}
</style>
