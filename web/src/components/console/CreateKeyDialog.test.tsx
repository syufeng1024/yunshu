import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { renderWithProviders } from '@/test/renderConsole'
import { CreateKeyDialog } from './CreateKeyDialog'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

const model = {
  id: 7,
  model_name: 'deepseek-chat',
  display_name: 'DeepSeek Chat',
  model_type: 'TEXT',
  context_length: 65_536,
  protocol: 'OPENAI_COMPAT',
  aliases: [],
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/console/models?')) {
      return Promise.resolve({ items: [model], page: 1, page_size: 100, total: 1 })
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })
  mockedPost.mockResolvedValue({
    items: [
      {
        id: 99,
        key_name: 'ui-permanent',
        key: 'sk-plain-secret-123456',
        key_prefix: 'sk-plain',
        key_last4: '3456',
        key_masked: 'sk-plain****3456',
        model_scope_mode: 'ALL',
        model_ids: [],
        group_code: 'standard',
        expire_at: null,
        ip_allowlist: [],
      },
    ],
    count: 1,
    warning: '完整密钥仅本次返回，关闭后无法再次查看',
  })
})

afterEach(cleanup)

describe('CreateKeyDialog', () => {
  // 参考站的「新建密钥」只有名称与范围两类必填项；我们的四步向导与来源地址白名单
  // 都已下线（2026-11-05 需求方裁定），这里钉住「一屏填完」的形态。
  it('一屏展示名称、数量、模型范围与可选有效期，没有白名单与分步按钮', () => {
    renderWithProviders(<CreateKeyDialog open onClose={vi.fn()} onCreated={vi.fn()} />)

    expect(screen.getByLabelText(/密钥名称/)).toBeInTheDocument()
    expect(screen.getByLabelText(/创建数量/)).toBeInTheDocument()
    expect(screen.getByLabelText(/可调用的模型/)).toBeInTheDocument()
    expect(screen.getByLabelText(/到期时间/)).toBeInTheDocument()
    expect(screen.getByText(/留空表示永不过期/)).toBeInTheDocument()

    expect(screen.queryByText(/白名单/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下一步' })).not.toBeInTheDocument()
  })

  it('不填有效期直接提交：请求体不带 expire_at，也不带已下线的来源地址字段', async () => {
    const onCreated = vi.fn()
    const user = userEvent.setup()
    renderWithProviders(<CreateKeyDialog open onClose={vi.fn()} onCreated={onCreated} />)

    await user.type(screen.getByLabelText(/密钥名称/), 'ui-permanent')
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    await waitFor(() => expect(onCreated).toHaveBeenCalled())
    const body = mockedPost.mock.calls[0]?.[1] as Record<string, unknown>
    expect(body.name).toBe('ui-permanent')
    expect(body.model_scope_mode).toBe('ALL')
    expect(body).not.toHaveProperty('expire_at')
    expect(body).not.toHaveProperty('ip_allowlist')
    expect(body).not.toHaveProperty('group_code')
    expect(body).not.toHaveProperty('model_ids')
  })

  it('填写有效期时按时间提交', async () => {
    const user = userEvent.setup()
    renderWithProviders(<CreateKeyDialog open onClose={vi.fn()} onCreated={vi.fn()} />)

    await user.type(screen.getByLabelText(/密钥名称/), 'with-expiry')
    fireEvent.change(screen.getByLabelText(/到期时间/), {
      target: { value: '2030-01-01T10:00' },
    })
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    await waitFor(() => expect(mockedPost).toHaveBeenCalledTimes(1))
    const body = mockedPost.mock.calls[0]?.[1] as Record<string, unknown>
    expect(new Date(body.expire_at as string).getFullYear()).toBe(2030)
  })

  it('选「仅指定模型」时提交 model_ids', async () => {
    const user = userEvent.setup()
    renderWithProviders(<CreateKeyDialog open onClose={vi.fn()} onCreated={vi.fn()} />)

    await user.type(screen.getByLabelText(/密钥名称/), 'scoped')
    await user.selectOptions(screen.getByLabelText(/可调用的模型/), 'SELECTED')
    await user.click(await screen.findByRole('checkbox', { name: /DeepSeek Chat/ }))
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    await waitFor(() => expect(mockedPost).toHaveBeenCalledTimes(1))
    const body = mockedPost.mock.calls[0]?.[1] as Record<string, unknown>
    expect(body.model_scope_mode).toBe('SELECTED')
    expect(body.model_ids).toEqual([7])
  })

  it('数量超出 1–10 时报错且不发请求', async () => {
    const user = userEvent.setup()
    renderWithProviders(<CreateKeyDialog open onClose={vi.fn()} onCreated={vi.fn()} />)

    await user.type(screen.getByLabelText(/密钥名称/), 'prod-key')
    const count = screen.getByLabelText(/创建数量/)
    await user.clear(count)
    await user.type(count, '11')
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    expect(await screen.findByText(/创建数量需为 1–10 的整数/)).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()
  })
})
