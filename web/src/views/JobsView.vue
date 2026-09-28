<script setup lang="ts">
import PageHeader from '../components/layout/PageHeader.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
</script>

<template>
  <div class="p-6">
    <PageHeader title="任务" subtitle="列表、筛选与新建/编辑任务表单" />
    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        title="任务列表将在 M4 落地"
        description="M3 只交付骨架与实时链路；表格、筛选条、批量操作条与 JobForm 属 M4"
      />
    </section>
  </div>
</template>
