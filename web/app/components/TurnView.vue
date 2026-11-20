<script setup lang="ts">
import DOMPurify from 'dompurify'
import { marked } from 'marked'
import type { Step, Turn } from '~/composables/types'

const props = defineProps<{ turn: Turn }>()
const showSteps = ref(true)

const answerHtml = computed(() =>
  props.turn.answer ? DOMPurify.sanitize(marked.parse(props.turn.answer, { async: false })) : '')

function code(step: Step): string {
  const i = step.input as Record<string, unknown>
  if (typeof i.sql === 'string') return i.sql
  if (typeof i.code === 'string') return i.code
  return JSON.stringify(i)
}

const stepCount = computed(() => props.turn.steps.length)
</script>

<template>
  <article class="turn">
    <p class="question">{{ turn.question }}</p>

    <div v-if="stepCount" class="steps">
      <button type="button" class="link" @click="showSteps = !showSteps">
        {{ showSteps ? '▾' : '▸' }} {{ stepCount }} step{{ stepCount === 1 ? '' : 's' }}
      </button>
      <ol v-if="showSteps">
        <li v-for="s in turn.steps" :key="s.step" :class="{ failed: !!s.error, pending: !s.done }">
          <div class="step-head">
            <code>{{ s.tool }}</code>
            <span v-if="!s.done" class="muted">running…</span>
            <span v-else-if="s.error" class="bad">error, retrying</span>
          </div>
          <pre v-if="s.tool !== 'describe_dataset'">{{ code(s) }}</pre>
          <pre v-if="s.error" class="err">{{ s.error }}</pre>
          <pre v-else-if="s.summary" class="out">{{ s.summary }}</pre>
        </li>
      </ol>
    </div>

    <div v-if="turn.charts.length" class="charts">
      <img v-for="url in turn.charts" :key="url" :src="url" alt="Chart produced by the agent" loading="lazy">
    </div>

    <div v-if="answerHtml" class="answer" v-html="answerHtml" />
    <p v-else-if="turn.status === 'running'" class="muted">Thinking…</p>

    <p v-if="turn.status === 'budget_exhausted'" class="warn">The agent hit its step limit, so this answer may be incomplete.</p>
    <p v-if="turn.status === 'error'" class="bad">{{ turn.error }}</p>
    <p v-if="turn.costUsd !== undefined" class="muted cost">Cost: ${{ turn.costUsd.toFixed(4) }}</p>
  </article>
</template>
