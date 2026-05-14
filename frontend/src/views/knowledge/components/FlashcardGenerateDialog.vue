<script setup lang="ts">
/**
 * 知识库闪卡生成对话框：
 * - 参考 genai-rag 的「主题 + 文档上下文 → 闪卡」交互，经 WeKnora 后端聚合 Markdown 后调用 Flashcard Bridge。
 * - 同步请求，生成完成后在下方网格展示正反面，点击卡片翻面。
 */
import { ref, computed, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import {
  generateFlashcards,
  flashcardHealth,
  type FlashcardItem,
  type FlashcardGenerateResult,
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

const topic = ref('')
const count = ref(10)
const language = ref('zh')
const llmModelId = ref('')
const submitting = ref(false)
const result = ref<FlashcardGenerateResult | null>(null)
/** 每张卡是否显示背面（翻面状态） */
const flipped = ref<Record<number, boolean>>({})

const chatModels = ref<ModelConfig[]>([])
const llmOptions = computed(() =>
  chatModels.value
    .filter((m) => m.id)
    .map((m) => ({
      label: m.name + (m.parameters?.base_url ? ` (${m.parameters.base_url})` : ''),
      value: m.id as string,
    })),
)

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
      loadModels()
      result.value = null
      flipped.value = {}
    }
  },
)

function close() {
  emit('update:visible', false)
}

function toggleFlip(i: number) {
  flipped.value = { ...flipped.value, [i]: !flipped.value[i] }
}

async function onSubmit() {
  if (!props.kbId) return
  const tp = topic.value.trim()
  if (!tp) {
    MessagePlugin.warning(t('flashcardGen.topicPlaceholder'))
    return
  }
  submitting.value = true
  result.value = null
  try {
    await flashcardHealth()
  } catch {
    MessagePlugin.error(t('flashcardGen.healthFail'))
    submitting.value = false
    return
  }
  try {
    const res: any = await generateFlashcards(props.kbId, {
      topic: tp,
      count: count.value,
      language: language.value,
      llm_model_id: llmModelId.value || undefined,
    })
    const data = res?.data ?? res
    result.value = data as FlashcardGenerateResult
    if (!data?.flashcards?.length) {
      MessagePlugin.info(t('flashcardGen.empty'))
    } else {
      MessagePlugin.success(data.message || t('flashcardGen.submit'))
    }
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('flashcardGen.error'))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <t-dialog
    :visible="visible"
    @update:visible="emit('update:visible', $event)"
    :header="t('flashcardGen.title')"
    width="920px"
    :confirm-btn="null"
    :cancel-btn="t('flashcardGen.close')"
    placement="center"
    destroy-on-close
  >
    <div class="fc-root">
      <p class="fc-hint">{{ kbName ? `「${kbName}」` : '' }}{{ t('flashcardGen.entryTooltip') }}</p>
      <div class="fc-form">
        <t-form layout="vertical">
          <t-form-item :label="t('flashcardGen.topic')" required>
            <t-input v-model="topic" :placeholder="t('flashcardGen.topicPlaceholder')" />
          </t-form-item>
          <t-row :gutter="16">
            <t-col :span="4">
              <t-form-item :label="t('flashcardGen.count')">
                <t-input-number v-model="count" :min="1" :max="50" theme="column" />
              </t-form-item>
            </t-col>
            <t-col :span="4">
              <t-form-item :label="t('flashcardGen.language')">
                <t-select v-model="language" :options="[
                  { label: '中文', value: 'zh' },
                  { label: 'English', value: 'en' },
                ]" />
              </t-form-item>
            </t-col>
            <t-col :span="12">
              <t-form-item :label="t('flashcardGen.llmModel')">
                <t-select
                  v-model="llmModelId"
                  clearable
                  :placeholder="t('flashcardGen.llmPlaceholder')"
                  :options="llmOptions"
                />
              </t-form-item>
            </t-col>
          </t-row>
          <t-button theme="primary" :loading="submitting" @click="onSubmit">
            {{ submitting ? t('flashcardGen.generating') : t('flashcardGen.submit') }}
          </t-button>
        </t-form>
      </div>

      <div v-if="result?.flashcards?.length" class="fc-grid">
        <div
          v-for="(card, i) in result.flashcards"
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
      <div v-else-if="result && !result.flashcards?.length" class="fc-empty">
        {{ t('flashcardGen.empty') }}
      </div>
    </div>
  </t-dialog>
</template>

<style scoped lang="less">
.fc-root {
  max-height: 70vh;
  overflow: auto;
}
.fc-hint {
  color: var(--td-text-color-secondary);
  font-size: 13px;
  margin: 0 0 16px;
}
.fc-form {
  margin-bottom: 20px;
}
.fc-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 16px;
  margin-top: 8px;
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
}
.fc-card-inner.flipped {
  transform: rotateY(180deg);
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
