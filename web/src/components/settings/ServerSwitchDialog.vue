<template>
  <Teleport to="body">
    <div class="dialog-overlay" @click.self="$emit('close')">
      <div class="dialog-panel">
        <!-- Header -->
        <div class="dialog-header">
          <button class="dialog-back" @click="$emit('close')">
            <ChevronLeft :size="20" />
          </button>
          <h2 class="dialog-title">{{ t('settings.serverSwitch.title') }}</h2>
          <button class="dialog-add-btn" @click="openAddForm" :title="t('settings.serverSwitch.add')">
            <Plus :size="18" />
          </button>
        </div>

        <!-- Server list -->
        <div class="dialog-body">
          <div v-if="!showForm">
            <div v-if="servers.length === 0" class="dialog-empty">
              <Server :size="32" class="dialog-empty-icon" />
              <p>{{ t('settings.serverSwitch.empty') }}</p>
            </div>
            <div
              v-for="s in servers"
              :key="s.id"
              class="server-row"
              @click="switchTo(s)"
            >
              <div class="server-dot" :style="{ background: s.tagColor || '#58a6ff' }"></div>
              <div class="server-info">
                <div class="server-name">
                  {{ s.name }}
                  <span v-if="s.isDefault" class="server-default-badge">{{ t('settings.serverSwitch.default') }}</span>
                </div>
                <div class="server-addr">{{ s.protocol }}://{{ s.host }}:{{ s.port }}</div>
              </div>
              <div class="server-actions">
                <button class="server-action-btn" @click.stop="openEditForm(s)">{{ t('settings.serverSwitch.edit') }}</button>
                <button class="server-action-btn delete" @click.stop="deleteServer(s.id)">{{ t('settings.serverSwitch.delete') }}</button>
              </div>
            </div>
          </div>

          <!-- Add/Edit form -->
          <div v-else class="server-form">
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.name') }}</label>
              <input v-model="form.name" type="text" :placeholder="t('settings.serverSwitch.namePlaceholder')" />
            </div>
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.protocol') }}</label>
              <div class="form-radio-group">
                <label><input type="radio" v-model="form.protocol" value="https" /> HTTPS</label>
                <label><input type="radio" v-model="form.protocol" value="http" /> HTTP</label>
              </div>
            </div>
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.host') }}</label>
              <input v-model="form.host" type="text" :placeholder="t('settings.serverSwitch.hostPlaceholder')" autocorrect="off" autocapitalize="none" spellcheck="false" />
            </div>
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.port') }}</label>
              <input v-model.number="form.port" type="number" />
            </div>
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.password') }}</label>
              <input v-model="form.password" type="password" />
            </div>
            <div class="form-field">
              <label>{{ t('settings.serverSwitch.color') }}</label>
              <div class="color-picker">
                <div
                  v-for="c in tagColors"
                  :key="c"
                  class="color-dot"
                  :class="{ selected: form.tagColor === c }"
                  :style="{ background: c }"
                  @click="form.tagColor = c"
                ></div>
              </div>
            </div>
            <div class="form-field">
              <label class="form-checkbox">
                <input type="checkbox" v-model="form.isDefault" />
                {{ t('settings.serverSwitch.setDefault') }}
              </label>
            </div>
            <div class="form-actions">
              <button class="btn-cancel" @click="showForm = false">{{ t('settings.serverSwitch.cancel') }}</button>
              <button class="btn-save" @click="saveServer">{{ t('settings.serverSwitch.save') }}</button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ChevronLeft, Plus, Server } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
const emit = defineEmits<{ close: []; switched: [url: string] }>()

const TAG_COLORS = ['#58a6ff', '#3fb950', '#d29922', '#f85149', '#bc8cff', '#8b949e']
const STORAGE_KEY = 'clawbench-settings-serverList'

interface ServerInfo {
  id: string
  name: string
  protocol: string
  host: string
  port: number
  password: string
  notes: string
  tagColor: string
  lastConnectedAt: number
  isDefault: boolean
}

const tagColors = TAG_COLORS
const servers = ref<ServerInfo[]>([])
const showForm = ref(false)
const editingId = ref<string | null>(null)
const form = ref<ServerInfo>(emptyForm())

function emptyForm(): ServerInfo {
  return { id: '', name: '', protocol: 'https', host: '', port: 20000, password: '', notes: '', tagColor: '#58a6ff', lastConnectedAt: 0, isDefault: false }
}

function loadServers() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    servers.value = raw ? JSON.parse(raw) : []
  } catch { servers.value = [] }
}

function saveServers() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(servers.value))
}

function openAddForm() {
  editingId.value = null
  form.value = emptyForm()
  showForm.value = true
}

function openEditForm(s: ServerInfo) {
  editingId.value = s.id
  form.value = { ...s }
  showForm.value = true
}

function saveServer() {
  if (!form.value.name.trim() || !form.value.host.trim()) return
  if (!form.value.id) form.value.id = crypto.randomUUID?.() || Math.random().toString(36).slice(2)

  if (form.value.isDefault) {
    servers.value.forEach(s => { s.isDefault = false })
  }

  const idx = servers.value.findIndex(s => s.id === form.value.id)
  if (idx >= 0) {
    servers.value[idx] = { ...form.value }
  } else {
    servers.value.push({ ...form.value })
  }
  saveServers()
  showForm.value = false
}

function deleteServer(id: string) {
  servers.value = servers.value.filter(s => s.id !== id)
  saveServers()
}

