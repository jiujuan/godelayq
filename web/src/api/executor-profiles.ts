/* 档位的在线管理端点（TASK-W06 的写口 + TASK-W08 补的读口）。
 *
 * 这一组端点要 executors.web_enabled=true 才存在：关闭的部署统一回 503，
 * 而且两种 503 的文案可区分（"没打开" vs "打开了却没装配依赖"），照原样显示给运维就行。
 * 身份门槛是 ops——比建任务与删组都高，因为它改的是"这台机器能执行什么"（设计文档 D10）。
 * 提交 exec.* 任务的门槛是另一件事（executors.required_role），两条判定互不替代。
 */
import { request } from './client'
import type {
  ExecutorProfileDeleteResult,
  ExecutorProfileRecord,
  ExecutorProfileRecordResponse,
  ExecutorProfile,
} from './types'

const BASE = '/executors/profiles'

/**
 * 读一条档位文件的记录（定义）。
 *
 * 配置侧的档位没有存储记录，这里会拿到 409（与 PUT/DELETE 同一条判据），
 * 因此只对 GET /executors 里 `editable` 那一种行调用它。
 * 返回体里的 env 只有键名（env_keys）：取值不外露，编辑完也别把它拼回去。
 */
export function getExecutorProfileRecord(name: string): Promise<ExecutorProfileRecordResponse> {
  return request<ExecutorProfileRecordResponse>(`${BASE}/${encodeURIComponent(name)}`)
}

/** 新建档位。探测失败不阻止保存：201 的响应里 runtime_ok 可能是 false，reason 要照原样显示 */
export function createExecutorProfile(body: ExecutorProfileRecord): Promise<ExecutorProfile> {
  return request<ExecutorProfile>(BASE, { method: 'POST', body })
}

/**
 * 修改档位。kind / script / program 三条改不了（400，文案点名"删了重建"）；
 * 体内不写 name 时取路径上那个，写了就必须与路径一致。
 * env 键整个不带 = 保持原值不变（表单就是这么做的）。
 */
export function updateExecutorProfile(
  name: string,
  body: ExecutorProfileRecord,
): Promise<ExecutorProfile> {
  return request<ExecutorProfile>(`${BASE}/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body,
  })
}

/**
 * 删除档位。默认 jobs=pause：该类型的待执行任务被钉住，正在执行的一条都不中止。
 * block 是"还有任务就别删"的显式语义，非空时后端回 409 并给出条数。
 */
export function deleteExecutorProfile(
  name: string,
  strategy: 'pause' | 'block' = 'pause',
): Promise<ExecutorProfileDeleteResult> {
  return request<ExecutorProfileDeleteResult>(`${BASE}/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    query: { jobs: strategy },
  })
}
