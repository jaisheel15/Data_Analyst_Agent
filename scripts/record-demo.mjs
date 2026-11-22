// Records the README demo against the running stack (http://localhost:3000).
// Needs: npm i playwright-core (anywhere), a system Chromium, and ffmpeg.
// Frames are screenshots stitched by the system ffmpeg, so nothing is downloaded.
//   NODE_PATH=/path/to/node_modules node scripts/record-demo.mjs docs/demo
// Writes <out>.mp4 and <out>.gif. Uses a few cents of the daily chat budget.
import { createRequire } from 'node:module'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'

const require = createRequire(import.meta.url)
const { chromium } = require('playwright-core')
const out = process.argv[2] ?? 'docs/demo'
const questions = [
  'Why did West revenue drop in Q3 2025?',
  'Show me a chart of revenue by region and quarter.',
]
const size = { width: 1280, height: 800 }
const frames = mkdtempSync(join(tmpdir(), 'demo-'))

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM ?? '/usr/bin/chromium', args: ['--no-sandbox'] })
const ctx = await browser.newContext({ viewport: size })
const page = await ctx.newPage()

let n = 0
let recording = true
const started = Date.now()
const grabber = (async () => {
  while (recording) {
    await page.screenshot({ path: join(frames, `f${String(n++).padStart(5, '0')}.png`) }).catch(() => {})
  }
})()
await page.goto('http://localhost:3000')
await page.waitForTimeout(1500)
await page.getByRole('button', { name: 'Retail sales' }).click()
await page.getByText('retail_sales', { exact: false }).first().waitFor()
await page.waitForTimeout(2000)

for (const q of questions) {
  const box = page.getByPlaceholder('Ask about your data…')
  await box.click()
  await box.pressSequentially(q, { delay: 45 })
  await page.waitForTimeout(600)
  await page.getByRole('button', { name: 'Ask' }).click()
  await page.getByRole('button', { name: 'Working…' }).waitFor()
  await page.getByRole('button', { name: 'Ask' }).waitFor({ timeout: 240_000 })
  await page.waitForTimeout(2500)
  await page.locator('.log').evaluate((el) => el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' }))
  await page.waitForTimeout(3500)
}
recording = false
await grabber
const fps = n / ((Date.now() - started) / 1000)
await ctx.close()
await browser.close()

const input = ['-framerate', fps.toFixed(3), '-i', join(frames, 'f%05d.png')]
mkdirSync(dirname(out), { recursive: true })
// 2x speed: the agent's thinking time is not the interesting part.
const speed = 'setpts=PTS/2'
execFileSync('ffmpeg', ['-y', '-loglevel', 'error', ...input, '-vf', `${speed},scale=1280:-2`, '-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-crf', '28', `${out}.mp4`])
execFileSync('ffmpeg', ['-y', '-loglevel', 'error', ...input, '-vf', `${speed},fps=8,scale=800:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96[p];[b][p]paletteuse=dither=bayer:bayer_scale=4`, `${out}.gif`])
console.log('wrote', `${out}.mp4`, `${out}.gif`)