function switchTo(s: ServerInfo) {
  const url = `${s.protocol}://${s.host}:${s.port}`
  // Update lastConnectedAt
  const idx = servers.value.findIndex(sv => sv.id === s.id)
  if (idx >= 0) {
    servers.value[idx].lastConnectedAt = Date.now()
    saveServers()
  }
  // In Android app mode, use native bridge to switch within the app
  const an = (window as any).AndroidNative
  if (an && typeof an.connectToServer === 'function') {
    an.connectToServer(url, s.password || '')
    return
  }
  // Web mode: navigate directly
  window.location.href = url
}

onMounted(loadServers)
</script>

<style scoped>
.dialog-overlay {
  position: fixed; inset: 0; z-index: 999;
  background: rgba(0, 0, 0, 0.5);
  display: flex; align-items: center; justify-content: center;
  padding: 0 12px;
}

.dialog-panel {
  width: 100%; max-width: 420px; max-height: 85vh;
  background: var(--bg-primary); border: 1px solid var(--border-color);
  border-radius: 14px; display: flex; flex-direction: column; overflow: hidden;
}

.dialog-header {
  display: flex; align-items: center; justify-content: space-between;
  padding: 14px 16px; border-bottom: 1px solid var(--border-color); flex-shrink: 0;
}

.dialog-back {
  background: none; border: none; color: var(--accent-color);
  cursor: pointer; padding: 4px; display: flex; align-items: center;
}

.dialog-title { font-size: 16px; font-weight: 600; color: var(--text-primary); margin: 0; }

.dialog-add-btn {
  background: none; border: 1px solid var(--border-color); color: var(--accent-color);
  border-radius: 8px; padding: 6px; cursor: pointer; display: flex; align-items: center;
  transition: border-color 0.2s;
}
.dialog-add-btn:hover { border-color: var(--accent-color); }

.dialog-body { flex: 1; overflow-y: auto; padding: 0; }

.dialog-empty {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  padding: 48px 20px; color: var(--text-hint); gap: 12px;
}
.dialog-empty-icon { opacity: 0.3; }

.server-row {
  display: flex; align-items: center; padding: 12px 16px;
  border-bottom: 1px solid var(--border-color); gap: 10px; cursor: pointer;
  transition: background 0.15s;
}
.server-row:active { background: rgba(88, 166, 255, 0.08); }

.server-dot { width: 10px; height: 10px; border-radius: 50%; flex-shrink: 0; }

.server-info { flex: 1; min-width: 0; }
.server-name { font-size: 14px; font-weight: 500; color: var(--text-primary); display: flex; align-items: center; gap: 6px; }
.server-default-badge { font-size: 10px; color: var(--accent-color); background: rgba(88, 166, 255, 0.12); padding: 1px 6px; border-radius: 4px; }
.server-addr { font-size: 12px; color: var(--text-muted); margin-top: 2px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.server-actions { display: flex; gap: 6px; flex-shrink: 0; }
.server-action-btn {
  background: none; border: 1px solid var(--border-color); color: var(--text-muted);
  font-size: 12px; padding: 4px 8px; border-radius: 6px; cursor: pointer;
  transition: border-color 0.2s, color 0.2s;
}
.server-action-btn:hover { border-color: var(--accent-color); color: var(--accent-color); }
.server-action-btn.delete:hover { border-color: var(--color-red, #dc2626); color: var(--color-red, #dc2626); }

/* Form */
.server-form { padding: 16px; }
.form-field { margin-bottom: 14px; }
.form-field label { display: block; font-size: 12px; color: var(--text-muted); margin-bottom: 6px; }
.form-field input[type="text"], .form-field input[type="number"], .form-field input[type="password"] {
  width: 100%; padding: 10px 12px; border: 1px solid var(--border-color);
  border-radius: 8px; font-size: 14px; background: var(--bg-secondary);
  color: var(--text-primary); outline: none; transition: border-color 0.2s;
}
.form-field input:focus { border-color: var(--accent-color); }

.form-radio-group { display: flex; gap: 16px; }
.form-radio-group label { display: flex; align-items: center; gap: 6px; font-size: 14px; color: var(--text-primary); cursor: pointer; margin-bottom: 0; }

.color-picker { display: flex; gap: 10px; }
.color-dot {
  width: 28px; height: 28px; border-radius: 50%; border: 3px solid transparent;
  cursor: pointer; transition: border-color 0.2s, transform 0.15s; position: relative;
}
.color-dot:hover { transform: scale(1.15); }
.color-dot.selected { border-color: var(--text-primary); }
.color-dot.selected::after {
  content: '✓'; position: absolute; inset: 0; display: flex;
  align-items: center; justify-content: center; color: #fff; font-size: 13px; font-weight: 700;
}

.form-checkbox { display: flex; align-items: center; gap: 8px; font-size: 14px; color: var(--text-primary); cursor: pointer; margin-bottom: 0; }

.form-actions { display: flex; gap: 10px; margin-top: 20px; }
.form-actions button {
  flex: 1; height: 40px; border-radius: 10px; font-size: 14px;
  font-weight: 600; cursor: pointer; border: none; transition: background 0.2s;
}
.btn-save { background: var(--accent-color); color: #fff; }
.btn-save:hover { background: var(--accent-hover); }
.btn-cancel { background: var(--bg-secondary); color: var(--text-muted); border: 1px solid var(--border-color) !important; }
</style>
