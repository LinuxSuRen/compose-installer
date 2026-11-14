import { reactive, ref, watch } from 'vue'

// Persist form parameters to sessionStorage: values survive page reloads and
// accidental reopens within the same tab session. Sensitive fields listed in
// exclude (password-like fields, "a.b" dot paths supported) are never written.

function readStored (key) {
  try {
    const raw = sessionStorage.getItem(key)
    if (raw != null) return JSON.parse(raw)
  } catch { /* ignore corrupted data */ }
  return undefined
}

function writeStored (key, value) {
  try {
    sessionStorage.setItem(key, JSON.stringify(value))
  } catch { /* storage full/disabled: ignore */ }
}

function delPath (obj, pathStr) {
  const parts = pathStr.split('.')
  let o = obj
  for (let i = 0; i < parts.length - 1; i++) {
    o = o?.[parts[i]]
    if (!o || typeof o !== 'object') return
  }
  delete o[parts[parts.length - 1]]
}

// Object form state: sessionState('key', {defaults...}, ['excluded.fields'])
export function sessionState (key, defaults, exclude = []) {
  const stored = readStored(key)
  const state = reactive(stored ? { ...defaults, ...stored } : { ...defaults })
  watch(state, val => {
    const copy = JSON.parse(JSON.stringify(val))
    for (const p of exclude) delPath(copy, p)
    writeStored(key, copy)
  }, { deep: true })
  return state
}

// Single-value state (ref): sessionRef('key', defaultValue)
export function sessionRef (key, defaultValue) {
  const stored = readStored(key)
  const r = ref(stored !== undefined ? stored : defaultValue)
  watch(r, v => writeStored(key, v), { deep: true })
  return r
}
