import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { api, errorHint, errorMessage } from '@/api/client'
import { usePublicPlans } from '@/api/public'
import { Badge, Button, ErrorState, Icon, Input, Skeleton, Tabs } from '@/components'
import { safeNextPath } from '@/lib/authRoutes'
import { ROUTES } from '@/lib/constants'
import { formatCreditAmount } from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'
import { useAuthStore } from '@/stores/auth'
import type { CurrentUser } from '@/stores/auth'

type LoginMode = 'password' | 'code'

// 服务端 AUTH_SMS_MODE 的**部署期镜像**（'true' / 'false'）。SPA 读不到服务端环境
// 变量，`GET /auth/capabilities`（P2-1）落地后删除。两者必须一起翻，否则页脚会说谎
// —— 文案说支持短信、提交后却 400。
const SMS_ENABLED = import.meta.env.VITE_AUTH_SMS_ENABLED === 'true'

/**
 * 登录页脚的渠道说明。
 *
 * 抽成纯函数（与本目录其它页的 `isSelfAudit` / `detectUserSearchField` 同一个理由）：
 * `import.meta.env` 在模块加载时就被内联，测试无法逐个用例翻转它，而这两句话必须
 * 两条分支都被断言过——它们是「这个部署能不能用短信」的唯一对外表述。
 */
export function loginChannelCopy(smsEnabled: boolean): string {
  return smsEnabled
    ? '支持邮箱、手机号或用户名登录。'
    : '支持邮箱、手机号或用户名登录；短信验证码未开放。'
}

/** 注册表单上方的渠道说明，与页脚同一份开关。 */
export function registerChannelCopy(smsEnabled: boolean): string {
  return smsEnabled
    ? '注册支持邮箱与手机号；短信验证码已开放'
    : '注册只支持邮箱；短信验证码暂未开放。'
}

// The server accepts exactly six digits. Checking here keeps an incomplete code
// from being sent at all: every rejected attempt counts against the five-try
// budget and burns the code, so a typo would cost the user a whole new e-mail.
const VERIFICATION_CODE_RE = /^\d{6}$/
const CODE_FORMAT_HINT = '验证码是 6 位数字，请检查是否输完整。'

interface AuthResponse {
  user: CurrentUser
}

interface AgreementCheckboxProps {
  checked: boolean
  onChange: (checked: boolean) => void
}

/**
 * 登录与注册共用同一个协议勾选块。
 *
 * 抽成组件不是为了省几行 JSX，而是为了只有一份文案：两处各写一份时，改协议名
 * 或加一项《服务条款》很容易只改一边。而「用户同意了什么」是合规事实，两份文案
 * 漂移的代价不是视觉问题。
 */
function AgreementCheckbox({ checked, onChange }: AgreementCheckboxProps) {
  return (
    <label className="flex items-start gap-2 text-xs text-ink-secondary">
      <input
        type="checkbox"
        checked={checked}
        onChange={(event) => onChange(event.target.checked)}
        className="mt-0.5 h-4 w-4 rounded border-border-strong text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
      />
      <span>
        我已阅读并同意
        {/* 书名号放进链接里：点击区域覆盖整个《用户协议》，而不是只有四个字。
            协议名必须是能点的链接，否则等于「同意一份看不到的文件」；
            target=_blank 是为了不丢掉已经填好的表单。 */}
        <Link
          to={ROUTES.terms}
          target="_blank"
          rel="noopener noreferrer"
          className="font-medium text-primary hover:text-primary-hover"
        >
          《用户协议》
        </Link>
        和
        <Link
          to={ROUTES.privacy}
          target="_blank"
          rel="noopener noreferrer"
          className="font-medium text-primary hover:text-primary-hover"
        >
          《隐私政策》
        </Link>
        。
      </span>
    </label>
  )
}

