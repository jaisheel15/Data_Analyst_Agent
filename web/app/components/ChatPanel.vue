<script setup lang="ts">
const { turns, busy, ask, datasets } = useAnalyst()
const question = ref('')
const log = ref<HTMLElement | null>(null)

async function submit() {
  const q = question.value.trim()
  if (!q || busy.value) return
  question.value = ''
  const done = ask(q)
  await nextTick()
  log.value?.scrollTo({ top: log.value.scrollHeight, behavior: 'smooth' })
  await done
}

// Follow the answer as it streams in, unless the reader has scrolled up.
watch(turns, async () => {
  const el = log.value
  if (!el || el.scrollHeight - el.scrollTop - el.clientHeight > 120) return
  await nextTick()
  el.scrollTop = el.scrollHeight
}, { deep: true })

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    submit()
  }
}
</script>

<template>
  <section class="panel chat">
    <h2>Ask</h2>
    <div ref="log" class="log">
      <p v-if="!turns.length" class="muted empty">
        {{ datasets.length ? 'Ask a question about your data, e.g. "Which region had the highest revenue last quarter?"' : 'Upload a dataset or try a sample to begin.' }}
      </p>
      <TurnView v-for="(t, i) in turns" :key="i" :turn="t" />
    </div>
    <form class="ask" @submit.prevent="submit">
      <textarea
        v-model="question"
        rows="2"
        placeholder="Ask about your data…"
        :disabled="busy"
        maxlength="4000"
        @keydown="onKey"
      />
      <button type="submit" :disabled="busy || !question.trim()">{{ busy ? 'Working…' : 'Ask' }}</button>
    </form>
  </section>
</template>
