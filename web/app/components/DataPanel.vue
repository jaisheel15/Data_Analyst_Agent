<script setup lang="ts">
const { datasets, upload, uploadSample, createSession, busy } = useAnalyst()
const uploading = ref(false)
const dragging = ref(false)
const open = ref<string | null>(null)

const samples = [
  { label: 'Retail sales', path: '/samples/retail_sales.csv' },
  { label: 'Employees', path: '/samples/employees.csv' },
]

async function handle(files: FileList | null | undefined) {
  if (!files?.length) return
  uploading.value = true
  try {
    for (const f of Array.from(files)) await upload(f)
  } finally {
    uploading.value = false
  }
}

async function sample(path: string) {
  uploading.value = true
  try {
    await uploadSample(path)
  } finally {
    uploading.value = false
  }
}

function onDrop(e: DragEvent) {
  dragging.value = false
  handle(e.dataTransfer?.files)
}

function cell(v: unknown): string {
  return v === null || v === undefined ? 'NULL' : String(v)
}
</script>

<template>
  <section class="panel data">
    <h2>Data</h2>
    <label
      class="drop"
      :class="{ dragging }"
      @dragover.prevent="dragging = true"
      @dragleave="dragging = false"
      @drop.prevent="onDrop"
    >
      <input type="file" accept=".csv,.parquet" multiple hidden @change="handle(($event.target as HTMLInputElement).files)">
      <strong>{{ uploading ? 'Uploading…' : 'Drop a CSV or Parquet file' }}</strong>
      <span>or click to choose (max 25 MB)</span>
    </label>
    <p class="notice">
      Don't upload sensitive data: column names and sample rows are sent to the model provider.
      Data is deleted after 24 hours.
    </p>
    <div class="samples">
      <span>Try a sample:</span>
      <button v-for="s in samples" :key="s.path" type="button" :disabled="uploading" @click="sample(s.path)">
        {{ s.label }}
      </button>
    </div>

    <ul class="tables">
      <li v-for="t in datasets" :key="t.name">
        <button type="button" class="table-name" @click="open = open === t.name ? null : t.name">
          <code>{{ t.name }}</code> <span class="muted">{{ t.rows.toLocaleString() }} rows · {{ t.columns.length }} columns</span>
        </button>
        <div v-if="open === t.name" class="schema">
          <table>
            <thead><tr><th v-for="c in t.columns" :key="c.name">{{ c.name }}<br><small>{{ c.type }}</small></th></tr></thead>
            <tbody>
              <tr v-for="(row, i) in t.sample_rows ?? []" :key="i"><td v-for="(v, j) in row" :key="j">{{ cell(v) }}</td></tr>
            </tbody>
          </table>
          <p v-if="!t.sample_rows?.length" class="muted">Sample rows appear right after upload.</p>
        </div>
      </li>
    </ul>
    <p v-if="!datasets.length" class="muted">No datasets yet.</p>
    <button type="button" class="link" :disabled="busy" @click="createSession">Start a new session</button>
  </section>
</template>
