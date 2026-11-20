<script setup lang="ts">
const { init, notice } = useAnalyst()
const startError = ref<string | null>(null)

onMounted(async () => {
  try {
    await init()
  } catch (err) {
    startError.value = `Could not reach the API: ${err instanceof Error ? err.message : err}`
  }
})
</script>

<template>
  <div class="shell">
    <header class="top">
      <h1>Data Analyst Agent</h1>
      <p class="tagline">Upload a dataset, ask a question, watch the agent work.</p>
    </header>
    <p v-if="startError" class="banner error">{{ startError }}</p>
    <p v-if="notice" class="banner error">{{ notice }}</p>
    <main class="layout">
      <DataPanel />
      <ChatPanel />
    </main>
  </div>
</template>
