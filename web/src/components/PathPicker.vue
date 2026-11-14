<template>
  <div class="pp-root">
    <div class="pp-input-row">
      <input class="pp-input" :value="modelValue" :placeholder="placeholder"
             @input="$emit('update:modelValue', $event.target.value)" />
      <button type="button" class="pp-browse" @click="open">Browse…</button>
    </div>

    <div v-if="visible" class="pp-modal" @click.self="visible = false">
      <div class="pp-dialog">
        <div class="pp-head">
          <button type="button" class="pp-nav" @click="go(listing.parent)" :disabled="!listing.parent">↑ Up</button>
          <input class="pp-path" v-model.trim="manualPath" placeholder="Type a path and press Enter to jump there" @keyup.enter="go(manualPath)" />
          <button type="button" class="pp-close" @click="visible = false">×</button>
        </div>
        <div v-if="roots.length > 1" class="pp-drives">
          <span class="pp-drives-label">Drives</span>
          <button v-for="r in roots" :key="r" type="button" class="pp-drive"
                  :class="{ on: listing.path && listing.path[0] === r[0] }"
                  @click="go(r)">{{ r.replace('\\', '') }}</button>
        </div>
        <div class="pp-list">
          <div v-if="error" class="pp-error">{{ error }}</div>
          <div v-else-if="!entries.length" class="pp-empty">(empty directory)</div>
          <div v-for="e in entries" :key="e.name" class="pp-item" :class="{ sel: selected === e.name }"
               @click="select(e)" @dblclick="confirmSel(e)">
            <span class="pp-icon">{{ e.isDir ? '📁' : '📄' }}</span>
            <span class="pp-name">{{ e.name }}</span>
            <span class="pp-meta">{{ e.isDir ? '' : fmtSize(e.size) }}</span>
          </div>
        </div>
        <div class="pp-foot">
          <span v-if="type === 'file'" class="muted">
            {{ selected ? selected : 'No file selected' }}
          </span>
          <span v-else class="muted">The current directory will be used</span>
          <div class="pp-foot-right">
            <button v-if="type === 'file'" type="button" class="pp-cancel"
                    :disabled="!selected" @click="confirmName()">Select this file</button>
            <button v-else type="button" class="pp-cancel" @click="confirmDir()">Select current directory</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import { api } from '../api'

const props = defineProps({
  modelValue: { type: String, default: '' },
  type: { type: String, default: 'file' }, // file | dir
  placeholder: { type: String, default: '' }
})
const emit = defineEmits(['update:modelValue'])

const visible = ref(false)
const listing = ref({ path: '', parent: '', entries: [] })
const roots = ref([])
const entries = ref([])
const error = ref('')
const selected = ref('')
const manualPath = ref('')

async function open () {
  visible.value = true
  error.value = ''
  selected.value = ''
  await go(props.modelValue || '')
}

async function go (path) {
  error.value = ''
  try {
    const data = await api.fs(path || undefined)
    if (!data.isDir) { error.value = 'Not a directory'; return }
    listing.value = data
    entries.value = data.entries || []
    roots.value = data.roots || []
    manualPath.value = data.path
    // The start/jump path may be a file: the backend then lists its parent
    // directory and reports the file via focus — preselect it here so
    // "Select this file" is immediately usable.
    selected.value = ''
    if (data.focus && entries.value.some(e => !e.isDir && e.name === data.focus)) {
      selected.value = data.focus
    }
  } catch (e) {
    error.value = e.message
  }
}

function select (e) {
  if (e.isDir) {
    selected.value = ''
    go(join(listing.value.path, e.name))
  } else {
    selected.value = e.name
  }
}

function confirmSel (e) {
  if (e.isDir) return
  selected.value = e.name
  confirmName()
}

function confirmName () {
  if (!selected.value) return
  emit('update:modelValue', join(listing.value.path, selected.value))
  visible.value = false
}

function confirmDir () {
  emit('update:modelValue', listing.value.path)
  visible.value = false
}

function join (dir, name) {
  const sep = dir.includes('/') && !dir.includes('\\') ? '/' : '\\'
  return dir.replace(/[\\/]+$/, '') + sep + name
}

function fmtSize (n) {
  if (n > 1024 * 1024 * 1024) return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB'
  if (n > 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB'
  if (n > 1024) return (n / 1024).toFixed(0) + ' KB'
  return n + ' B'
}
</script>

<style scoped>
.pp-root { display: flex; flex-direction: column; min-width: 0; }
.pp-input-row { display: flex; gap: 6px; min-width: 0; }
.pp-input { flex: 1; min-width: 0; padding: 7px 10px; border: 1px solid var(--border); border-radius: 6px; font-size: 14px; }
.pp-browse { white-space: nowrap; border: 1px solid var(--border); background: #fff; border-radius: 6px; padding: 0 12px; cursor: pointer; font-size: 13px; }
.pp-browse:hover { border-color: var(--primary); color: var(--primary); }
.pp-modal { position: fixed; inset: 0; background: rgba(15,23,42,.45); display: flex; align-items: center; justify-content: center; z-index: 50; }
.pp-dialog { background: #fff; border-radius: 10px; width: 640px; max-width: 92vw; max-height: 80vh; display: flex; flex-direction: column; }
.pp-head { display: flex; gap: 8px; padding: 12px 12px 8px; align-items: center; }
.pp-nav { border: 1px solid var(--border); background: #fff; border-radius: 6px; padding: 6px 10px; cursor: pointer; font-size: 13px; }
.pp-nav:disabled { opacity: .4; cursor: default; }
.pp-path { flex: 1; padding: 6px 10px; border: 1px solid var(--border); border-radius: 6px; font-size: 13px; }
.pp-close { border: none; background: none; font-size: 20px; cursor: pointer; color: var(--muted); }
.pp-drives { display: flex; align-items: center; gap: 6px; padding: 0 12px 8px; flex-wrap: wrap; }
.pp-drives-label { font-size: 12px; color: var(--muted); margin-right: 2px; }
.pp-drive { border: 1px solid var(--border); background: #fff; border-radius: 6px; padding: 3px 12px; cursor: pointer; font-size: 13px; }
.pp-drive:hover { border-color: var(--primary); color: var(--primary); }
.pp-drive.on { background: var(--primary); border-color: var(--primary); color: #fff; }
.pp-list { flex: 1; overflow-y: auto; border-top: 1px solid var(--border); border-bottom: 1px solid var(--border); margin: 4px 12px; min-height: 240px; }
.pp-item { display: flex; align-items: center; gap: 8px; padding: 6px 10px; font-size: 13.5px; cursor: pointer; border-radius: 6px; }
.pp-item:hover { background: #eef2f7; }
.pp-item.sel { background: #dbeafe; }
.pp-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pp-meta { color: var(--muted); font-size: 12px; }
.pp-error { color: #b91c1c; padding: 12px; font-size: 13px; }
.pp-empty { color: var(--muted); padding: 12px; font-size: 13px; }
.pp-foot { display: flex; justify-content: space-between; align-items: center; padding: 10px 12px; font-size: 13px; gap: 8px; }
.pp-foot-right { display: flex; gap: 8px; }
.pp-cancel { background: var(--primary); color: #fff; border: none; border-radius: 6px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
.pp-cancel:disabled { background: #9ca3af; cursor: not-allowed; }
</style>
