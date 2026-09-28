<script setup lang="ts">
import PageHeader from '../components/layout/PageHeader.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
</script>

<template>
  <div class="p-6">
    <PageHeader title="分组" subtitle="分组注册表：名称、描述、颜色与挂载的任务数" />
    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        title="分组管理将在 M4 落地"
        description="左侧组列表 + 右侧编辑区、删除确认弹窗（detach 后果说明）属 M4；后端端点已就绪"
      />
    </section>
  </div>
</template>
