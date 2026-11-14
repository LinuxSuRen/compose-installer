<template>
  <div class="container">
    <header class="top">
      <h1>compose-installer</h1>
      <span class="sub">Pack (online machine) · Install (local / SSH remote)</span>
      <span v-if="ver" class="sub" style="margin-left:auto">v{{ ver }}</span>
    </header>

    <div class="tabs">
      <button v-if="packVisible" :class="{ active: tab === 'pack' }" @click="tab = 'pack'">Pack</button>
      <button :class="{ active: tab === 'install' }" @click="tab = 'install'">Install</button>
    </div>

    <PackForm v-if="tab === 'pack'" @started="onStarted" />
    <InstallForm v-else :job-status="jobStatus" @started="onStarted" />

    <LogPanel v-if="jobId" :job-id="jobId" @status="jobStatus = $event" />
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { api } from './api'
import PackForm from './components/PackForm.vue'
import InstallForm from './components/InstallForm.vue'
import LogPanel from './components/LogPanel.vue'

// Selected tab persists in localStorage (survives restarts); first use opens Install.
const TAB_KEY = 'compose-installer.tab'
let storedTab = null
try { storedTab = localStorage.getItem(TAB_KEY) } catch { /* ignore */ }
const tab = ref(storedTab === 'pack' ? 'pack' : 'install')
watch(tab, v => {
  try { localStorage.setItem(TAB_KEY, v) } catch { /* ignore */ }
})

const jobId = ref('')
const jobStatus = ref('') // '' | running | success | failed
const ver = ref('')

// Pack tab visibility: hidden by default when the backend reports hidePack
// (container deliverable); typing openthebox on an empty spot unlocks it for
// this browser session (hint printed to the console).
const UNLOCK_KEY = 'compose-installer.pack-unlocked'
const packVisible = ref(true)

onMounted(async () => {
  try {
    const h = await api.health()
    ver.value = h.version || ''
    if (String(h.hidePack) === 'true' && sessionStorage.getItem(UNLOCK_KEY) !== '1') {
      packVisible.value = false
      if (tab.value === 'pack') tab.value = 'install'
      // eslint-disable-next-line no-console
      console.info('[compose-installer] The Pack tab is hidden on this server. Type "openthebox" on an empty area of the page to reveal it.')
    }
  } catch { /* ignore */ }
})

// Typed-sequence detection (single-character keys only; typing inside inputs,
// textareas and contenteditable elements is skipped via e.target).
const SEQUENCE = 'openthebox'
let typed = ''
function onKeydown (e) {
  const t = e.target
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return
  if (!e.key || e.key.length !== 1) return
  typed = (typed + e.key.toLowerCase()).slice(-SEQUENCE.length)
  if (typed === SEQUENCE && !packVisible.value) {
    packVisible.value = true
    tab.value = 'pack'
    try { sessionStorage.setItem(UNLOCK_KEY, '1') } catch { /* ignore */ }
    // eslint-disable-next-line no-console
    console.info('[compose-installer] Pack tab is now visible for this browser session.')
  }
}
addEventListener('keydown', onKeydown)
onUnmounted(() => removeEventListener('keydown', onKeydown))

function onStarted (id) {
  jobId.value = id
  jobStatus.value = 'running'
}
</script>

<script>
export default { name: 'App' }
</script>
