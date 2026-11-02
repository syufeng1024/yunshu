import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import {
  useChangeUserRole,
  useCreateOperatorUser,
  useDisableUser,
  useEnableUser,
  useSubmitBalanceAdjust,
  useUser,
  useUsers,
} from '@/api/admin'
import type {
  AdminUser,
  AdminUserListParams,
  AdminUserRole,
  AdminUserStatus,
  BalanceAdjustDirection,
  BalanceAdjustState,
  ProvisionableRole,
} from '@/api/admin'
import {
  Badge,
  Button,
  Drawer,
  ErrorState,
  Input,
  Modal,
  PageHeader,
  Select,
  Table,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import { AuditTrail } from '@/components/admin'
import { ConfirmDialog } from '@/components/console'
import { formatCreditAmount, formatDateTime, formatInteger } from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'
import { useAuthStore } from '@/stores/auth'

const PAGE_SIZE = 20

type StatusFilter = 'ALL' | AdminUserStatus
type SearchField = 'AUTO' | 'email' | 'phone' | 'user_no'
type DetectedField = Exclude<SearchField, 'AUTO'>

const ROLE_LABEL: Record<string, string> = {
  PERSONAL: '个人用户',
  OPS: '运营',
  FINANCE: '财务',
  SUPER_ADMIN: '超级管理员',
}

/**
 * 新建运营账号可选的角色。刻意不含 PERSONAL：客户账号必须走公开注册，
 * 那条路径带体验积分、设备指纹与协议同意；后端同样会以 400 拒绝 PERSONAL。
 */
const PROVISIONABLE_ROLE_OPTIONS: { value: ProvisionableRole; label: string }[] = [
  { value: 'OPS', label: ROLE_LABEL.OPS },
  { value: 'FINANCE', label: ROLE_LABEL.FINANCE },
  { value: 'SUPER_ADMIN', label: ROLE_LABEL.SUPER_ADMIN },
]

/** 变更角色可选的角色：客户与运营可以互转，所以这里是完整枚举。 */
const ASSIGNABLE_ROLE_OPTIONS: { value: AdminUserRole; label: string }[] = [
  { value: 'PERSONAL', label: ROLE_LABEL.PERSONAL },
  { value: 'OPS', label: ROLE_LABEL.OPS },
  { value: 'FINANCE', label: ROLE_LABEL.FINANCE },
  { value: 'SUPER_ADMIN', label: ROLE_LABEL.SUPER_ADMIN },
]

/** 密码按 UTF-8 字节计：后端限制 8..72 字节（bcrypt 在 72 字节处截断）。 */
function passwordByteLength(raw: string): number {
  return new TextEncoder().encode(raw).length
}

const STATUS_LABEL: Record<string, string> = {
  NORMAL: '正常',
  DISABLED: '已禁用',
  FROZEN_OVERDRAFT: '透支冻结',
}

const SEARCH_FIELD_LABEL: Record<DetectedField, string> = {
  email: '邮箱',
  phone: '手机号',
  user_no: '用户号',
}

const BALANCE_DIRECTION_LABEL: Record<BalanceAdjustDirection, string> = {
  INCREASE: '增加',
  DECREASE: '减少',
}

const BALANCE_STATE_LABEL: Record<BalanceAdjustState, string> = {
  PENDING: '待审核',
  APPROVED: '已通过',
  REJECTED: '已驳回',
}

function parseBalanceAmount(raw: string): number | null {
  const normalized = normalizeDigits(raw)
    .replace(/[，,\s]/g, '')
  if (!/^\d+$/.test(normalized)) return null
  const value = Number(normalized)
  if (!Number.isSafeInteger(value) || value <= 0) return null
  return value
}

function normalizeDigits(raw: string): string {
  return raw.replace(/[０-９]/g, (char) => String.fromCharCode(char.charCodeAt(0) - 0xfee0))
}

/**
 * users.go has no `q` parameter: the endpoint only performs independent LIKE
 * matches on email / phone / user_no. The automatic rule is shown in the UI so
 * an operator can see which column will be queried, and the select can
 * override it.
 */
export function detectUserSearchField(raw: string): DetectedField {
  const value = normalizeDigits(raw).trim()
  if (value.includes('@')) return 'email'
  if (/^\d{11}$/.test(value)) return 'phone'
  return 'user_no'
}

function statusVariant(status: AdminUserStatus): 'success' | 'danger' | 'warning' {
  if (status === 'NORMAL') return 'success'
  if (status === 'DISABLED') return 'danger'
  return 'warning'
}

function DetailRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-3 border-b border-border py-1.5 text-xs last:border-0">
      <span className="text-ink-muted">{label}</span>
      <span className="text-right text-ink">{value}</span>
    </div>
  )
}

