<script setup lang="ts">
import PageHeader from '../components/layout/PageHeader.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
</script>

<template>
  <div class="p-6">
    <PageHeader title="运维" subtitle="调度总开关、运行时诊断、清空事件缓冲（仅 ops）" />
    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        title="运维面板将在 M4 落地"
        description="GET /admin/runtime 诊断、suspend/unsuspend 二次确认与 Topbar 挂起横幅属 M4；只有 ops 角色能进这一页"
      />
    </section>
  </div>
</template>
