<script setup>
import { ref, computed } from 'vue'
import AppIcon from './AppIcon.vue'

// A settable exclusion list: the keywords a scan refuses to import. Entries are
// chips so each one can be removed on its own — the defaults are only defaults,
// and a user who wants NCED back deletes the word.
const props = defineProps({
  // The list being edited.
  modelValue: { type: Array, default: () => [] },
  // The list in force when this library follows the global setting.
  inherited: { type: Array, default: () => [] },
  // "inherit" | "custom". Omitted on the global form, which has nothing to
  // inherit from.
  mode: { type: String, default: '' },
})
const emit = defineEmits(['update:modelValue', 'update:mode'])

const draft = ref('')
const custom = computed(() => props.mode === 'custom')
const effective = computed(() => (props.mode ? (custom.value ? props.modelValue : props.inherited) : props.modelValue))

function add() {
  const word = draft.value.trim()
  if (!word) return
  const list = props.modelValue.slice()
  if (!list.includes(word)) list.push(word)
  draft.value = ''
  emit('update:modelValue', list)
  if (props.mode && !custom.value) emit('update:mode', 'custom')
}

// Typing a comma or pressing enter both end a word: the list is often pasted in.
function onKey(e) {
  if (e.key === 'Enter' || e.key === ',') {
    e.preventDefault()
    add()
  }
}

function remove(word) {
  emit('update:modelValue', props.modelValue.filter((w) => w !== word))
  if (props.mode && !custom.value) emit('update:mode', 'custom')
}

function setMode(next) {
  if (next === 'custom') {
    // Start from what the library is already doing, so switching to "custom"
    // does not silently change which files are imported.
    if (!props.modelValue.length) emit('update:modelValue', props.inherited.slice())
    emit('update:mode', 'custom')
  } else {
    emit('update:mode', 'inherit')
  }
}
</script>

<template>
  <div class="exclude-box">
    <div v-if="mode" class="segmented seg">
      <button :class="{ on: !custom }" @click="setMode('inherit')">跟随全局</button>
      <button :class="{ on: custom }" @click="setMode('custom')">自定义</button>
    </div>

    <div class="chips">
      <span v-for="w in effective" :key="w" class="chip on kw">
        {{ w }}
        <button
          v-if="!mode || custom" class="x" :title="`不再排除「${w}」`" @click="remove(w)"
        ><AppIcon name="close" :size="12" /></button>
      </span>
      <span v-if="!effective.length" class="tiny muted">没有排除项——扫描会导入所有找到的文件。</span>
    </div>

    <div v-if="!mode || custom" class="add-row">
      <input
        v-model="draft" placeholder="加一个关键词,回车确认" @keydown="onKey"
      />
      <button class="btn sm" :disabled="!draft.trim()" @click="add">
        <AppIcon name="plus" :size="14" /> 添加
      </button>
    </div>
    <div v-else class="tiny muted">这个库使用全局列表;要单独调整就切到「自定义」。</div>
    <div class="tiny muted">
      按文件名匹配,英文按整词(如 pv 命中 [PV1],不会命中 Spy);中文按包含。
    </div>
  </div>
</template>

<style scoped>
.exclude-box { display: flex; flex-direction: column; gap: 8px; }
.seg { align-self: flex-start; }
.chips { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.kw { display: inline-flex; align-items: center; gap: 4px; }
.kw .x {
  display: inline-flex; align-items: center; justify-content: center;
  border: 0; background: transparent; color: inherit; cursor: pointer;
  padding: 0; opacity: .55;
}
.kw .x:hover { opacity: 1; color: var(--danger, #e5484d); }
.add-row { display: flex; gap: 8px; }
.add-row input { flex: 1; }
.muted { opacity: .7; }
</style>
