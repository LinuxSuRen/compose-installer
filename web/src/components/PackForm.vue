<template>
  <div class="card">
    <h3>{{ form.online
      ? 'Pack: deployment files only → lightweight online bundle (the target machine pulls images at install time)'
      : 'Pack: pull all images + deployment files → single offline bundle (needs network and a local docker)' }}</h3>
    <div class="form-grid">
      <div class="form-item wide">
        <label>Project directory (contains the compose files / compose-installer.json)</label>
        <PathPicker v-model="form.projectDir" type="dir" placeholder="Empty = auto-detect upward from the executable" @update:model-value="queueLoadProject" />
        <span v-if="projectError" class="hint" style="color:#b91c1c">{{ projectError }}</span>
      </div>
      <div class="form-item">
        <label>Product</label>
        <select v-model="form.product" :disabled="!productNames.length" @change="onProductChange">
          <option v-if="!productNames.length" value="">No products found</option>
          <option v-for="n in productNames" :key="n" :value="n">{{ n }}</option>
        </select>
        <span v-if="productError" class="hint" style="color:#b91c1c">{{ productError }}</span>
      </div>
      <div class="form-item">
        <label>Environment</label>
        <select v-model="form.env" :disabled="!envOptions.length" @change="onEnvChange">
          <option v-if="!envOptions.length" value="">—</option>
          <option v-for="e in envOptions" :key="e" :value="e">{{ e }}</option>
        </select>
      </div>
      <div class="form-item" v-if="versionOptions">
        <label>Version</label>
        <select v-model="form.version">
          <option v-for="v in versionOptions" :key="v" :value="v">
            {{ v }}{{ v === latestVersion ? ' (latest)' : '' }}
          </option>
        </select>
        <span class="hint" v-if="!versionOptions.length">{{ productError || 'No version manifest found for this environment' }}</span>
      </div>
      <div class="form-item">
        <label>Target platform (images are pulled for this architecture)</label>
        <select v-model="form.platform">
          <option value="">Auto (follow the packing machine)</option>
          <option value="linux/amd64">linux/amd64 (x86_64)</option>
          <option value="linux/arm64">linux/arm64 (ARM, e.g. Jetson)</option>
        </select>
      </div>
      <div class="form-item wide" v-if="profileOptions.length">
        <label>Compose profiles (discovered from the selected environment, optional)</label>
        <div class="profile-box">
          <label v-for="p in profileOptions" :key="p" class="profile-chk">
            <input type="checkbox" :value="p" v-model="form.profileSel" style="width:auto;margin:0" /> {{ p }}
          </label>
        </div>
      </div>
      <div class="form-item wide" style="flex-direction:row;align-items:center;gap:8px">
        <input id="pack-online" type="checkbox" v-model="form.online" style="width:auto;margin:0" />
        <label for="pack-online" style="margin:0;cursor:pointer">
          Online bundle (no images and no docker offline materials; the target machine pulls over the network)
        </label>
      </div>
      <div class="form-item">
        <label>Registry (for docker login on the packing machine)</label>
        <input v-model.trim="form.registry" placeholder="registry.example.com" :disabled="form.online" />
      </div>
      <div class="form-item">
        <label>Registry username ({{ form.online ? 'online bundles need no login' : 'leave empty to reuse the local docker login state' }})</label>
        <input v-model.trim="form.registryUser" :disabled="form.online" />
      </div>
      <div class="form-item">
        <label>Registry password</label>
        <input v-model="form.registryPassword" type="password" autocomplete="new-password" :disabled="form.online" />
      </div>
      <div class="form-item wide">
        <label>Output bundle path (optional)</label>
        <PathPicker v-model="form.output" type="file" placeholder="Empty = auto-named into <project>/dist/" />
      </div>
    </div>

    <details class="adv">
      <summary>Advanced</summary>
      <div class="form-grid" style="margin-top:10px">
        <div class="form-item">
          <label>Docker/compose installer image ({{ form.online ? 'online bundles need no installer image' : 'must contain /offline/install.sh' }})</label>
          <input v-model.trim="form.installerImage" placeholder="registry.example.com/stable/docker:27.5.1-dind" :disabled="form.online" />
        </div>
        <div class="form-item">
          <label>Materials path inside the image</label>
          <input v-model.trim="form.installerPath" placeholder="/offline" :disabled="form.online" />
        </div>
      </div>
    </details>

    <div v-if="form.online" class="hint" style="margin:0 0 8px">
      Online bundle notes: the packing machine pulls nothing (no registry login, no installer image,
      no docker offline materials). At install time the target machine runs docker login and
      docker compose pull (registry credentials go to the install page under Advanced) and installs
      docker online when it is missing.
    </div>
    <button class="primary" :disabled="loading" @click="start">
      {{ loading ? 'Loading…' : 'Start pack' }}
    </button>
    <div v-if="hint" class="hint" style="margin-top:8px">{{ hint }}</div>
  </div>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api'
