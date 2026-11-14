<template>
  <div class="card">
    <div :class="['status', status]">
      {{ statusText }}
      <span class="muted" style="margin-left:10px">⏱ {{ elapsedText }}</span>
      <span v-if="jobId" class="muted">({{ jobId }})</span>
    </div>
    <div ref="box" class="log-box">{{ lines.join('\n') }}</div>
  </div>
</template>

<script setup>
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '../api'

const props = defineProps({ jobId: { type: String, required: true } })
const emit = defineEmits(['status'])

const lines = ref([])
const status = ref('running')
const errText = ref('')
const box = ref(null)
const startedAtMs = ref(0)
const durationMs = ref(0)
const nowMs = ref(Date.now())

const statusText = ref('Running…')

// Elapsed time ticks locally every second while running; once finished, the
// server-reported total duration wins.
const elapsedText = computed(() => {
  if (status.value !== 'running' && durationMs.value) {
    return fmt(durationMs.value)
  }
  if (!startedAtMs.value) return '—'
  return fmt(Math.max(0, nowMs.value - startedAtMs.value))
})

let offset = 0
let pollTimer = null
let tickTimer = null

watch(() => props.jobId, reset, { immediate: true })

function reset () {
  lines.value = []
  status.value = 'running'
  statusText.value = 'Running…'
  errText.value = ''
  startedAtMs.value = 0
  durationMs.value = 0
  offset = 0
  clearInterval(pollTimer)
  clearInterval(tickTimer)
  pollTimer = setInterval(poll, 1000)
  tickTimer = setInterval(() => { nowMs.value = Date.now() }, 1000)
  poll()
}

async function poll () {
  if (!props.jobId) return
  try {
    const data = await api.job(props.jobId, offset)
    if (data.lines?.length) {
      lines.value.push(...data.lines)
      offset = data.total
      nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight })
    }
    status.value = data.status
    emit('status', data.status)
    errText.value = data.err || ''
    if (data.startedAtMs) startedAtMs.value = data.startedAtMs
    if (data.durationMs) durationMs.value = data.durationMs
    if (data.status === 'success') statusText.value = '✔ Success'
    if (data.status === 'failed') statusText.value = '✖ Failed: ' + (errText.value || 'see log')
    if (data.status !== 'running') {
      clearInterval(pollTimer)
      clearInterval(tickTimer)
      pollTimer = null
      tickTimer = null
    }
  } catch { /* transient network error: retried next round */ }
}

function fmt (ms) {
  const total = Math.floor(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
}

onBeforeUnmount(() => { clearInterval(pollTimer); clearInterval(tickTimer) })
</script>