export default function AdminUsersPage() {
  usePageTitle('用户管理')
  const toast = useToast()

  const [search, setSearch] = useState('')
  const [searchField, setSearchField] = useState<SearchField>('AUTO')
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('ALL')
  const [page, setPage] = useState(1)
  const debouncedSearch = useDebouncedValue(search, 300)
  const effectiveField: DetectedField =
    searchField === 'AUTO' ? detectUserSearchField(debouncedSearch) : searchField

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, searchField, statusFilter])

  const queryParams: AdminUserListParams = {
    page,
    pageSize: PAGE_SIZE,
    status: statusFilter === 'ALL' ? undefined : statusFilter,
    sort: 'created_at',
    order: 'desc',
  }
  const searchValue = normalizeDigits(debouncedSearch).trim()
  if (searchValue) {
    if (effectiveField === 'email') queryParams.email = searchValue
    else if (effectiveField === 'phone') queryParams.phone = searchValue
    else queryParams.userNo = searchValue
  }

  const list = useUsers(queryParams)

  const [detailId, setDetailId] = useState<number | null>(null)
  const detail = useUser(detailId)

  const disableUser = useDisableUser()
  const enableUser = useEnableUser()

  // 开通运营账号与变更角色都挂 admin.all（只有超级管理员持有）。这里的显隐只是
  // 体验层：两条接口在服务端已经拦住，其他角色即使构造请求也是 403。
  const currentRole = useAuthStore((state) => state.user?.role)
  const canProvisionRole = currentRole === 'SUPER_ADMIN'

  const createOperator = useCreateOperatorUser()
  const [createOpen, setCreateOpen] = useState(false)
  const [createIdentifier, setCreateIdentifier] = useState('')
  const [createPassword, setCreatePassword] = useState('')
  const [createNickname, setCreateNickname] = useState('')
  const [createRole, setCreateRole] = useState<ProvisionableRole>('OPS')
  const [createIdentifierError, setCreateIdentifierError] = useState<string | null>(null)
  const [createPasswordError, setCreatePasswordError] = useState<string | null>(null)
  const [createError, setCreateError] = useState<string | null>(null)

  const changeRole = useChangeUserRole()
  const [roleDraft, setRoleDraft] = useState<AdminUserRole>('PERSONAL')
  const [roleReason, setRoleReason] = useState('')
  const [roleReasonError, setRoleReasonError] = useState<string | null>(null)
  const [roleError, setRoleError] = useState<string | null>(null)

  const detailRole = detail.data?.user.role
  useEffect(() => {
    if (!detailRole) return
    // 抽屉里的下拉以服务端的当前角色为准：角色变更成功后会重新拉详情，这里随之
    // 复位，避免下拉还停在旧值上再提交一次。
    setRoleDraft(detailRole as AdminUserRole)
    setRoleReason('')
    setRoleReasonError(null)
    setRoleError(null)
  }, [detailRole])

  const [disableTarget, setDisableTarget] = useState<AdminUser | null>(null)
  const [disableReason, setDisableReason] = useState('')
  const [disableReasonError, setDisableReasonError] = useState<string | null>(null)
  const [disableError, setDisableError] = useState<string | null>(null)

  const [enableTarget, setEnableTarget] = useState<AdminUser | null>(null)
  const [enableError, setEnableError] = useState<string | null>(null)

  const submitBalance = useSubmitBalanceAdjust()
  const [balanceTarget, setBalanceTarget] = useState<AdminUser | null>(null)
  const [balanceDirection, setBalanceDirection] =
    useState<BalanceAdjustDirection>('INCREASE')
  const [balanceAmount, setBalanceAmount] = useState('')
  const [balanceReason, setBalanceReason] = useState('')
  const [balanceAmountError, setBalanceAmountError] = useState<string | null>(null)
  const [balanceReasonError, setBalanceReasonError] = useState<string | null>(null)
  const [balanceError, setBalanceError] = useState<string | null>(null)

  const [auditOpen, setAuditOpen] = useState(false)
  const [auditTarget, setAuditTarget] = useState<{ type?: string; id?: string }>({})
  const [auditRefreshKey, setAuditRefreshKey] = useState(0)

  const openAudit = (targetType?: string, targetId?: number | string) => {
    setAuditTarget({
      type: targetType,
      id: targetId === undefined ? undefined : String(targetId),
    })
    setAuditRefreshKey((value) => value + 1)
    setAuditOpen(true)
  }

  const openDisable = (user: AdminUser) => {
    setDisableTarget(user)
    setDisableReason('')
    setDisableReasonError(null)
    setDisableError(null)
  }

  const handleDisable = async () => {
    if (!disableTarget) return
    const reason = disableReason.trim()
    const length = [...reason].length
    if (length < 5) {
      setDisableReasonError('禁用原因至少 5 个字。')
      return
    }
    if (length > 255) {
      setDisableReasonError('禁用原因最多 255 个字符。')
      return
    }
    setDisableReasonError(null)
    setDisableError(null)
    try {
      await disableUser.mutateAsync({ id: disableTarget.id, input: { reason } })
      toast.success('用户已禁用')
      openAudit('t_user', disableTarget.id)
      setDisableTarget(null)
    } catch (error) {
      const hint = errorHint(error)
      setDisableError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const handleEnable = async () => {
    if (!enableTarget) return
    setEnableError(null)
    try {
      await enableUser.mutateAsync(enableTarget.id)
      toast.success('用户已启用')
      openAudit('t_user', enableTarget.id)
      setEnableTarget(null)
    } catch (error) {
      const hint = errorHint(error)
      setEnableError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const openBalanceAdjust = (user: AdminUser) => {
    setBalanceTarget(user)
    setBalanceDirection('INCREASE')
    setBalanceAmount('')
    setBalanceReason('')
    setBalanceAmountError(null)
    setBalanceReasonError(null)
    setBalanceError(null)
  }

  const handleBalanceAdjust = async () => {
    if (!balanceTarget) return
    const amount = parseBalanceAmount(balanceAmount)
    const reason = balanceReason.trim()
    const reasonLength = [...reason].length
    let invalid = false
    setBalanceAmountError(null)
    setBalanceReasonError(null)
    setBalanceError(null)
    if (amount === null) {
      setBalanceAmountError('金额需为大于 0 的整数微积分；超过后端单张上限会被拒绝。')
      invalid = true
    }
    if (reasonLength < 5) {
      setBalanceReasonError('调整理由至少 5 个字。')
      invalid = true
    } else if (reasonLength > 255) {
      setBalanceReasonError('调整理由最多 255 个字符。')
      invalid = true
    }
    if (invalid || amount === null) return
    try {
      const response = await submitBalance.mutateAsync({
        id: balanceTarget.id,
        input: { direction: balanceDirection, amount_micro: amount, reason },
      })
      toast.success('已提交审核', '需由其他审核人批准后才会落账。')
      openAudit('t_balance_adjust', response.balance_adjust.id)
      setBalanceTarget(null)
    } catch (error) {
      const hint = errorHint(error)
      setBalanceError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const openCreate = () => {
    setCreateOpen(true)
    setCreateIdentifier('')
    setCreatePassword('')
    setCreateNickname('')
    setCreateRole('OPS')
    setCreateIdentifierError(null)
    setCreatePasswordError(null)
    setCreateError(null)
  }

  const handleCreateOperator = async () => {
    const identifier = createIdentifier.trim()
    const bytes = passwordByteLength(createPassword)
    let invalid = false
    setCreateIdentifierError(null)
    setCreatePasswordError(null)
    setCreateError(null)
    if (!identifier) {
      setCreateIdentifierError('请填写邮箱或手机号。')
      invalid = true
    }
    if (bytes < 8 || bytes > 72) {
      setCreatePasswordError('初始密码需为 8~72 字节（一个汉字约占 3 字节）。')
      invalid = true
    }
    if (invalid) return
    try {
      const created = await createOperator.mutateAsync({
        identifier,
        password: createPassword,
        nickname: createNickname.trim() || undefined,
        role: createRole,
      })
      toast.success(
        '运营账号已创建',
        `${created.user.user_no} 已开通为${ROLE_LABEL[created.user.role] ?? created.user.role}，初始密码请通过安全渠道转交。`,
      )
      setCreateOpen(false)
    } catch (error) {
      const hint = errorHint(error)
      setCreateError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const handleChangeRole = async () => {
    if (!detailUser) return
    const reason = roleReason.trim()
    const reasonLength = [...reason].length
    setRoleReasonError(null)
    setRoleError(null)
    if (roleDraft === detailUser.role) {
      setRoleError('所选角色与当前角色相同，无需变更。')
      return
    }
    if (reasonLength === 0) {
      setRoleReasonError('请填写变更理由，会写入审计。')
      return
    }
    if (reasonLength > 255) {
      setRoleReasonError('变更理由最多 255 个字符。')
      return
    }
    try {
      const response = await changeRole.mutateAsync({
        id: detailUser.id,
        input: { role: roleDraft, reason },
      })
      toast.success(
        '角色已变更',
        `${ROLE_LABEL[detailUser.role] ?? detailUser.role} → ${ROLE_LABEL[response.user.role] ?? response.user.role}`,
      )
    } catch (error) {
      const hint = errorHint(error)
      setRoleError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const listError = list.isError ? errorMessage(list.error) : null
  const listHint = list.isError ? errorHint(list.error) : null
  const searchHint = searchValue
    ? `自动判定：含 @ → 邮箱；纯 11 位数字 → 手机号；其余 → 用户号。当前按${SEARCH_FIELD_LABEL[effectiveField]}查询。`
    : '自动判定：含 @ → 邮箱；纯 11 位数字 → 手机号；其余 → 用户号。也可手动指定查询字段。'

  const columns: TableColumn<AdminUser>[] = [
    {
      key: 'user',
      header: '用户',
      render: (user) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{user.nickname || '—'}</p>
          <code className="text-xs text-ink-secondary">{user.user_no}</code>
        </div>
      ),
    },
    {
      key: 'contact',
      header: '联系方式',
      render: (user) => (
        <div className="space-y-0.5 text-xs text-ink-secondary">
          <p>{user.email || '—'}</p>
          <p>{user.phone || '—'}</p>
        </div>
      ),
    },
    {
      key: 'role',
      header: '角色',
      render: (user) => <span className="text-xs">{ROLE_LABEL[user.role] ?? user.role}</span>,
    },
    {
      key: 'status',
      header: '状态',
      render: (user) => (
        <Badge variant={statusVariant(user.status)} dot>
          {STATUS_LABEL[user.status] ?? user.status}
        </Badge>
      ),
    },
    {
      key: 'plan',
      header: '套餐',
      render: (user) =>
        user.plan_name || user.plan_code ? (
          <div className="text-xs text-ink-secondary">
            <p>{user.plan_name || '—'}</p>
            <p className="text-ink-muted">{user.plan_code}</p>
          </div>
        ) : (
          <span className="text-xs text-ink-muted">—</span>
        ),
    },
    {
      key: 'risk',
      header: '风险分',
      align: 'right',
      render: (user) => <span className="tabular-nums">{formatInteger(user.risk_score)}</span>,
    },
    {
      key: 'created_at',
      header: '注册时间',
      render: (user) => (
        <span className="text-xs text-ink-secondary">{formatDateTime(user.created_at)}</span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (user) => (
        <div className="flex flex-wrap items-center justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => setDetailId(user.id)}>
            详情
          </Button>
          {user.status === 'NORMAL' ? (
            <Button variant="ghost" size="sm" className="text-danger" onClick={() => openDisable(user)}>
              禁用
            </Button>
          ) : null}
          {user.status === 'DISABLED' ? (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setEnableError(null)
                setEnableTarget(user)
              }}
            >
              启用
            </Button>
          ) : null}
          {user.status === 'FROZEN_OVERDRAFT' ? (
            <span className="text-xs text-ink-muted">透支冻结：充值后自动恢复</span>
          ) : null}
        </div>
      ),
    },
  ]

  const detailUser = detail.data?.user
  const balance = detail.data?.balance

  return (
    <div className="space-y-5">
      <PageHeader
        title="用户管理"
        description="账号状态、套餐与余额展示；余额调整提交后进入审核队列，由他人批准后落账。运营与财务账号由超级管理员在此开通，角色变更同样留痕。"
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {canProvisionRole ? (
              <Button variant="secondary" onClick={openCreate}>
                新建运营账号
              </Button>
            ) : null}
            <Button variant="secondary" onClick={() => openAudit()}>
              审计记录
            </Button>
          </div>
        }
      />

      <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_12rem_12rem]">
        <Input
          label="搜索用户"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="邮箱 / 手机号 / 用户号"
          hint={searchHint}
        />
        <Select
          label="搜索字段"
          value={searchField}
          onChange={(event) => setSearchField(event.target.value as SearchField)}
          options={[
            { value: 'AUTO', label: '自动判定' },
            { value: 'email', label: '邮箱' },
            { value: 'phone', label: '手机号' },
            { value: 'user_no', label: '用户号' },
          ]}
        />
        <Select
          label="状态"
          value={statusFilter}
          onChange={(event) => setStatusFilter(event.target.value as StatusFilter)}
          options={[
            { value: 'ALL', label: '全部状态' },
            { value: 'NORMAL', label: STATUS_LABEL.NORMAL },
            { value: 'DISABLED', label: STATUS_LABEL.DISABLED },
            { value: 'FROZEN_OVERDRAFT', label: STATUS_LABEL.FROZEN_OVERDRAFT },
          ]}
        />
      </div>

      {listHint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {listHint}
        </p>
      ) : null}

      <Table
        ariaLabel="用户列表"
        columns={columns}
        rows={list.data?.items ?? []}
        rowKey={(user) => String(user.id)}
        loading={list.isLoading}
        error={listError}
        onRetry={() => void list.refetch()}
        emptyTitle="暂无用户"
        emptyDescription={
          searchValue || statusFilter !== 'ALL'
            ? '当前搜索与筛选组合没有匹配的用户。'
            : '后端当前没有用户数据。'
        }
        pagination={
          list.data
            ? {
                page: list.data.page,
                pageSize: list.data.page_size,
                total: list.data.total,
                onPageChange: setPage,
              }
            : undefined
        }
      />

      <Drawer
        open={detailId !== null}
        onClose={() => setDetailId(null)}
        title="用户详情"
        description="余额与状态展示；余额调整需另一位审核人批准，Key 数为实时聚合值。"
        size="lg"
      >
        {detail.isLoading ? (
          <p className="text-sm text-ink-secondary">正在读取用户详情…</p>
        ) : detail.isError ? (
          <ErrorState
            message={errorMessage(detail.error)}
            hint={errorHint(detail.error)}
            onRetry={() => void detail.refetch()}
          />
        ) : detailUser ? (
          <div className="space-y-5">
            <section className="space-y-1">
              <h3 className="text-sm font-semibold text-ink">资料</h3>
              <DetailRow label="用户号" value={<code>{detailUser.user_no}</code>} />
              <DetailRow label="昵称" value={detailUser.nickname || '—'} />
              <DetailRow label="邮箱" value={detailUser.email || '—'} />
              <DetailRow label="手机号" value={detailUser.phone || '—'} />
              <DetailRow label="角色" value={ROLE_LABEL[detailUser.role] ?? detailUser.role} />
              <DetailRow
                label="状态"
                value={
                  <Badge variant={statusVariant(detailUser.status)}>
                    {STATUS_LABEL[detailUser.status] ?? detailUser.status}
                  </Badge>
                }
              />
              <DetailRow
                label="套餐"
                value={
                  detailUser.plan_name || detailUser.plan_code
                    ? `${detailUser.plan_name || '—'}（${detailUser.plan_code}）`
                    : '—'
                }
              />
              <DetailRow label="风险分" value={formatInteger(detailUser.risk_score)} />
              {detailUser.disable_source ? (
                <DetailRow label="禁用来源" value={detailUser.disable_source} />
              ) : null}
              {detailUser.disable_reason ? (
                <DetailRow label="禁用原因" value={detailUser.disable_reason} />
              ) : null}
              {detailUser.disabled_at ? (
                <DetailRow
                  label="禁用时间"
                  value={formatDateTime(detailUser.disabled_at)}
                />
              ) : null}
              {detailUser.disabled_by !== null && detailUser.disabled_by !== undefined ? (
                <DetailRow label="禁用操作人" value={`#${detailUser.disabled_by}`} />
              ) : null}
            </section>

            {canProvisionRole ? (
              <section className="space-y-2">
                <h3 className="text-sm font-semibold text-ink">变更角色</h3>
                <p className="text-xs text-ink-muted">
                  角色变更立即生效并写入审计（前后角色与状态，不含密码）。不能修改自己的角色；
                  最后一个超级管理员不能降级，否则将无人能进入运营后台。
                </p>
                <Select
                  label="目标角色"
                  value={roleDraft}
                  onChange={(event) => {
                    setRoleDraft(event.target.value as AdminUserRole)
                    setRoleError(null)
                  }}
                  options={ASSIGNABLE_ROLE_OPTIONS}
                />
                <Input
                  label="变更理由"
                  required
                  value={roleReason}
                  error={roleReasonError ?? undefined}
                  hint="必填，最多 255 个字符；会写入审计。"
                  onChange={(event) => {
                    setRoleReason(event.target.value)
                    setRoleReasonError(null)
                  }}
                />
                {roleError ? (
                  <p role="alert" className="text-xs text-danger">
                    {roleError}
                  </p>
                ) : null}
                <Button
                  variant="secondary"
                  size="sm"
                  loading={changeRole.isPending}
                  onClick={() => void handleChangeRole()}
                >
                  提交角色变更
                </Button>
              </section>
            ) : null}

            <section className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-semibold text-ink">余额</h3>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => openBalanceAdjust(detailUser)}
                >
                  调整余额
                </Button>
              </div>
              {balance && !balance.exists ? (
                <p className="text-xs text-ink-muted">该用户尚无积分账户，以下数值为零值。</p>
              ) : null}
              <DetailRow
                label="总余额"
                value={`${formatCreditAmount(balance?.balance_micro ?? 0)} 积分`}
              />
              <DetailRow
                label="充值余额"
                value={`${formatCreditAmount(balance?.balance_recharge_micro ?? 0)} 积分`}
              />
              <DetailRow
                label="赠送余额"
                value={`${formatCreditAmount(balance?.balance_bonus_micro ?? 0)} 积分`}
              />
              <DetailRow
                label="冻结退款"
                value={`${formatCreditAmount(balance?.frozen_refund_micro ?? 0)} 积分`}
              />
              <DetailRow
                label="透支额度"
                value={`${formatCreditAmount(balance?.overdraft_limit_micro ?? 0)} 积分`}
              />
            </section>

            <section className="space-y-1">
              <h3 className="text-sm font-semibold text-ink">最近余额调整</h3>
              {detail.data?.latest_balance_adjust ? (
                <>
                  <DetailRow
                    label="状态"
                    value={
                      <Badge
                        variant={
                          detail.data.latest_balance_adjust.state === 'APPROVED'
                            ? 'success'
                            : detail.data.latest_balance_adjust.state === 'REJECTED'
                              ? 'danger'
                              : 'warning'
                        }
                      >
                        {BALANCE_STATE_LABEL[detail.data.latest_balance_adjust.state]}
                      </Badge>
                    }
                  />
                  <DetailRow
                    label="方向"
                    value={
                      BALANCE_DIRECTION_LABEL[detail.data.latest_balance_adjust.direction]
                    }
                  />
                  <DetailRow
                    label="金额"
                    value={`${formatCreditAmount(detail.data.latest_balance_adjust.amount_micro)} 积分`}
                  />
                  <DetailRow label="理由" value={detail.data.latest_balance_adjust.reason} />
                  <DetailRow
                    label="提交时间"
                    value={formatDateTime(detail.data.latest_balance_adjust.created_at)}
                  />
                  {detail.data.latest_balance_adjust.decided_at ? (
                    <DetailRow
                      label="审批时间"
                      value={formatDateTime(detail.data.latest_balance_adjust.decided_at)}
                    />
                  ) : null}
                </>
              ) : (
                <p className="text-xs text-ink-muted">该用户暂无余额调整记录。</p>
              )}
            </section>

            <section className="space-y-1">
              <h3 className="text-sm font-semibold text-ink">Key 数</h3>
              <p className="text-sm text-ink">
                {formatInteger(detailUser.key_count)} 个活跃 Key
              </p>
              <p className="text-xs text-ink-muted">
                仅统计 status=ENABLED 且未删除的 Key。
              </p>
            </section>
          </div>
        ) : (
          <ErrorState message="用户详情为空" />
        )}
      </Drawer>

      <ConfirmDialog
        open={disableTarget !== null}
        title="禁用用户"
        description={
          disableTarget ? (
            <div className="space-y-3">
              <p>
                禁用后「{disableTarget.nickname || disableTarget.user_no}」将无法登录；原因会写入审计。
              </p>
              <Input
                label="禁用原因"
                required
                value={disableReason}
                error={disableReasonError ?? undefined}
                hint="至少 5 个字，最多 255 个字符。"
                onChange={(event) => {
                  setDisableReason(event.target.value)
                  setDisableReasonError(null)
                }}
              />
            </div>
          ) : undefined
        }
        confirmLabel="确认禁用"
        loading={disableUser.isPending}
        error={disableError}
        onConfirm={() => void handleDisable()}
        onCancel={() => {
          if (disableUser.isPending) return
          setDisableTarget(null)
          setDisableReasonError(null)
          setDisableError(null)
        }}
      />

      <ConfirmDialog
        open={enableTarget !== null}
        title="启用用户"
        description={
          enableTarget
            ? `启用后「${enableTarget.nickname || enableTarget.user_no}」可重新登录。透支冻结账号需充值后由账务自动恢复，运营启用无效。确认启用？`
            : undefined
        }
        confirmLabel="确认启用"
        loading={enableUser.isPending}
        error={enableError}
        onConfirm={() => void handleEnable()}
        onCancel={() => {
          if (enableUser.isPending) return
          setEnableTarget(null)
          setEnableError(null)
        }}
      />

      <ConfirmDialog
        open={balanceTarget !== null}
        title="提交余额调整"
        description={
          balanceTarget ? (
            <div className="space-y-3">
              <p>
                调整「{balanceTarget.nickname || balanceTarget.user_no}」的余额会进入审核队列；
                必须由另一位审核人批准后才会产生账本分录。
              </p>
              <Select
                label="方向"
                value={balanceDirection}
                onChange={(event) =>
                  setBalanceDirection(event.target.value as BalanceAdjustDirection)
                }
                options={[
                  { value: 'INCREASE', label: '增加余额' },
                  { value: 'DECREASE', label: '减少余额' },
                ]}
              />
              <Input
                label="金额（微积分）"
                required
                inputMode="numeric"
                value={balanceAmount}
                error={balanceAmountError ?? undefined}
                hint="必须大于 0；单张上限由后端校验，超限会返回 400。"
                onChange={(event) => {
                  setBalanceAmount(event.target.value)
                  setBalanceAmountError(null)
                }}
              />
              <Input
                label="调整理由"
                required
                value={balanceReason}
                error={balanceReasonError ?? undefined}
                hint="至少 5 个字；会写入审核队列与审计。"
                onChange={(event) => {
                  setBalanceReason(event.target.value)
                  setBalanceReasonError(null)
                }}
              />
            </div>
          ) : undefined
        }
        confirmLabel="提交审核"
        loading={submitBalance.isPending}
        error={balanceError}
        onConfirm={() => void handleBalanceAdjust()}
        onCancel={() => {
          if (submitBalance.isPending) return
          setBalanceTarget(null)
          setBalanceAmountError(null)
          setBalanceReasonError(null)
          setBalanceError(null)
        }}
      />

      <Modal
        open={createOpen}
        onClose={() => {
          if (createOperator.isPending) return
          setCreateOpen(false)
        }}
        title="新建运营账号"
        description="由超级管理员开通运营、财务或超级管理员账号。客户账号请让用户自行注册（试用额度、设备指纹与协议同意只在注册流程里采集）。"
        footer={
          <>
            <Button
              variant="ghost"
              onClick={() => setCreateOpen(false)}
              disabled={createOperator.isPending}
            >
              取消
            </Button>
            <Button
              loading={createOperator.isPending}
              onClick={() => void handleCreateOperator()}
            >
              创建账号
            </Button>
          </>
        }
      >
        <div className="space-y-3">
          <Input
            label="账号（邮箱 / 手机号）"
            required
            value={createIdentifier}
            error={createIdentifierError ?? undefined}
            hint="邮箱会被转为小写；手机号会去掉空格、括号与 + 号。"
            onChange={(event) => {
              setCreateIdentifier(event.target.value)
              setCreateIdentifierError(null)
            }}
          />
          <Input
            label="初始密码"
            required
            type="password"
            autoComplete="new-password"
            value={createPassword}
            error={createPasswordError ?? undefined}
            hint="8~72 字节；只用于创建这一次请求，不会回显、不会写入审计或日志。"
            onChange={(event) => {
              setCreatePassword(event.target.value)
              setCreatePasswordError(null)
            }}
          />
          <Input
            label="昵称"
            value={createNickname}
            hint="可留空；最多 64 个字符。"
            onChange={(event) => setCreateNickname(event.target.value)}
          />
          <Select
            label="角色"
            value={createRole}
            onChange={(event) => setCreateRole(event.target.value as ProvisionableRole)}
            options={PROVISIONABLE_ROLE_OPTIONS}
          />
          {createError ? (
            <p role="alert" className="text-xs text-danger">
              {createError}
            </p>
          ) : null}
        </div>
      </Modal>

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module="USER"
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}
