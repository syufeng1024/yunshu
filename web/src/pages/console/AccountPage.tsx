import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { bindEmail, sendBindEmailCode } from '@/api/auth'
import { api, errorHint, errorMessage } from '@/api/client'
import { Badge, Button, Input, PageHeader } from '@/components'
import { ROUTES, USER_ROLE_LABEL, USER_STATUS_LABEL } from '@/lib/constants'
import { usePageTitle } from '@/lib/usePageTitle'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'

/** 与服务端 maskIdentifier 同一条规则（保留首字符 + 域名），只在本地展示用。 */
function maskEmail(email: string): string {
  const at = email.indexOf('@')
  if (at <= 0) return '***'
  const local = email.slice(0, at)
  return (local.length <= 1 ? '*' : `${local.slice(0, 1)}***`) + email.slice(at)
}

/**
 * 账户设置：账号信息、登录邮箱、修改密码、注销。
 *
 * 这三件事以前在控制台里没有任何入口——后端接口（/api/v1/auth/password、
 * /api/v1/auth/cancel）早就有了，只是没有页面，用户改密码只能去翻文档。
 */
export default function AccountPage() {
  usePageTitle('控制台 · 账户设置')
  const user = useAuthStore((state) => state.user)
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()

  const [nickname, setNickname] = useState(user?.display_name ?? '')
  const [username, setUsername] = useState(user?.username ?? '')
  // 只有用户真的改过用户名才把它放进请求体：这个字段的三态语义是
  // 缺省=不改、空串=清空、有值=设置，未改动时发送会变成一次误清空。
  const [usernameDirty, setUsernameDirty] = useState(false)
  const [profileSaving, setProfileSaving] = useState(false)
  const [profileError, setProfileError] = useState<string | null>(null)
  const [profileSaved, setProfileSaved] = useState(false)

  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [pwError, setPwError] = useState<string | null>(null)
  const [pwHint, setPwHint] = useState<string | null>(null)
  const [pwSubmitting, setPwSubmitting] = useState(false)

  // 登录邮箱绑定：未绑定 → 输入 → 验码 → 已绑定。三态在同一个卡片里切换。
  const [bindEmailValue, setBindEmailValue] = useState('')
  const [bindCode, setBindCode] = useState('')
  const [bindStep, setBindStep] = useState<'idle' | 'code'>('idle')
  const [bindSending, setBindSending] = useState(false)
  const [bindSubmitting, setBindSubmitting] = useState(false)
  const [bindError, setBindError] = useState<string | null>(null)
  const [bindHint, setBindHint] = useState<string | null>(null)
  const [bindCooldown, setBindCooldown] = useState(0)

  const [cancelOpen, setCancelOpen] = useState(false)
  const [cancelConfirm, setCancelConfirm] = useState('')
  const [cancelError, setCancelError] = useState<string | null>(null)
  const [cancelHint, setCancelHint] = useState<string | null>(null)
  const [cancelSubmitting, setCancelSubmitting] = useState(false)

  useEffect(() => {
    if (bindCooldown <= 0) return
    const timer = window.setInterval(() => {
      setBindCooldown((left) => (left <= 1 ? 0 : left - 1))
    }, 1000)
    return () => window.clearInterval(timer)
  }, [bindCooldown])

  const handleSendBindCode = async () => {
    setBindError(null)
    setBindHint(null)
    const value = bindEmailValue.trim()
    if (!value.includes('@')) {
      setBindError('请输入完整的登录邮箱。')
      return
    }
    setBindSending(true)
    try {
      const response = await sendBindEmailCode(value)
      setBindStep('code')
      setBindCooldown(response.resend_after > 0 ? response.resend_after : 60)
    } catch (error) {
      setBindError(errorMessage(error))
      setBindHint(errorHint(error))
    } finally {
      setBindSending(false)
    }
  }

  const handleBindEmail = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setBindError(null)
    setBindHint(null)
    if (!/^\d{6}$/.test(bindCode.trim())) {
      setBindError('验证码是 6 位数字，请检查是否输完整。')
      return
    }
    setBindSubmitting(true)
    try {
      const data = await bindEmail({ email: bindEmailValue.trim(), code: bindCode.trim() })
      // 绑定不改变会话，但当前用户视图里的 email 变了：不写回 store 的话，
      // 侧边栏与注销确认框会继续用旧值（注销要输入完整邮箱）。
      const previous = useAuthStore.getState().user
      if (previous) {
        useAuthStore.setState({ user: { ...previous, ...data.user } })
      }
      setBindStep('idle')
      setBindCode('')
      setBindCooldown(0)
    } catch (error) {
      setBindError(errorMessage(error))
      setBindHint(errorHint(error))
    } finally {
      setBindSubmitting(false)
    }
  }

  const saveProfile = async () => {
    setProfileSaving(true)
    setProfileError(null)
    setProfileSaved(false)
    try {
      const body: { nickname: string; username?: string } = { nickname: nickname.trim() }
      if (usernameDirty) body.username = username.trim()
      const data = await api.patch<{ user: CurrentUser }>('/api/v1/auth/profile', body)
      // 直接写回 store：否则侧边栏与页头仍是旧名字，看起来像没保存成功。
      useAuthStore.setState({ user: data.user })
      setUsername(data.user.username ?? '')
      setUsernameDirty(false)
      setProfileSaved(true)
    } catch (error) {
      setProfileError(errorMessage(error))
    } finally {
      setProfileSaving(false)
    }
  }

  const handlePassword = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setPwError(null)
    setPwHint(null)
    if (!oldPassword) {
      setPwError('请输入当前密码。')
      return
    }
    // 与服务端 validatePassword 一致（≥ 8 字节），不在这里放行再让服务端拒。
    if (newPassword.length < 8) {
      setPwError('新密码至少 8 位。')
      return
    }
    if (newPassword !== confirmPassword) {
      setPwError('两次输入的新密码不一致。')
      return
    }
    setPwSubmitting(true)
    try {
      await api.post<void>('/api/v1/auth/password', {
        old_password: oldPassword,
        new_password: newPassword,
      })
      // 服务端改密会删除该账号的全部会话（含当前这一个），所以这里不是「可选
      // 的顺手登出」：不跳走，下一个请求也会 401 被踹出去。
      await logout()
      navigate(`${ROUTES.login}?reason=password-changed`, { replace: true })
    } catch (error) {
      setPwError(errorMessage(error))
      setPwHint(errorHint(error))
    } finally {
      setPwSubmitting(false)
    }
  }

  const handleCancel = async () => {
    setCancelError(null)
    const typed = cancelConfirm.trim().toLowerCase()
    if (typed !== (user?.email ?? '').toLowerCase()) {
      setCancelError('请输入完整的登录邮箱以确认。')
      return
    }
    setCancelSubmitting(true)
    try {
      await api.post<void>('/api/v1/auth/cancel')
      await logout()
      navigate(ROUTES.home, { replace: true })
    } catch (error) {
      setCancelError(errorMessage(error))
      setCancelHint(errorHint(error))
    } finally {
      setCancelSubmitting(false)
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader title="账户设置" description="账号信息、登录密码与账号注销。" />

      <section className="rounded-xl border border-border bg-surface p-5">
        <h2 className="text-sm font-semibold text-ink">账号信息</h2>
        <dl className="mt-4 grid gap-4 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-xs text-ink-muted">登录邮箱</dt>
            <dd className="mt-1 text-ink">{user?.email ?? '—'}</dd>
          </div>
          <div className="sm:col-span-2 space-y-4">
            <div>
              <dt className="text-xs text-ink-muted">显示名称</dt>
              <dd className="mt-2">
                <Input
                  aria-label="显示名称"
                  value={nickname}
                  onChange={(event) => {
                    setNickname(event.target.value)
                    setProfileSaved(false)
                  }}
                  placeholder="留空则显示用户名或登录邮箱"
                />
              </dd>
            </div>
            <div>
              <dt className="text-xs text-ink-muted">登录用户名（可选）</dt>
              <dd className="mt-2">
                <Input
                  aria-label="登录用户名"
                  value={username}
                  onChange={(event) => {
                    setUsername(event.target.value)
                    setUsernameDirty(true)
                    setProfileSaved(false)
                  }}
                  placeholder="留空则只能用邮箱 / 手机号登录"
                  autoComplete="username"
                />
              </dd>
              <p className="mt-1 text-xs text-ink-muted">
                3–32 位，字母开头，可用小写字母、数字、点、下划线、短横线。设置后可用它登录，
                也可以随时清空。
              </p>
            </div>
            <div className="flex items-center gap-3">
              <Button
                type="button"
                variant="secondary"
                onClick={() => void saveProfile()}
                loading={profileSaving}
              >
                保存资料
              </Button>
              {profileSaved ? (
                <span role="status" className="text-xs text-success">
                  已保存
                </span>
              ) : null}
            </div>
            {profileError ? (
              <p role="alert" className="text-xs text-danger">
                {profileError}
              </p>
            ) : null}
          </div>
          <div>
            <dt className="text-xs text-ink-muted">账号角色</dt>
            <dd className="mt-1">
              <Badge variant="info">{user ? USER_ROLE_LABEL[user.role] : '—'}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-xs text-ink-muted">账号状态</dt>
            <dd className="mt-1">
              <Badge variant={user?.status === 'NORMAL' ? 'success' : 'warning'}>
                {user ? USER_STATUS_LABEL[user.status] : '—'}
              </Badge>
            </dd>
          </div>
        </dl>
        <p className="mt-4 text-xs text-ink-muted">
          登录邮箱在未绑定时可自助绑定；已绑定的更换需提交工单。账号角色不可自助修改。
          用户名可随时设置或清空。
        </p>
      </section>

      <section className="rounded-xl border border-border bg-surface p-5">
        <h2 className="text-sm font-semibold text-ink">登录邮箱</h2>
        {user?.email ? (
          <div className="mt-3 space-y-1 text-sm">
            <p className="text-ink">{maskEmail(user.email)}</p>
            <p className="text-xs text-ink-muted">
              已绑定。更换登录邮箱请联系客服提交工单——换绑等于更换账号接管主通道，
              需要额外的身份核验。
            </p>
          </div>
        ) : (
          <>
            <p className="mt-1 text-xs text-ink-secondary">
              当前账号没有登录邮箱。绑一个你自己的邮箱，忘记密码时就能自助重置，
              不必再联系管理员。
            </p>
            {bindStep === 'idle' ? (
              <div className="mt-4 max-w-md space-y-3">
                <Input
                  label="登录邮箱"
                  type="email"
                  value={bindEmailValue}
                  onChange={(event) => setBindEmailValue(event.target.value)}
                  autoComplete="email"
                  placeholder="you@example.com"
                />
                {bindError ? (
                  <div role="alert" className="rounded-lg border border-danger bg-danger-soft p-3 text-xs">
                    <p className="font-medium text-danger">{bindError}</p>
                    {bindHint ? <p className="mt-1 text-ink-secondary">{bindHint}</p> : null}
                  </div>
                ) : null}
                <Button
                  type="button"
                  variant="secondary"
                  loading={bindSending}
                  onClick={() => {
                    void handleSendBindCode()
                  }}
                >
                  发送验证码
                </Button>
              </div>
            ) : (
              <form className="mt-4 max-w-md space-y-3" onSubmit={handleBindEmail} noValidate>
                <p role="status" className="rounded-lg border border-info bg-info-soft p-3 text-xs text-ink-secondary">
                  验证码已发送到 {bindEmailValue.trim()}，10 分钟内有效。
                </p>
                <div className="flex items-end gap-2">
                  <Input
                    label="验证码"
                    value={bindCode}
                    onChange={(event) => setBindCode(event.target.value)}
                    autoComplete="one-time-code"
                    inputMode="numeric"
                  />
                  <Button
                    variant="secondary"
                    size="md"
                    type="button"
                    className="shrink-0 whitespace-nowrap"
                    loading={bindSending}
                    disabled={bindCooldown > 0}
                    onClick={() => {
                      void handleSendBindCode()
                    }}
                  >
                    {bindCooldown > 0 ? `${bindCooldown}s 后重发` : '重新获取'}
                  </Button>
                </div>
                {bindError ? (
                  <div role="alert" className="rounded-lg border border-danger bg-danger-soft p-3 text-xs">
                    <p className="font-medium text-danger">{bindError}</p>
                    {bindHint ? <p className="mt-1 text-ink-secondary">{bindHint}</p> : null}
                  </div>
                ) : null}
                <Button type="submit" loading={bindSubmitting}>
                  确认绑定
                </Button>
              </form>
            )}
          </>
        )}
      </section>

      <section className="rounded-xl border border-border bg-surface p-5">
        <h2 className="text-sm font-semibold text-ink">修改密码</h2>
        <p className="mt-1 text-xs text-ink-secondary">
          修改成功后，所有设备上的登录状态都会失效，需要用新密码重新登录。
        </p>
        <form className="mt-4 max-w-md space-y-4" onSubmit={handlePassword} noValidate>
          <Input
            label="当前密码"
            type="password"
            value={oldPassword}
            onChange={(event) => setOldPassword(event.target.value)}
            autoComplete="current-password"
            required
          />
          <Input
            label="新密码"
            type="password"
            value={newPassword}
            onChange={(event) => setNewPassword(event.target.value)}
            autoComplete="new-password"
            placeholder="至少 8 位"
            required
          />
          <Input
            label="确认新密码"
            type="password"
            value={confirmPassword}
            onChange={(event) => setConfirmPassword(event.target.value)}
            autoComplete="new-password"
            placeholder="再次输入新密码"
            required
          />
          {pwError ? (
            <div role="alert" className="rounded-lg border border-danger bg-danger-soft p-3 text-xs">
              <p className="font-medium text-danger">{pwError}</p>
              {pwHint ? <p className="mt-1 text-ink-secondary">{pwHint}</p> : null}
            </div>
          ) : null}
          <Button type="submit" loading={pwSubmitting}>
            更新密码
          </Button>
        </form>
      </section>

      <section className="rounded-xl border border-danger bg-danger-soft/40 p-5">
        <h2 className="text-sm font-semibold text-danger">注销账号</h2>
        <p className="mt-1 text-xs text-ink-secondary">
          注销后账号将停用、无法登录，余额与密钥同时失效。存在未结工单时不允许注销。
        </p>
        {cancelOpen ? (
          <div className="mt-4 max-w-md space-y-3">
            <Input
              label="输入登录邮箱以确认"
              value={cancelConfirm}
              onChange={(event) => setCancelConfirm(event.target.value)}
              placeholder={user?.email ?? ''}
              autoComplete="off"
            />
            {cancelError ? (
              <div role="alert" className="rounded-lg border border-danger bg-surface p-3 text-xs">
                <p className="font-medium text-danger">{cancelError}</p>
                {cancelHint ? <p className="mt-1 text-ink-secondary">{cancelHint}</p> : null}
              </div>
            ) : null}
            <div className="flex gap-2">
              <Button variant="danger" onClick={() => void handleCancel()} loading={cancelSubmitting}>
                确认注销
              </Button>
              <Button
                variant="secondary"
                onClick={() => {
                  setCancelOpen(false)
                  setCancelConfirm('')
                  setCancelError(null)
                  setCancelHint(null)
                }}
              >
                取消
              </Button>
            </div>
          </div>
        ) : (
          <Button variant="secondary" className="mt-4" onClick={() => setCancelOpen(true)}>
            我要注销账号
          </Button>
        )}
      </section>
    </div>
  )
}
