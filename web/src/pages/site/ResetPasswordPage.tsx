import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { resetPassword, sendResetCode } from '@/api/auth'
import { errorHint, errorMessage } from '@/api/client'
import { Button, Input } from '@/components'
import { ROUTES } from '@/lib/constants'
import { usePageTitle } from '@/lib/usePageTitle'
import { useAuthStore } from '@/stores/auth'

type ResetStep = 'identify' | 'verify' | 'done'

// The server accepts exactly six digits. Checking here keeps an incomplete code
// from being sent at all: every rejected attempt counts against the five-try
// budget and burns the code, so a typo would cost the user a whole new e-mail.
const VERIFICATION_CODE_RE = /^\d{6}$/
const CODE_FORMAT_HINT = '验证码是 6 位数字，请检查是否输完整。'

/**
 * 短信渠道是否已开通。
 *
 * 这是服务端 `AUTH_SMS_MODE` 的**部署期镜像**，不是运行时真相（SPA 读不到服务端
 * 环境变量，`GET /auth/capabilities` 还没做）。两个变量必须一起翻；翻错的表现是
 * 「页面上说支持短信、提交后 400」。P2-1 上线后删掉它。
 */
const SMS_ENABLED = import.meta.env.VITE_AUTH_SMS_ENABLED === 'true'

/** 与服务端 hint 同文案：本地拦截时用户看到的句子与 400 时一模一样。 */
const SMS_UNAVAILABLE_HINT =
  '短信验证码暂未开放，请使用邮箱；若账号未绑定邮箱，请登录后在「账户设置」绑定'

/** 成功后停留多久再跳登录页：够读完一句提示，又不至于让人以为页面卡住。 */
const DONE_REDIRECT_MS = 1500

/**
 * 找回密码。三步在同一个页面里切换：输入标识 → 验证码 + 新密码 → 成功。
 *
 * 页面**不加 RequireAuth**：已登录但忘了旧密码是真实场景，重置成功后连同当前
 * 会话一起失效，所以这里要能在登录态渲染。
 */