export default function LoginPage() {
  usePageTitle('登录 / 注册')

  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const plansQuery = usePublicPlans()
  const authStatus = useAuthStore((state) => state.status)

  const tab = searchParams.get('tab') === 'register' ? 'register' : 'login'
  const planCode = searchParams.get('plan')
  const intent = searchParams.get('intent')
  const next = searchParams.get('next')

  const trialPlan = plansQuery.data?.data.find((plan) => plan.code === 'TRIAL')
  const selectedPlan = planCode
    ? plansQuery.data?.data.find((plan) => plan.code === planCode)
    : undefined

  const [loginMode, setLoginMode] = useState<LoginMode>('password')
  // 找回密码页成功后会把用户刚才输入的标识带回来（?identifier=），这里直接预填：
  // 刚重置完密码的人已经证明过自己知道这个标识，再让他敲一次只会增加出错机会。
  const [identifier, setIdentifier] = useState(searchParams.get('identifier') ?? '')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [code, setCode] = useState('')
  const [agreed, setAgreed] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [hint, setHint] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  // Verification codes are issued by POST /api/v1/auth/verification-code. The
  // server enforces a 60s cooldown; the countdown below only mirrors it so the
  // button is not clickable during a window that would answer 429.
  const [sendingCode, setSendingCode] = useState(false)
  const [codeSent, setCodeSent] = useState(false)
  const [cooldown, setCooldown] = useState(0)
  const [claimNotice, setClaimNotice] = useState(false)

  // A signed-in visitor should never sit on the login form. Before this, the
  // header kept offering 登录 even after a successful sign-in, so clicking it
  // showed the form again and looked like the session had been dropped.
  const currentUser = useAuthStore((state) => state.user)
  useEffect(() => {
    if (authStatus === 'authenticated') {
      navigate(safeNextPath(next, currentUser?.role ?? null), { replace: true })
    }
  }, [authStatus, navigate, next, currentUser?.role])

  const clearFeedback = () => {
    setError(null)
    setHint(null)
  }

  const selectTab = (nextTab: string) => {
    clearFeedback()
    const params = new URLSearchParams(searchParams)
    params.set('tab', nextTab)
    setSearchParams(params, { replace: true })
  }

  const finishAuth = (user: CurrentUser) => {
    useAuthStore.setState({ status: 'authenticated', user, error: null, hint: null })
    navigate(safeNextPath(next, user.role), { replace: true })
  }

  const handleLogin = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    clearFeedback()
    const account = identifier.trim()
    if (!account) {
      setError('请输入邮箱、手机号或用户名。')
      return
    }
    if (loginMode === 'password' && !password) {
      setError('请输入密码。')
      return
    }
    if (loginMode === 'code' && !VERIFICATION_CODE_RE.test(code.trim())) {
      setError(CODE_FORMAT_HINT)
      return
    }
    if (!agreed) {
      setError('请先勾选并同意用户协议与隐私政策。')
      return
    }

    setSubmitting(true)
    try {
      const payload =
        loginMode === 'password'
          ? { identifier: account, password }
          : { identifier: account, code: code.trim() }
      const response = await api.post<AuthResponse>('/api/v1/auth/login', payload)
      finishAuth(response.user)
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
    } finally {
      setSubmitting(false)
    }
  }

  const handleRegister = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    clearFeedback()
    const account = identifier.trim()
    if (!account) {
      setError('请输入邮箱或手机号。')
      return
    }
    if (!VERIFICATION_CODE_RE.test(code.trim())) {
      setError(CODE_FORMAT_HINT)
      return
    }
    if (!password) {
      setError('请设置密码。')
      return
    }
    if (password !== confirmPassword) {
      setError('两次输入的密码不一致。')
      return
    }
    if (!agreed) {
      setError('请先勾选并同意用户协议与隐私政策。')
      return
    }

    setSubmitting(true)
    try {
      const response = await api.post<AuthResponse>('/api/v1/auth/register', {
        identifier: account,
        code: code.trim(),
        password,
      })
      finishAuth(response.user)
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
    } finally {
      setSubmitting(false)
    }
  }

  const handleSendCode = async (purpose: 'login' | 'register') => {
    clearFeedback()
    const account = identifier.trim()
    if (!account) {
      setError('请先填写邮箱或手机号。')
      return
    }
    // SMS delivery is billed per message and has no provider yet. The server
    // refuses it too (400 + hint); stopping here keeps the click from looking
    // like a delivery failure.
    if (!account.includes('@')) {
      setError('短信验证码暂未开放，请使用邮箱。')
      return
    }
    setSendingCode(true)
    try {
      await api.post<{ channel: string; expires_in: number; resend_after: number }>(
        '/api/v1/auth/verification-code',
        { identifier: account, purpose },
      )
      setCodeSent(true)
      setCooldown(60)
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
    } finally {
      setSendingCode(false)
    }
  }

  // One timer for both tabs: the cooldown is per address on the server, so
  // switching from register to login must not unlock the button.
  useEffect(() => {
    if (cooldown <= 0) return
    const timer = window.setInterval(() => {
      setCooldown((left) => (left <= 1 ? 0 : left - 1))
    }, 1000)
    return () => window.clearInterval(timer)
  }, [cooldown])

  const handleClaim = () => {
    if (authStatus === 'authenticated') {
      navigate(`${ROUTES.consoleRecharge}?claim=trial`)
      return
    }
    setClaimNotice(true)
    selectTab('register')
  }

  const feedback =
    error !== null ? (
      <div role="alert" className="rounded-lg border border-danger bg-danger-soft p-3 text-xs">
        <p className="font-medium text-danger">{error}</p>
        {hint ? <p className="mt-1 text-ink-secondary">{hint}</p> : null}
      </div>
    ) : null

  const loginForm = (
    <form className="space-y-4" onSubmit={handleLogin} noValidate>
      <div className="flex items-center gap-3 text-xs">
        <button
          type="button"
          aria-pressed={loginMode === 'password'}
          onClick={() => setLoginMode('password')}
          className={loginMode === 'password' ? 'font-medium text-primary' : 'text-ink-muted'}
        >
          密码登录
        </button>
        <span className="text-ink-muted">/</span>
        <button
          type="button"
          aria-pressed={loginMode === 'code'}
          onClick={() => setLoginMode('code')}
          className={loginMode === 'code' ? 'font-medium text-primary' : 'text-ink-muted'}
        >
          验证码登录
        </button>
      </div>

      <Input
        label="账号"
        value={identifier}
        onChange={(event) => setIdentifier(event.target.value)}
        autoComplete="username"
        placeholder="邮箱 / 手机号 / 用户名"
        required
      />

      {loginMode === 'password' ? (
        <Input
          label="密码"
          type="password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          autoComplete="current-password"
          required
        />
      ) : (
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
            loading={sendingCode}
            disabled={cooldown > 0}
            onClick={() => {
              void handleSendCode('login')
            }}
          >
            {cooldown > 0 ? `${cooldown}s 后重发` : codeSent ? '重新获取' : '获取验证码'}
          </Button>
        </div>
      )}

      {/* 「忘记密码」贴着密码输入：用户是在输入密码的那一刻才想起忘记密码的，
          放在提交按钮下面要往下找一行。现在它是真链接，指向 /reset-password。 */}
      <div className="flex justify-end">
        <Link to={ROUTES.resetPassword} className="text-xs text-ink-secondary hover:text-ink">
          忘记密码
        </Link>
      </div>

      <AgreementCheckbox checked={agreed} onChange={setAgreed} />

      {feedback}

      <Button type="submit" className="w-full" loading={submitting}>
        登录
      </Button>

      <p className="text-xs text-ink-muted">{loginChannelCopy(SMS_ENABLED)}</p>
    </form>
  )

  const registerForm = (
    <form className="space-y-4" onSubmit={handleRegister} noValidate>
      {claimNotice ? (
        <div className="rounded-lg border border-info bg-info-soft p-3 text-xs text-ink-secondary">
          完成注册后，请在控制台主动领取体验积分；积分不会自动到账。
        </div>
      ) : null}

      <Input
        label="邮箱"
        value={identifier}
        onChange={(event) => setIdentifier(event.target.value)}
        autoComplete="username"
        placeholder="you@example.com"
        required
      />

      <p className="-mt-2 text-xs text-ink-muted">{registerChannelCopy(SMS_ENABLED)}</p>

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
          loading={sendingCode}
          disabled={cooldown > 0}
          onClick={() => {
            void handleSendCode('register')
          }}
        >
          {cooldown > 0 ? `${cooldown}s 后重发` : codeSent ? '重新获取' : '获取验证码'}
        </Button>
      </div>

      <Input
        label="密码"
        type="password"
        value={password}
        onChange={(event) => setPassword(event.target.value)}
        autoComplete="new-password"
        placeholder="至少 8 位"
        required
      />

      <Input
        label="确认密码"
        type="password"
        value={confirmPassword}
        onChange={(event) => setConfirmPassword(event.target.value)}
        autoComplete="new-password"
        placeholder="再次输入密码"
        required
      />

      <AgreementCheckbox checked={agreed} onChange={setAgreed} />

      {feedback}

      <Button type="submit" className="w-full" loading={submitting}>
        注册并进入控制台
      </Button>

      <p className="text-xs text-ink-muted">
        注册环节不做实名；领取体验积分与申请开票时再按要求核验。
      </p>
    </form>
  )

  return (
    <div className="mx-auto w-full max-w-5xl px-4 py-12 sm:px-6">
      {searchParams.get('reason') === 'password-changed' ? (
        <div
          role="status"
          className="mx-auto mb-6 max-w-xl rounded-lg border border-success bg-success-soft p-3 text-center text-xs text-success"
        >
          密码已更新，其他设备已退出登录。请用新密码重新登录。
        </div>
      ) : null}
      <header className="text-center">
        <Badge variant="primary" dot>
          {planCode ? `已选择套餐：${selectedPlan?.name ?? planCode}` : '云枢 API'}
        </Badge>
        <h1 className="mt-4 text-3xl font-semibold tracking-tight text-ink">
          {tab === 'login' ? '登录账号' : '创建账号'}
        </h1>
        <p className="mx-auto mt-3 max-w-xl text-sm leading-relaxed text-ink-secondary">
          {tab === 'login'
            ? '进入控制台，管理密钥、用量和账单。'
            : '注册后即可创建密钥并发起调用。'}
        </p>
      </header>

      {/* lg:items-start：右侧信息卡与左侧表单不等高，默认 stretch 会把表单卡
          拉成和信息栏同高，底部留出一块空白，看起来像没排完。 */}
      <div className="mt-8 grid gap-6 lg:grid-cols-[minmax(0,1fr)_20rem] lg:items-start">
        <section className="rounded-2xl border border-border bg-surface p-5 shadow-card sm:p-6">
          <Tabs
            ariaLabel="登录或注册"
            value={tab}
            onChange={selectTab}
            items={[
              { id: 'login', label: '登录', content: loginForm },
              { id: 'register', label: '注册', content: registerForm },
            ]}
          />
        </section>

        <aside className="space-y-4">
          <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
            <Badge variant="primary">体验版</Badge>
            <h2 className="mt-3 text-sm font-semibold text-ink">主动领取体验积分</h2>
            {plansQuery.isLoading ? (
              <div className="mt-3 space-y-2">
                <Skeleton className="h-4 w-full" />
                <Skeleton className="h-4 w-2/3" />
              </div>
            ) : plansQuery.isError ? (
              <div className="mt-3">
                <ErrorState
                  compact
                  message={errorMessage(plansQuery.error)}
                  onRetry={() => {
                    void plansQuery.refetch()
                  }}
                />
              </div>
            ) : trialPlan ? (
              <p className="mt-2 text-xs leading-relaxed text-ink-secondary">
                注册后可在控制台主动领取{' '}
                <span className="font-medium tabular-nums text-ink">
                  {formatCreditAmount(trialPlan.grant_bonus_micro)}
                </span>{' '}
                积分（7 天体验版），领取后即可调用。
              </p>
            ) : (
              <p className="mt-2 text-xs leading-relaxed text-ink-secondary">
                注册后可在控制台主动领取体验积分，额度以控制台展示为准。
              </p>
            )}
            <Button
              type="button"
              variant="secondary"
              size="sm"
              className="mt-4 w-full"
              onClick={handleClaim}
            >
              主动领取体验积分
            </Button>
          </section>

          <section className="rounded-xl border border-border bg-surface p-5">
            <h2 className="text-sm font-semibold text-ink">接入前先看文档</h2>
            <p className="mt-2 text-xs leading-relaxed text-ink-secondary">
              Base URL 是否带 /v1、模型名从哪里取，文档页与工具集成页都给了最小请求。
            </p>
            <div className="mt-3 flex flex-wrap gap-3 text-xs">
              <Link to={ROUTES.docs} className="font-medium text-primary hover:text-primary-hover">
                开发者文档
              </Link>
              <Link
                to={ROUTES.integrations}
                className="font-medium text-primary hover:text-primary-hover"
              >
                工具集成
              </Link>
            </div>
          </section>

          {intent === 'sales' ? (
            <div className="flex gap-2 rounded-xl border border-info bg-info-soft p-4 text-xs text-ink-secondary">
              <Icon name="ticket" className="mt-0.5 h-4 w-4 shrink-0 text-info" />
              企业采购咨询：完成注册后可在控制台工单中心提交需求。
            </div>
          ) : null}
        </aside>
      </div>
    </div>
  )
}