import PathPicker from './PathPicker.vue'
import { sessionState } from '../useSession'

const emit = defineEmits(['started'])

const form = sessionState('compose-installer.pack', {
  projectDir: '',
  product: '',
  env: '',
  version: '',
  profileSel: [],
  platform: '',
  online: false,
  installerImage: '',
  installerPath: '',
  output: '',
  registry: '',
  registryUser: '',
  registryPassword: ''
}, ['registryPassword']) // passwords are never persisted

// /api/project drives product / env / version / profile options.
const projectData = ref(null)
const projectError = ref('')
const productNames = ref([])
const versionOptions = ref(null) // null = this env ships no version manifest
const latestVersion = ref('')
const profileOptions = ref([])
const loading = ref(false)
const hint = ref('')

let loadSeq = 0
let debounceTimer = null

onMounted(() => loadProject())
onBeforeUnmount(() => clearTimeout(debounceTimer))

// PathPicker emits on every keystroke: debounce the /api/project refresh and
// drop responses that are already stale.
function queueLoadProject () {
  clearTimeout(debounceTimer)
  debounceTimer = setTimeout(loadProject, 400)
}

async function loadProject () {
  clearTimeout(debounceTimer)
  const seq = ++loadSeq
  loading.value = true
  projectError.value = ''
  try {
    const data = await api.project(form.projectDir)
    if (seq !== loadSeq) return
    projectData.value = data
    applyProduct()
  } catch (e) {
    if (seq !== loadSeq) return
    projectData.value = null
    productNames.value = []
    versionOptions.value = null
    profileOptions.value = []
    projectError.value = e.message
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

function productInfo () {
  const products = (projectData.value && projectData.value.products) || {}
  return products[form.product] || null
}

const productError = computed(() => (productInfo() || {}).error || '')
const envOptions = computed(() => {
  const info = productInfo()
  return (info && Array.isArray(info.envs) ? info.envs : []) || []
})

function applyProduct () {
  const data = projectData.value || {}
  const names = Object.keys(data.products || {})
  productNames.value = names
  // Keep a valid remembered selection; otherwise prefer the configured default
  // product, then the first one (a single product is still shown, preselected).
  if (!names.includes(form.product)) {
    const def = data.defaultProduct
    form.product = (def && names.includes(def)) ? def : (names[0] || '')
  }
  applyEnvDetails(false)
}

function onProductChange () { applyEnvDetails(false) }
function onEnvChange () { applyEnvDetails(true) }

// versions / latest / profiles all arrive in the same /api/project response:
// product and env switches recompute locally without extra requests.
function applyEnvDetails (reset) {
  const info = productInfo()
  const envs = (info && Array.isArray(info.envs) ? info.envs : []) || []
  if (!envs.includes(form.env)) form.env = envs[0] || ''

  const versions = info && info.versions ? info.versions[form.env] : null
  const latest = info && info.latest ? info.latest[form.env] : ''
  versionOptions.value = Array.isArray(versions) ? [...versions] : null
  latestVersion.value = latest || ''
  if (versionOptions.value) {
    if (reset || !versionOptions.value.includes(form.version)) {
      form.version = versionOptions.value.includes(latestVersion.value)
        ? latestVersion.value
        : (versionOptions.value[0] || '')
    }
  } else {
    // No version manifest for this env: the version field stays hidden.
    form.version = ''
  }

  profileOptions.value = ((info && info.profiles ? info.profiles[form.env] : []) || [])
  const valid = profileOptions.value
  // Drop remembered profile selections that no longer exist in this env.
  form.profileSel = (Array.isArray(form.profileSel) ? form.profileSel : []).filter(p => valid.includes(p))
}

async function start () {
  hint.value = ''
  if (!form.product) { hint.value = 'Select a product first'; return }
  if (!form.env) { hint.value = 'Select an environment first'; return }
  const body = {
    projectDir: form.projectDir,
    product: form.product,
    env: form.env,
    version: versionOptions.value ? form.version : '',
    profiles: Array.isArray(form.profileSel) ? [...form.profileSel] : [],
    installerImage: form.online ? '' : form.installerImage,
    installerPath: form.online ? '' : form.installerPath,
    output: form.output,
    registry: form.registry,
    registryUser: form.online ? '' : form.registryUser,
    registryPassword: form.online ? '' : form.registryPassword,
    online: !!form.online,
    platform: form.platform || ''
  }
  try {
    const { jobId } = await api.pack(body)
    hint.value = `Job submitted: ${jobId}`
    emit('started', jobId)
  } catch (e) {
    hint.value = `Submit failed: ${e.message}`
  }
}
</script>
