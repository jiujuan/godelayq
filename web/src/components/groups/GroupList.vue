<script setup lang="ts">
/* 左侧组列表：颜色点 + 名称 + 挂载数。
   未注册（registered=false）的条目要看得见——改名改到一半失败会留下"组没了、
   任务还挂着它"的半成品，藏起来就无处下手。 */
import UiBadge from '../ui/UiBadge.vue'
import type { Group } from '../../api/types'

const props = defineProps<{ groups: Group[]; selected: string }>()
const emit = defineEmits<{ select: [name: string] }>()

function keyOf(name: string): string {
  return name.toLowerCase()
}

function dot(color: string): Record<string, string> {
  // 没配色的组用描边空圈，而不是挑一个默认色假装它有身份
  return color ? { backgroundColor: color } : { border: '1px solid var(--color-border)' }
}

function isSelected(group: Group): boolean {
  return keyOf(props.selected) === keyOf(group.name)
}
</script>

<template>
  <ul class="flex flex-col">
    <li v-for="group in groups" :key="group.name">
      <button
        type="button"
        class="flex w-full items-center gap-2.5 border-l-2 px-4 py-2.5 text-left text-sm transition-colors"
        :class="isSelected(group)
          ? 'border-l-[var(--color-primary)] bg-[var(--color-primary-soft)]'
          : 'border-l-transparent hover:bg-[var(--color-surface)]'"
        @click="emit('select', group.name)"
      >
        <span class="h-2.5 w-2.5 shrink-0 rounded-full" :style="dot(group.color ?? '')"></span>

        <span class="min-w-0 flex-1 truncate font-medium">{{ group.name }}</span>

        <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ group.job_count }}</span>
        <UiBadge v-if="!group.registered" tone="warning">未注册</UiBadge>
      </button>
    </li>

    <li v-if="groups.length === 0" class="px-4 py-6 text-sm text-[var(--color-text-muted)]">
      注册表里还没有分组。
    </li>
  </ul>
</template>
