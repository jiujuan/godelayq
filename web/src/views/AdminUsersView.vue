<script setup lang="ts">
/* 账号管理（§13）：admin 档在线增改删控制台账号。
 *
 * 后端契约在 handlers_users.go：写后缓存即时失效，改动立即生效，不再需要
 * "改配置 + 重启"。两个保护都在服务端（最后一个 admin 不可删/降、不能删自己），
 * 前端做的是把后果说在前面：删除确认预置后果文案，自己的删除按钮直接置灰。
 *
 * 未启用管理库（mgrstore.enabled=false）时列表 503，那是部署选择而不是故障，
 * 与台账页同一待遇：给引导说明，不弹错误横幅。 */
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { Plus } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import UiInput from '../components/ui/UiInput.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import { createUser, deleteUser, listUsers, updateUser } from '../api/admin'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import { formatDateTime } from '../display'
import type { RoleName, UserView } from '../api/types'

const ROLE_OPTIONS: { value: RoleName; label: string }[] = [
  { value: 'viewer', label: 'viewer（只读）' },
  { value: 'operator', label: 'operator（读写任务）' },
  { value: 'admin', label: 'admin（含账号管理）' },
  { value: 'ops', label: 'ops（含运维）' },
]

const auth = useAuthStore()
const toast = useToastStore()
const queryClient = useQueryClient()

const usersQuery = useQuery({ queryKey: queryKeys.users, queryFn: listUsers })
const users = computed(() => usersQuery.data.value ?? [])

/** 未启用账号存储的 503：部署选择，给引导不给报错横幅（与台账页同款判定） */
const notEnabled = computed(() => {
  const err = usersQuery.error.value
  return err instanceof ApiError && err.status === 503
})

const busy = ref(false)
const formError = ref<string | null>(null)
/** null = 表单关闭；'' = 新建；其他 = 编辑该账号 */
const editing = ref<string | null>(null)

const draftName = ref('')
const draftPassword = ref('')
const draftRole = ref<RoleName>('viewer')

const pendingDelete = ref<UserView | null>(null)

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return fallback
}

function refresh(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.users })
}

function startCreate(): void {
  editing.value = ''
  formError.value = null
  draftName.value = ''
  draftPassword.value = ''
  draftRole.value = 'viewer'
}

function startEdit(user: UserView): void {
  editing.value = user.name
  formError.value = null
  draftName.value = user.name
  draftPassword.value = ''
  draftRole.value = user.role
}

function closeForm(): void {
  editing.value = null
  formError.value = null
}

async function save(): Promise<void> {
  busy.value = true
  formError.value = null
  try {
    if (editing.value === '') {
      const created = await createUser({
        name: draftName.value,
        password: draftPassword.value,
        role: draftRole.value,
      })
      toast.success(`账号 ${created.name} 已创建`)
    } else {
      // 指针语义：密码留空 = 不改密码，只提交角色
      const updated = await updateUser(draftName.value, {
        role: draftRole.value,
        ...(draftPassword.value ? { password: draftPassword.value } : {}),
      })
      toast.success(`账号 ${updated.name} 已更新`)
    }
    closeForm()
    refresh()
  } catch (err) {
    formError.value = describeError(err, '保存失败')
  } finally {
    busy.value = false
  }
}

