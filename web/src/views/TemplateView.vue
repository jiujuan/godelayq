<script setup lang="ts">
/* 任务模板页：把 src/content/job-template.md 渲染成页面。
 *
 * markdown 只有一份，下载按钮发出去的 Blob 与页面同源，不存在"文档改了页面没改"。
 * 之所以走 ?raw 打进产物而不是放 web/public 让浏览器直接取文件：内嵌形态下
 * serveConsole 只把 /assets/* 当真实文件，其余路径一律回落 index.html
 * （api/console.go:65-68），根目录的 .md 在单二进制里根本请求不到。
 */
import { computed } from 'vue'
import MarkdownIt from 'markdown-it'
import { Download } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiButton from '../components/ui/UiButton.vue'
import templateSource from '../content/job-template.md?raw'

// html:false：文档里的原生 HTML 一律转义。渲染自家仓库的文件也不给注入留口子。
const md = new MarkdownIt({ html: false, linkify: true })
const rendered = computed(() => md.render(templateSource))

function download(): void {
  const blob = new Blob([templateSource], { type: 'text/markdown;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = 'godelayq-新建任务模板.md'
  link.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="任务模板" subtitle="新建任务每一项的写法、可直接改用的示例与报错对照">
      <template #actions>
        <UiButton variant="outline" size="sm" @click="download">
          <Download :size="14" aria-hidden="true" />
          下载模板（.md）
        </UiButton>
      </template>
    </PageHeader>

    <article
      class="doc rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white px-6 py-5"
      v-html="rendered"
    />
  </div>
</template>

<style scoped>
/* markdown 输出的排版：没有引 typography 插件，只为这份文档要用的元素写。 */
.doc :deep(h1) {
  margin: 0 0 0.75rem;
  font-size: 1.375rem;
  font-weight: 600;
}
.doc :deep(h2) {
  margin: 2rem 0 0.75rem;
  padding-top: 1rem;
  border-top: 1px solid var(--color-border);
  font-size: 1.0625rem;
  font-weight: 600;
}
.doc :deep(h3) {
  margin: 1.5rem 0 0.5rem;
  font-size: 0.9375rem;
  font-weight: 600;
}
.doc :deep(p) {
  margin: 0.5rem 0;
  font-size: 0.875rem;
  line-height: 1.7;
}
.doc :deep(ul),
.doc :deep(ol) {
  margin: 0.5rem 0;
  padding-left: 1.25rem;
  font-size: 0.875rem;
  line-height: 1.7;
}
.doc :deep(li) {
  margin: 0.25rem 0;
}
.doc :deep(a) {
  color: var(--color-primary);
  text-decoration: underline;
}
.doc :deep(blockquote) {
  margin: 0.75rem 0;
  padding: 0.5rem 0.75rem;
  border-left: 3px solid var(--color-primary);
  background: var(--color-surface);
  font-size: 0.875rem;
}
.doc :deep(code) {
  padding: 0.1rem 0.3rem;
  border-radius: var(--radius-control);
  background: var(--color-surface);
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.8125rem;
}
.doc :deep(pre) {
  margin: 0.75rem 0;
  padding: 0.75rem;
  overflow-x: auto;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-card);
  background: var(--color-surface);
}
.doc :deep(pre code) {
  padding: 0;
  background: none;
  line-height: 1.6;
}
.doc :deep(table) {
  width: 100%;
  margin: 0.75rem 0;
  border-collapse: collapse;
  font-size: 0.8125rem;
}
.doc :deep(th),
.doc :deep(td) {
  padding: 0.4rem 0.6rem;
  border: 1px solid var(--color-border);
  text-align: left;
}
.doc :deep(th) {
  background: var(--color-surface);
  font-weight: 600;
}
.doc :deep(hr) {
  margin: 1.5rem 0;
  border: none;
  border-top: 1px solid var(--color-border);
}
</style>