export default function ResetPasswordPage() {
  usePageTitle('找回密码')

  const navigate = useNavigate()
  const clearAuth = useAuthStore((state) => state.clear)

  const [step, setStep] = useState<ResetStep>('identify')
  const [account, setAccount] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [hint, setHint] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  // Server-side cooldown is 60 seconds per address. The countdown mirrors the
  // value the response reported rather than a hard-coded 60, so a future change
  // to the policy does not need a frontend release.
  const [cooldown, setCooldown] = useState(0)

  useEffect(() => {
    if (cooldown <= 0) return
    const timer = window.setInterval(() => {
      setCooldown((left) => (left <= 1 ? 0 : left - 1))
    }, 1000)
    return () => window.clearInterval(timer)
  }, [cooldown])

  // 成功之后自动回登录页，并且**预填**用户刚才输入的标识：让他少敲一次，同时
  // 页面上的提示能被读到。replace 是为了让浏览器后退不会回到「已重置」这一步。
  useEffect(() => {
    if (step !== 'done') return
    const timer = window.setTimeout(() => {
      navigate(`${ROUTES.login}?identifier=${encodeURIComponent(account)}`, { replace: true })
    }, DONE_REDIRECT_MS)
    return () => window.clearTimeout(timer)
  }, [step, account, navigate])

  const clearFeedback = () => {
    setError(null)
    setHint(null)
  }

  const handleSendCode = async () => {
    clearFeedback()
    const value = account.trim()
    if (!value) {
      setError('请输入邮箱或手机号。')
      return
    }
    // SMS delivery is billed per message and may not be switched on. The server
    // refuses a handset identifier too (400 + the same hint); stopping here
    // keeps the click from looking like a delivery failure.
    if (!SMS_ENABLED && !value.includes('@')) {
      setError(SMS_UNAVAILABLE_HINT)
      return
    }
    setSending(true)
    try {
      const response = await sendResetCode(value)
      setAccount(value)
      setStep('verify')
      setCooldown(response.resend_after > 0 ? response.resend_after : 60)
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
    } finally {
      setSending(false)
    }
  }

  const handleReset = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    clearFeedback()
    if (!VERIFICATION_CODE_RE.test(code.trim())) {
      setError(CODE_FORMAT_HINT)
      return
    }
    // 与服务端 validatePassword 一致（≥ 8 字节）；「密码是否够强」这类策略不在
    // 前端判断，前端只在本地拦下明显不合规的输入，省一次往返。
    if (password.length < 8) {
      setError('新密码至少 8 位。')
      return
    }
    if (password !== confirmPassword) {
      setError('两次输入的新密码不一致。')
      return
    }
    setSubmitting(true)
    try {
      await resetPassword({
        identifier: account.trim(),
        code: code.trim(),
        new_password: password,
      })
      // 服务端已经吊销了该 uid 的全部会话（含当前这个），所以本地登录态必须一起
      // 复位：否则用户会看到「已重置成功」但界面还是登录态，下一个请求才 401。
      clearAuth()
      setStep('done')
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
    } finally {
      setSubmitting(false)
    }
  }

  const feedback =
    error !== null ? (
      <div role="alert" className="rounded-lg border border-danger bg-danger-soft p-3 text-xs">
        <p className="font-medium text-danger">{error}</p>
        {hint ? <p className="mt-1 text-ink-secondary">{hint}</p> : null}
      </div>
    ) : null

  if (step === 'done') {
    return (
      <div className="mx-auto w-full max-w-md px-4 py-12 sm:px-6">
        <div
          role="status"
          className="rounded-xl border border-success bg-success-soft p-5 text-center text-sm text-success"
        >
          密码已重置，请用新密码登录。
        </div>
        <p className="mt-4 text-center text-xs text-ink-secondary">
          所有设备上的登录状态都已失效，需要重新登录。
        </p>
        <div className="mt-6 text-center">
          <Button
            type="button"
            variant="secondary"
            onClick={() => {
              navigate(`${ROUTES.login}?identifier=${encodeURIComponent(account)}`, { replace: true })
            }}
          >
            去登录
          </Button>
        </div>
      </div>
    )
  }

  return (
    <div className="mx-auto w-full max-w-md px-4 py-12 sm:px-6">
      <header className="text-center">
        <h1 className="text-2xl font-semibold tracking-tight text-ink">找回密码</h1>
        <p className="mx-auto mt-3 max-w-sm text-sm leading-relaxed text-ink-secondary">
          用注册时绑定的邮箱或手机号收一条验证码，设置新密码后即可登录。
        </p>
      </header>

      <section className="mt-8 rounded-2xl border border-border bg-surface p-5 shadow-card sm:p-6">
        {step === 'identify' ? (
          <div className="space-y-4">
            <Input
              label="邮箱或手机号"
              value={account}
              onChange={(event) => setAccount(event.target.value)}
              autoComplete="username"
              placeholder="you@example.com"
              required
            />
            {feedback}
            <Button
              type="button"
              className="w-full"
              loading={sending}
              onClick={() => {
                void handleSendCode()
              }}
            >
              发送验证码
            </Button>
            <p className="text-xs text-ink-muted">
              {SMS_ENABLED
                ? '支持邮箱或手机号；验证码 10 分钟内有效。'
                : '当前仅支持邮箱；短信验证码未开放。'}
            </p>
          </div>
        ) : (
          <form className="space-y-4" onSubmit={handleReset} noValidate>
            {/* 只用用户自己输入的值：不显示服务端返回的任何账号标识（含掩码地址），
                否则「这个地址存不存在」会从文案里漏出去。 */}
            <p role="status" className="rounded-lg border border-info bg-info-soft p-3 text-xs text-ink-secondary">
              如果 {account} 已注册，验证码已发送。
            </p>
            <div className="flex items-end gap-2">
              <Input
                label="验证码"
                value={code}
                onChange={(event) => setCode(event.target.value)}
                autoComplete="one-time-code"
                inputMode="numeric"
                required
              />
              <Button
                variant="secondary"
                size="md"
                type="button"
                className="shrink-0 whitespace-nowrap"
                loading={sending}
                disabled={cooldown > 0}
                onClick={() => {
                  void handleSendCode()
                }}
              >
                {cooldown > 0 ? `${cooldown}s 后重发` : '重新获取'}
              </Button>
            </div>
            <Input
              label="新密码"
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
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
            {feedback}
            <Button type="submit" className="w-full" loading={submitting}>
              重置密码
            </Button>
            <p className="text-xs text-ink-muted">
              重置成功后所有设备都会退出登录，需要用新密码重新登录。
            </p>
          </form>
        )}
      </section>

      <p className="mt-6 text-center text-xs text-ink-secondary">
        想起密码了？
        <Link to={ROUTES.login} className="ml-1 font-medium text-primary hover:text-primary-hover">
          返回登录
        </Link>
      </p>
    </div>
  )
}
