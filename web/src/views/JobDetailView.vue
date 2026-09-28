<script setup lang="ts">
import { useRoute } from 'vue-router'
import PageHeader from '../components/layout/PageHeader.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'

const route = useRoute()
</script>

<template>
  <div class="p-6">
    <PageHeader title="任务详情" :subtitle="`ID ${String(route.params.id ?? '')}`" />
    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        title="详情页将在 M4 落地"
        description="基本信息卡、payload 只读视图与运行时间线（GET /jobs/:id/events + WS append）属 M4"
      />
    </section>
  </div>
</template>
