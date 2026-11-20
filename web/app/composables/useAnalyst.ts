import type { SessionView, Step, Table, Turn } from './types'

const SESSION_KEY = 'daa.session'

function readStoredSession(): string | null {
  try {
    return localStorage.getItem(SESSION_KEY)
  } catch {
    return null // storage blocked: fall back to a fresh session each load
  }
}

function storeSession(id: string | null) {
  try {
    if (id) localStorage.setItem(SESSION_KEY, id)
    else localStorage.removeItem(SESSION_KEY)
  } catch {
    /* storage unavailable; the session just won't survive a reload */
  }
}

async function errorText(resp: Response): Promise<string> {
  try {
    const body = await resp.json()
    if (typeof body?.error === 'string') return body.error
  } catch {
    /* not JSON */
  }
  return `Request failed (${resp.status})`
}

/** All app state: the session, its datasets, and the conversation. */
export function useAnalyst() {
  const sessionId = useState<string | null>('sessionId', () => null)
  const datasets = useState<Table[]>('datasets', () => [])
  const turns = useState<Turn[]>('turns', () => [])
  const busy = useState<boolean>('busy', () => false)
  const notice = useState<string | null>('notice', () => null)

  async function createSession() {
    const resp = await fetch('/api/sessions', { method: 'POST' })
    if (!resp.ok) throw new Error(await errorText(resp))
    const { id } = await resp.json()
    sessionId.value = id
    storeSession(id)
    datasets.value = []
    turns.value = []
  }

  /** Restore the stored session (sessions expire after 24h) or start a new one. */
  async function init() {
    const stored = readStoredSession()
    if (stored) {
      const resp = await fetch(`/api/sessions/${stored}`)
      if (resp.ok) {
        const view: SessionView = await resp.json()
        sessionId.value = view.id
        datasets.value = view.datasets
        turns.value = pairMessages(view)
        return
      }
      storeSession(null)
    }
    await createSession()
  }

  async function upload(file: File) {
    if (!sessionId.value) return
    notice.value = null
    const form = new FormData()
    form.append('file', file)
    const resp = await fetch(`/api/sessions/${sessionId.value}/datasets`, { method: 'POST', body: form })
    if (!resp.ok) {
      notice.value = await errorText(resp)
      return
    }
    const { table } = await resp.json()
    datasets.value = [...datasets.value, table]
  }

  async function uploadSample(path: string) {
    const resp = await fetch(path)
    const blob = await resp.blob()
    await upload(new File([blob], path.split('/').pop() ?? 'sample.csv', { type: 'text/csv' }))
  }

  async function ask(question: string) {
    if (!sessionId.value || busy.value || !question.trim()) return
    notice.value = null
    busy.value = true
    turns.value.push({ question, steps: [], charts: [], answer: '', status: 'running' })
    // Mutate through the reactive array so the UI updates as events arrive.
    const turn = turns.value[turns.value.length - 1]!
    try {
      const resp = await fetch(`/api/sessions/${sessionId.value}/messages`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ question }),
      })
      if (!resp.ok || !resp.body) {
        turn.status = 'error'
        turn.error = await errorText(resp)
        return
      }
      await readEvents(resp.body, (type, data) => applyEvent(turn, type, data))
      if (turn.status === 'running') {
        turn.status = 'error'
        turn.error = 'The connection closed before the answer arrived.'
      }
    } catch (err) {
      turn.status = 'error'
      turn.error = err instanceof Error ? err.message : String(err)
    } finally {
      busy.value = false
    }
  }

  return { sessionId, datasets, turns, busy, notice, init, createSession, upload, uploadSample, ask }
}

/** Rebuild question/answer pairs from stored messages (steps are not stored for display). */
function pairMessages(view: SessionView): Turn[] {
  const turns: Turn[] = []
  for (const m of view.messages) {
    if (m.role === 'user') {
      turns.push({ question: m.content, steps: [], charts: [], answer: '', status: 'error',
        error: 'This question did not finish.' })
    } else if (turns.length) {
      const last = turns[turns.length - 1]!
      last.answer = m.content
      last.status = 'done'
      last.error = undefined
    }
  }
  return turns
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function applyEvent(turn: Turn, type: string, data: any) {
  switch (type) {
    case 'tool_call':
      turn.steps.push({ step: data.step, tool: data.tool, input: data.input ?? {}, done: false })
      break
    case 'tool_result': {
      const step = turn.steps.find((s: Step) => s.step === data.step)
      if (step) Object.assign(step, { summary: data.summary, error: data.error, done: true })
      break
    }
    case 'chart':
      turn.charts.push(data.url)
      break
    case 'answer_delta':
      turn.answer += data.text
      break
    case 'done':
      turn.answer = data.answer
      turn.status = data.status
      turn.costUsd = data.cost_usd
      break
    case 'error':
      turn.status = 'error'
      turn.error = data.error
      break
  }
}

/** Parse a text/event-stream body. EventSource cannot POST, so this reads fetch's stream. */
async function readEvents(body: ReadableStream<Uint8Array>, onEvent: (type: string, data: unknown) => void) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let sep: number
    while ((sep = buffer.indexOf('\n\n')) >= 0) {
      const block = buffer.slice(0, sep)
      buffer = buffer.slice(sep + 2)
      let type = 'message'
      let data = ''
      for (const line of block.split('\n')) {
        if (line.startsWith('event: ')) type = line.slice(7)
        else if (line.startsWith('data: ')) data += line.slice(6)
      }
      try {
        onEvent(type, data ? JSON.parse(data) : null)
      } catch {
        /* ignore a malformed event rather than abort the stream */
      }
    }
  }
}
