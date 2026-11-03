import { useState } from 'react'
import type { FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, errorHint, errorMessage } from '@/api/client'
import { Badge, Button, Input, PageHeader } from '@/components'
import { ROUTES, USER_ROLE_LABEL, USER_STATUS_LABEL } from '@/lib/constants'
import { usePageTitle } from '@/lib/usePageTitle'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'

/**
 * 账户设置：账号信息、修改密码、注销。
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
  const [profileSaving, setProfileSaving] = useState(false)
  const [profileError, setProfileError] = useState<string | null>(null)
  const [profileSaved, setProfileSaved] = useState(false)

  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [pwError, setPwError] = useState<string | null>(null)
  const [pwHint, setPwHint] = useState<string | null>(null)
  const [pwSubmitting, setPwSubmitting] = useState(false)

  const [cancelOpen, setCancelOpen] = useState(false)
  const [cancelConfirm, setCancelConfirm] = useState('')
  const [cancelError, setCancelError] = useState<string | null>(null)
  const [cancelHint, setCancelHint] = useState<string | null>(null)
  const [cancelSubmitting, setCancelSubmitting] = useState(false)

  const saveNickname = async () => {
    setProfileSaving(true)
    setProfileError(null)
    setProfileSaved(false)
    try {
      const data = await api.patch<{ user: CurrentUser }>('/api/v1/auth/profile', {
        nickname: nickname.trim(),
      })
      // 直接写回 store：否则侧边栏与页头仍是旧名字，看起来像没保存成功。
      useAuthStore.setState({ user: data.user })
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
          <div className="sm:col-span-2">
            <dt className="text-xs text-ink-muted">显示名称</dt>
            <dd className="mt-2 flex flex-col gap-2 sm:flex-row sm:items-center">
              <div className="sm:max-w-xs sm:flex-1">
                <Input
                  aria-label="显示名称"
                  value={nickname}
                  onChange={(event) => {
                    setNickname(event.target.value)
                    setProfileSaved(false)
                  }}
                  placeholder="留空则显示登录邮箱"
                />
              </div>
              <Button
                type="button"
                variant="secondary"
                onClick={() => void saveNickname()}
                loading={profileSaving}
              >
                保存昵称
              </Button>
            </dd>
            {profileError ? (
              <p role="alert" className="mt-1 text-xs text-danger">
                {profileError}
              </p>
            ) : null}
            {profileSaved ? (
              <p role="status" className="mt-1 text-xs text-success">
                昵称已保存
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
          登录邮箱与账号角色不可自助修改；需要变更请提交工单。
        </p>
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
