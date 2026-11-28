// Shapes of the Go public API (api/internal/{session,dataset,chat,tool,chart}/handler.go).

export interface Column { name: string; type: string }

export interface Table {
  name: string
  rows: number
  columns: Column[]
  sample_rows?: unknown[][] | null
}

export interface StoredMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  created_at: string
}

export interface SessionView {
  id: string
  datasets: Table[]
  messages: StoredMessage[]
}

export interface Step {
  step: number
  tool: string
  input: Record<string, unknown>
  summary?: string
  error?: string
  done: boolean
}

export type TurnStatus = 'running' | 'done' | 'budget_exhausted' | 'error'

export interface Turn {
  question: string
  steps: Step[]
  charts: string[] // chart URLs
  answer: string
  status: TurnStatus
  error?: string
  costUsd?: number
}
