/* 展示层的小工具：格式化时间、缩写 ID、把一次执行结论写成一行。
   放在这里而不是每个组件里各写一份，是因为两件事都有不显然的前提：
   短 ID 要看尾段（下面第一条），执行结论的写法要在时间线、重试链与结果区块里一致。 */
import type { ExecMeta, JobEvent } from './api/types'

/**
 * 任务 ID 是 UUIDv7：高 48 位是创建时的毫秒时间戳。
 * 于是一起提交的任务前几位完全相同（实测同一秒内的 6 条任务前 8 位一样），
 * 用前缀当"人眼可辨的短码"会在最需要区分它们的时候（批量操作回报）失败。
 * 尾段是随机位，所以取后 8 位。
 */
export function shortJobId(id: string): string {
  return id.length > 8 ? id.slice(-8) : id
}

/**
 * 一条任务的类型（跑什么）。
 *
 * 新写法直接给 `type`；旧写法的请求体没有 type，那时名称兼作类型，
 * 后端的 `Job.HandlerKey()` 也是这条回退规则（core/job.go），界面跟着它走。
 */
export function jobTypeOf(job: { name: string; type?: string }): string {
  return job.type?.trim() || job.name.trim()
}

function parsed(iso: string): Date | null {
  const value = new Date(iso)
  return Number.isNaN(value.getTime()) ? null : value
}

/** 列表用：日期 + 时间 */
export function formatDateTime(iso: string): string {
  const date = parsed(iso)
  return date ? date.toLocaleString() : iso
}

/** 事件流用：只看时分秒 */
export function formatClockTime(iso: string): string {
  const date = parsed(iso)
  return date ? date.toLocaleTimeString() : iso
}

/** 耗时的一行写法：秒以内给毫秒，往上给一位小数的秒 */
export function formatDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
}

/**
 * 一次执行的结论：http 档位看状态码，进程档位看退出码，被信号中止的看信号名。
 *
 * 三者取一而不是并列显示：时间线一行放不下三样，而"看哪个数"由 kind 决定，
 * 与后端 core.ExecMeta 的取值口径一致（http 档位不产生退出码）。
 */
export function execOutcome(meta: ExecMeta): string {
  if (meta.kind === 'http') return `HTTP ${meta.http_status ?? 0}`
  if (meta.signal) return meta.signal
  return `exit ${meta.exit_code ?? 0}`
}

/** 结论 + 耗时，时间线与重试链用的那一行 */
export function execSummary(meta: ExecMeta): string {
  return `${execOutcome(meta)} · ${formatDurationMs(meta.duration_ms)}`
}

/**
 * 从事件里取执行结论。
 *
 * 只有 job.completed / job.failed 带这个键（core 的 eventData 在有结论时才写），
 * 形状不认就当没有——时间线遇到新的 data 形状时不该整条不显示。
 */
export function eventExec(event: JobEvent): ExecMeta | null {
  const data = event.data as { result?: unknown } | null | undefined
  if (!data || typeof data.result !== 'object' || data.result === null) return null

  const meta = data.result as Partial<ExecMeta>
  return typeof meta.kind === 'string' ? (meta as ExecMeta) : null
}

/**
 * 输出正文进 <pre> 之前的处理：去掉不可见控制字符、把超长单行折断。
 *
 * 正文由脚本或对端产生，两件事都会发生：
 *   - 控制字符（含改终端状态的 ESC 序列）在页面上是隐形的，留着会让人以为输出比实际短；
 *   - 一条几百 KB 的单行（进度条、未换行的 JSON）会把卡片撑到屏幕外。
 * 折行处标注省略了多少字符，这样"被折掉"这件事本身是看得见的。
 */
export function sanitizeOutput(text: string, lineLimit = 400): string {
  // 控制字符按 \x00-\x1f 与 \x7f 去，留 \n（分段）与 \t（对齐）；\r 一律丢掉，
  // 否则 CRLF 的产物会在每行末尾留一个隐形字符，折行判断也会差一格
  const cleaned = text.replace(/\r/g, '').replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, '\uFFFD')

  return cleaned
    .split('\n')
    .map((line) => {
      if (line.length <= lineLimit) return line
      return `${line.slice(0, lineLimit)} …（本行还有 ${line.length - lineLimit} 个字符未显示）`
    })
    .join('\n')
}