async function confirmDelete(): Promise<void> {
  const target = pendingDelete.value
  pendingDelete.value = null
  if (!target) return

  busy.value = true
  try {
    await deleteUser(target.name)
    toast.success(`账号 ${target.name} 已删除`)
    refresh()
  } catch (err) {
    toast.error(describeError(err, '删除失败'))
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="账号" subtitle="控制台账号在线管理；改动即时生效，无需改配置重启">
      <template #actions>
        <UiButton size="sm" variant="ghost" @click="startCreate">
          <Plus :size="14" aria-hidden="true" />
          新建账号
        </UiButton>
      </template>
    </PageHeader>

    <UiEmptyState
      v-if="notEnabled"
      title="未启用账号存储"
      description="mgrstore.enabled=false 的部署里账号仍在配置文件里声明（server.auth.users），改动需重启进程。打开管理库后即可在这里在线增改删。"
    />
    <template v-else>
      <p
        v-if="usersQuery.error.value"
        role="alert"
        class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
          bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
      >
        账号列表读取失败：{{ describeError(usersQuery.error.value, '未知错误') }}
      </p>

      <!-- 新建/编辑表单：一张卡片两种模式，名字在编辑态锁定（账号名是身份键） -->
      <section
        v-if="editing !== null"
        class="mb-4 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4"
      >
        <h2 class="mb-4 text-sm font-semibold">{{ editing === '' ? '新建账号' : `编辑 ${draftName}` }}</h2>

        <p
          v-if="formError"
          role="alert"
          class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
            bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
        >
          {{ formError }}
        </p>

        <form class="grid gap-4 md:grid-cols-3" @submit.prevent="save">
          <UiInput
            v-model="draftName"
            label="账号名"
            :disabled="editing !== '' || busy"
            hint="登录名，创建后不可改"
          />
          <UiInput
            v-model="draftPassword"
            label="密码"
            type="password"
            autocomplete="new-password"
            :placeholder="editing === '' ? '' : '留空表示不修改密码'"
            :hint="editing === '' ? '服务端只存 bcrypt 哈希' : undefined"
          />
          <UiSelect v-model="draftRole" label="角色" :options="ROLE_OPTIONS" />

          <div class="flex items-center gap-2 md:col-span-3">
            <UiButton type="submit" size="sm" :loading="busy">
              {{ editing === '' ? '创建' : '保存' }}
            </UiButton>
            <UiButton type="button" size="sm" variant="ghost" :disabled="busy" @click="closeForm">
              取消
            </UiButton>
          </div>
        </form>
      </section>

      <section class="overflow-hidden rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <div class="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2.5">
          <h2 class="text-sm font-semibold">全部账号</h2>
          <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ users.length }}</span>
        </div>

        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-[var(--color-border)] text-left text-xs uppercase tracking-wide text-[var(--color-text-muted)]">
              <th class="px-4 py-2 font-medium">账号</th>
              <th class="px-4 py-2 font-medium">角色</th>
              <th class="px-4 py-2 font-medium">创建时间</th>
              <th class="px-4 py-2 font-medium">更新时间</th>
              <th class="px-4 py-2 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--color-border)]">
            <tr v-for="user in users" :key="user.name" class="hover:bg-[var(--color-surface)]">
              <td class="px-4 py-2.5 font-medium">
                {{ user.name }}
                <span v-if="user.name === auth.user?.name" class="ml-1.5 text-xs text-[var(--color-text-muted)]">（我）</span>
              </td>
              <td class="px-4 py-2.5">
                <UiBadge tone="neutral">{{ user.role }}</UiBadge>
              </td>
              <td class="px-4 py-2.5 tabular-nums text-[var(--color-text-muted)]">{{ formatDateTime(user.created_at) }}</td>
              <td class="px-4 py-2.5 tabular-nums text-[var(--color-text-muted)]">{{ formatDateTime(user.updated_at) }}</td>
              <td class="px-4 py-2.5 text-right">
                <UiButton size="sm" variant="ghost" :disabled="busy" @click="startEdit(user)">编辑</UiButton>
                <UiButton
                  size="sm"
                  variant="ghost"
                  :disabled="busy || user.name === auth.user?.name"
                  :title="user.name === auth.user?.name ? '不能删除自己' : undefined"
                  @click="pendingDelete = user"
                >
                  删除
                </UiButton>
              </td>
            </tr>
          </tbody>
        </table>

        <UiEmptyState
          v-if="users.length === 0"
          title="还没有账号"
          description="用上方「新建账号」创建第一个；管理库启用时配置文件里的账号已一次性导入。"
        />
      </section>
    </template>

    <UiConfirm
      :open="pendingDelete !== null"
      :title="`删除账号 ${pendingDelete?.name ?? ''}？`"
      confirm-label="删除账号"
      tone="danger"
      :busy="busy"
      @cancel="pendingDelete = null"
      @confirm="confirmDelete"
    >
      删除后该账号立即无法登录，已签发的令牌在下次校验时失效。
      <em class="font-medium text-[var(--color-text)]">库里必须始终留有至少一个 admin</em>：
      删掉最后一个 admin、或删除自己，都会被服务端拒绝。
    </UiConfirm>
  </div>
</template>
