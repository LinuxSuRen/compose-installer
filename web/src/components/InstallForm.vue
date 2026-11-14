<template>
  <div class="card">
    <h3>Install: deploy a bundle to the local machine or a remote SSH host</h3>
    <div class="form-grid">
      <div class="form-item wide">
        <label>Bundle path (tar.gz on the machine running this tool)</label>
        <PathPicker v-model="form.bundlePath" type="file" placeholder="dist/&lt;product&gt;-&lt;env&gt;-….tar.gz" @update:model-value="probe" />
        <div v-if="bundleInfo" :class="{ 'bundle-info': true, error: !!bundleInfo.error }">
          <template v-if="bundleInfo.error">{{ bundleInfo.error }}</template>
          <template v-else>
            Product <b>{{ bundleInfo.product }}</b> · env <b>{{ bundleInfo.env }}</b> · version
            <b>{{ bundleInfo.version || '—' }}</b> · platform {{ bundleInfo.platform || 'any' }} ·
            {{ (bundleInfo.images || []).length }} images
            <template v-if="bundleOnline"> · <b>online bundle</b> (the target machine pulls images)</template>
            <template v-else> · offline bundle</template>
          </template>
        </div>
        <div v-if="bundleOnline" class="hint" style="margin-top:6px">
          Online bundle: at install time the target machine runs docker login and docker compose pull,
          so it needs network access to the registry. Private registry credentials go under Advanced.
        </div>
      </div>
      <div class="form-item">
        <label>Install mode</label>
        <select v-model="form.mode">
          <option value="remote">Remote (SSH)</option>
          <option value="local">Local (tool runs on {{ hostInfo ? hostInfo.osLabel : 'the target' }})</option>
        </select>
      </div>
      <div class="form-item">
        <label>Install directory (on the target)</label>
        <input v-model.trim="form.installDir" :placeholder="installDirPlaceholder" />
      </div>
      <div v-if="form.mode === 'local' && localHint" class="hint" style="grid-column:1/-1;margin:0">
        <b>{{ hostInfo.osLabel }}</b> detected · {{ localHint }}
        <a v-if="needDockerDesktop" :href="dockerDesktopURL" target="_blank" style="margin-left:4px">Get Docker Desktop ↗</a>
      </div>

      <template v-if="form.mode === 'remote'">
        <div class="form-item">
          <label>SSH host</label>
          <input v-model.trim="form.ssh.host" placeholder="192.168.1.100" />
        </div>
        <div class="form-item">
          <label>SSH port</label>
          <input v-model.number="form.ssh.port" type="number" />
        </div>
        <div class="form-item">
          <label>SSH user</label>
          <input v-model.trim="form.ssh.user" placeholder="root (or a user with passwordless sudo)" />
        </div>
        <div class="form-item">
          <label>SSH password (or a key under Advanced)</label>
          <input v-model="form.ssh.password" type="password" autocomplete="new-password" />
        </div>
      </template>

      <div v-if="requiredKeys.length" class="form-item wide">
        <label>Required environment variables (declared by this bundle<span class="req"> *</span>)</label>
        <div class="form-grid" style="grid-template-columns:1fr 1fr;margin-top:6px">
          <div v-for="k in requiredKeys" :key="k" class="form-item">
            <label>{{ k }}<span class="req"> *</span></label>
            <input v-model.trim="form.envVars[k]" :placeholder="`Value of ${k}`" />
          </div>
        </div>
      </div>
    </div>

    <details class="adv">
      <summary>Advanced</summary>
      <div class="form-grid" style="margin-top: 10px">
        <div class="form-item">
          <label>Docker install</label>
          <select v-model="dockerInstall">
            <option value="auto">Auto detect (skip when a working docker exists)</option>
            <option value="offline">Always install offline materials</option>
            <option value="skip">Skip (docker already installed)</option>
          </select>
        </div>
        <div class="form-item">
          <label>NAT fix (when the target daemon.json has "iptables": false)</label>
          <select v-model="natFixPolicy">
            <option value="restart">Remove the flag and restart docker (recommended)</option>
            <option value="manual">Add NAT rules without restarting docker</option>
            <option value="skip">Skip (warn only; containers may lose outbound network)</option>
          </select>
        </div>
        <div class="form-item" style="flex-direction:row;align-items:center;gap:8px">
          <input id="docker-force" type="checkbox" v-model="dockerForce" style="width:auto;margin:0" />
          <label for="docker-force" style="margin:0;cursor:pointer">
            Force reinstall docker (runs the offline installer even when a healthy docker exists)
          </label>
        </div>
        <div class="form-item">
          <label>Install script (entry point inside the offline materials)</label>
          <input v-model.trim="form.installScript" placeholder="install.sh" />
        </div>
        <div class="form-item">
          <label>Install script arguments (whitespace separated)</label>
          <input v-model.trim="scriptArgs" placeholder="--yes" />
        </div>
        <div class="form-item">
          <label>Registry (docker login on the target, online bundles)</label>
          <input v-model.trim="form.registry" placeholder="registry.example.com" />
        </div>
        <div class="form-item">
          <label>Registry username (leave empty to reuse the target's docker login state)</label>
          <input v-model.trim="form.registryUser" />
        </div>
        <div class="form-item">
          <label>Registry password (pair with the username)</label>
          <input v-model="form.registryPassword" type="password" autocomplete="new-password" />
        </div>
        <template v-if="form.mode === 'remote'">
          <div class="form-item">
            <label>SSH key file (optional; empty = default keys under ~/.ssh)</label>
            <PathPicker v-model="form.ssh.keyFile" type="file" placeholder="~/.ssh/id_rsa" />
          </div>
          <div class="form-item">
            <label>SSH key passphrase (optional)</label>
            <input v-model="form.ssh.keyPassphrase" type="password" autocomplete="new-password" />
          </div>
        </template>
        <div v-if="form.mode === 'remote' || showLocalSudo" class="form-item">
          <label>Sudo password ({{ form.mode === 'remote' ? 'target' : 'local' }} non-root user without passwordless sudo)</label>
          <input v-model="form.sudoPassword" type="password" autocomplete="new-password"
            :placeholder="form.mode === 'remote' ? 'Empty = reuse the SSH password' : 'Only needed when docker requires sudo'" />
        </div>
      </div>
    </details>

    <button class="primary" :disabled="jobStatus === 'running'" @click="start">
      {{ jobStatus === 'running' ? 'Installing…' : 'Start install' }}
    </button>
    <div v-if="hint" class="hint" style="margin-top:8px">{{ hint }}</div>
  </div>
</template>

<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '../api'
import PathPicker from './PathPicker.vue'
import { sessionState, sessionRef } from '../useSession'

const props = defineProps({
  jobStatus: { type: String, default: '' } // '' | running | success | failed
})
const emit = defineEmits(['started'])

const form = sessionState('compose-installer.install', {
  mode: 'remote',
  ssh: { host: '', port: 22, user: 'root', password: '', keyFile: '', keyPassphrase: '' },
  bundlePath: '',
  installDir: '',
  installScript: 'install.sh',
  envVars: {},
  sudoPassword: '',
  registry: '',
  registryUser: '',
  registryPassword: ''
}, ['ssh.password', 'ssh.keyPassphrase', 'sudoPassword', 'registryPassword', 'envVars'])
// Password-like fields are never persisted; required-env values may hold
// tokens/secrets, so they stay out of sessionStorage too.

const dockerInstall = sessionRef('compose-installer.install.docker', 'auto')
const dockerForce = sessionRef('compose-installer.install.dockerForce', false)
const natFixPolicy = sessionRef('compose-installer.install.natFixPolicy', 'restart')
const scriptArgs = sessionRef('compose-installer.install.args', '--yes')

const bundleInfo = ref(null)
const hint = ref('')
const hostInfo = ref(null) // machine running the tool: {os, osLabel, isRoot, home, bash}

const bundleOk = computed(() => !!(bundleInfo.value && !bundleInfo.value.error))
const bundleOnline = computed(() => !!(bundleOk.value && bundleInfo.value.online))
// The generic core: every manifest requiredEnv key becomes an input; values
// are sent as envVars (highest precedence on the target .env).
const requiredKeys = computed(() =>
  (bundleOk.value && Array.isArray(bundleInfo.value.requiredEnv)) ? bundleInfo.value.requiredEnv : [])
const bundleName = computed(() =>
  bundleOk.value ? (bundleInfo.value.installDirName || bundleInfo.value.product || '') : '')

// Keep one input slot per required key (missed keys default to empty).
watch(requiredKeys, keys => {
  for (const k of keys) {
    if (!(k in form.envVars)) form.envVars[k] = ''
  }
}, { immediate: true })

// Default install dir comes from the backend (/api/default-install-dir, same
// fallback the installer uses); refreshed when the mode, SSH user, or bundle
// (installDirName) changes.
async function refreshDefaultDir () {
  if (!bundleName.value) return
  try {
    const { dir } = await api.defaultInstallDir(bundleName.value, form.mode, form.ssh.user)
    if (dir) form.installDir = dir
  } catch { /* ignore, keep the current value */ }
}
watch([() => form.mode, () => form.ssh.user, bundleName], refreshDefaultDir, { immediate: true })

// Local mode on Linux/macOS as non-root: offer a sudo password (Windows has none).
const showLocalSudo = computed(() =>
  form.mode === 'local' && !!hostInfo.value &&
  (hostInfo.value.os === 'linux' || hostInfo.value.os === 'darwin') &&
  !hostInfo.value.isRoot)

// macOS/Windows get no automatic docker install; point to Docker Desktop.
const dockerDesktopURL = 'https://www.docker.com/products/docker-desktop/'
const needDockerDesktop = computed(() =>
  form.mode === 'local' && !!hostInfo.value &&
  (hostInfo.value.os === 'darwin' || hostInfo.value.os === 'windows'))

const localHint = computed(() => {
  if (form.mode !== 'local' || !hostInfo.value) return ''
  const h = hostInfo.value
  if (h.os === 'darwin') {
    return 'docker is not installed automatically: install and start Docker Desktop; the current user can use docker without sudo.'
  }
  if (h.os === 'windows') {
    return `docker is not installed automatically: install Docker Desktop and Git for Windows (for bash${h.bash ? '' : ' — bash was not detected, install Git for Windows first'}).`
  }
  if (h.isRoot) return 'Running as root: no sudo needed.'
  return 'Non-root user: fill in the sudo password (under Advanced) or add the user to the docker group.'
})

const installDirPlaceholder = computed(() => {
  if (form.mode === 'local' && hostInfo.value && hostInfo.value.os !== 'linux') {
    const home = (hostInfo.value.home || '~').replace(/\\/g, '/')
    return `Empty = ${home}/${bundleName.value || '<name>'}`
  }
  return `Empty = /opt/${bundleName.value || '<name>'}`
})

onMounted(async () => {
  try {
    hostInfo.value = await api.hostInfo()
    // Local non-Linux: drop any remembered /opt default so the backend can
    // fall back to the home directory.
    if (form.mode === 'local' && hostInfo.value.os !== 'linux' && form.installDir.startsWith('/opt/')) {
      form.installDir = ''
    }
  } catch { /* ignore, fall back to generic display */ }
  // Probe the remembered bundle so the manifest card and required env vars
  // show up immediately.
  if (form.bundlePath) probe()
})

async function probe () {
  bundleInfo.value = null
  if (!form.bundlePath) return
  try {
    bundleInfo.value = await api.bundleManifest(form.bundlePath)
  } catch (e) {
    bundleInfo.value = { error: e.message }
  }
}

async function start () {
  hint.value = ''
  if (!form.bundlePath) { hint.value = 'Select a bundle file first'; return }
  if (form.mode === 'remote' && !form.ssh.host) { hint.value = 'SSH host is required for a remote install'; return }
  // Front-end mirror of the backend check: every requiredEnv key must be set.
  const missing = requiredKeys.value.filter(k => !(form.envVars[k] || '').trim())
  if (missing.length) {
    hint.value = `Missing required environment variables: ${missing.join(', ')}`
    return
  }
  const envVars = {}
  for (const k of requiredKeys.value) envVars[k] = (form.envVars[k] || '').trim()
  const body = {
    mode: form.mode,
    ssh: form.mode === 'remote' ? { ...form.ssh } : {},
    bundlePath: form.bundlePath,
    installDir: form.installDir,
    dockerInstall: dockerInstall.value,
    dockerForce: dockerForce.value,
    installScript: form.installScript,
    installScriptArgs: scriptArgs.value ? scriptArgs.value.split(/\s+/).filter(Boolean) : [],
    envVars,
    sudoPassword: form.sudoPassword,
    registry: form.registry,
    registryUser: form.registryUser,
    registryPassword: form.registryPassword,
    natFixPolicy: natFixPolicy.value
  }
  try {
    const { jobId } = await api.install(body)
    hint.value = `Job submitted: ${jobId}`
    emit('started', jobId)
  } catch (e) {
    hint.value = `Submit failed: ${e.message}`
  }
}
</script>
