import { api } from '@/api/client'
import type { components } from './types'

/**
 * 自助找回密码与绑定登录邮箱的四个调用。
 *
 * 集中在一个文件里，而不是散在三个页面里直接拼路径：路径字符串写错两次不会报错，
 * 只会在运行时变成 404；请求/响应类型取自 `openapi.yaml` 生成的
 * `components['schemas'][...]`，所以契约改动会在 `npm run typecheck` 时报出来。
 *
 * `api.post` 已经自动带 `X-CSRF-Token`（见 `web/src/api/csrf.ts`），这四个函数里
 * **不要**手工加任何 header。
 */

type VerificationCodeRequest = components['schemas']['VerificationCodeRequest']
type VerificationCodeResponse = components['schemas']['VerificationCodeResponse']
type PasswordResetRequest = components['schemas']['PasswordResetRequest']
type EmailBindCodeRequest = components['schemas']['EmailBindCodeRequest']
type EmailBindRequest = components['schemas']['EmailBindRequest']
type UserResponse = components['schemas']['UserResponse']

/**
 * 发送重置密码的验证码（purpose=reset）。
 *
 * 服务端对「该地址有没有账号」零区分：无论结果如何都返回同一个 200，所以调用方
 * **不能**用返回值判断账号是否存在，也不要把响应里的任何内容当成账号信息展示。
 */
export function sendResetCode(identifier: string): Promise<VerificationCodeResponse> {
  const body: VerificationCodeRequest = { identifier, purpose: 'reset' }
  return api.post<VerificationCodeResponse>('/api/v1/auth/verification-code', body)
}

/**
 * 提交验证码与新密码。成功是 204（无响应体、不签发会话）。
 *
 * 校验顺序在服务端是「字段 → 密码规则 → 验码 → 写库」，所以这里先在本地把明显
 * 不合规的输入拦下来只是省一次往返，不是安全边界。
 */
export function resetPassword(payload: PasswordResetRequest): Promise<void> {
  return api.post<void>('/api/v1/auth/password/reset', payload)
}

/** 发送绑定登录邮箱的验证码（已登录，purpose=bind）。 */
export function sendBindEmailCode(email: string): Promise<VerificationCodeResponse> {
  const body: EmailBindCodeRequest = { email }
  return api.post<VerificationCodeResponse>('/api/v1/auth/email/code', body)
}

/** 验码并写入登录邮箱。成功后返回含新 email 的用户视图，会话不失效。 */
export function bindEmail(payload: EmailBindRequest): Promise<UserResponse> {
  return api.post<UserResponse>('/api/v1/auth/email', payload)
}
