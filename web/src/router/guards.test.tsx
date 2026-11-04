import { act, cleanup, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { ADMIN_PRICING_ROLES } from '@/lib/constants'
import type { UserRole } from '@/lib/constants'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'
import { renderWithProviders } from '@/test/renderConsole'
import { RequireAuth } from './guards'

/**
 * `RequireAuth` 的 `roles` 是 2026-11-02 新增的单页角色收窄能力：`role` 决定
 * 「是不是运营后台的人」，`roles` 再决定「这个运营后台的人能不能进这一页」。
 *
 * 三条断言缺一不可：
 * 1. 传了 `roles`，被排除的角色（FINANCE）看到 403 页，而不是把页面渲染出来；
 * 2. 传了 `roles`，被包含的角色（OPS / SUPER_ADMIN）正常拿到 children；
 * 3. **不**传 `roles`，FINANCE 依然能进 —— 保护既有 10 条运营路由的行为不被误改。
 */

function userOf(role: UserRole): CurrentUser {
  return {
    id: '1',
    email: 'demo@yunshu.test',
    display_name: '演示账号',
    role,
    status: 'NORMAL',
    csrf_token: 'csrf',
  }
}

/** 沿用 AppShell.test.tsx 的 auth store 夹具写法：直接置状态，不打接口。 */
function signInAs(role: UserRole): void {
  act(() => {
    useAuthStore.setState({ status: 'authenticated', user: userOf(role), error: null, hint: null })
  })
}

/**
 * 白名单直接复用生产常量 `ADMIN_PRICING_ROLES`，不在这里另抄一份字面量：
 * 抄一份的话「白名单内容」就有两个真理源，有人把 FINANCE 塞进常量时组件层用例
 * 仍然全绿（2026-11-02 变异实测确认过这个盲区），无法起到防回退作用。
 */
function renderGuarded(roles?: readonly UserRole[]) {
  return renderWithProviders(
    <RequireAuth role="admin" roles={roles}>
      <p>定价页正文</p>
    </RequireAuth>,
  )
}

afterEach(() => {
  cleanup()
  act(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })
})

describe('RequireAuth 的单页角色收窄', () => {
  it('FINANCE 不在 roles 里时渲染 403 页，不渲染页面正文', () => {
    signInAs('FINANCE')
    renderGuarded(ADMIN_PRICING_ROLES)

    expect(screen.getByText('403 · 无权限')).toBeInTheDocument()
    expect(screen.queryByText('定价页正文')).not.toBeInTheDocument()
  })

  it('OPS 在 roles 里时渲染 children', () => {
    signInAs('OPS')
    renderGuarded(ADMIN_PRICING_ROLES)

    expect(screen.getByText('定价页正文')).toBeInTheDocument()
    expect(screen.queryByText('403 · 无权限')).not.toBeInTheDocument()
  })

  it('SUPER_ADMIN 在 roles 里时渲染 children', () => {
    signInAs('SUPER_ADMIN')
    renderGuarded(ADMIN_PRICING_ROLES)

    expect(screen.getByText('定价页正文')).toBeInTheDocument()
    expect(screen.queryByText('403 · 无权限')).not.toBeInTheDocument()
  })

  it('不传 roles 时 FINANCE 仍然能进（既有运营路由行为不变）', () => {
    signInAs('FINANCE')
    renderGuarded()

    expect(screen.getByText('定价页正文')).toBeInTheDocument()
    expect(screen.queryByText('403 · 无权限')).not.toBeInTheDocument()
  })

  it('roles 传空数组时按 fail-closed 处理，连 OPS 也拿不到页面', () => {
    signInAs('OPS')
    renderGuarded([])

    expect(screen.getByText('403 · 无权限')).toBeInTheDocument()
    expect(screen.queryByText('定价页正文')).not.toBeInTheDocument()
  })

  it('PERSONAL 进运营后台仍被 role="admin" 挡住（roles 不改变这一层）', () => {
    signInAs('PERSONAL')
    renderGuarded(ADMIN_PRICING_ROLES)

    expect(screen.getByText('403 · 无权限')).toBeInTheDocument()
    expect(screen.queryByText('定价页正文')).not.toBeInTheDocument()
  })
})
