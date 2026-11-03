import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useKeys } from '@/api/console'
import { usePublicModels } from '@/api/public'
import { GettingStarted } from './GettingStarted'

vi.mock('@/api/console', () => ({ useKeys: vi.fn() }))
vi.mock('@/api/public', () => ({ usePublicModels: vi.fn() }))

const mockKeys = vi.mocked(useKeys)
const mockModels = vi.mocked(usePublicModels)

function keysResult(total: number) {
  return { data: { data: [], total }, isLoading: false, isError: false } as unknown as ReturnType<
    typeof useKeys
  >
}

beforeEach(() => {
  window.localStorage.clear()
  vi.clearAllMocks()
  mockModels.mockReturnValue({
    data: { data: [{ model_name: 'deepseek-v4' }] },
  } as unknown as ReturnType<typeof usePublicModels>)
})

describe('GettingStarted', () => {
  it('shows the three steps and a copyable example only before a key exists', () => {
    mockKeys.mockReturnValue(keysResult(0))
    render(
      <MemoryRouter>
        <GettingStarted />
      </MemoryRouter>,
    )

    expect(screen.getByText('开始使用')).toBeInTheDocument()
    expect(screen.getByText('创建 API 密钥')).toBeInTheDocument()
    expect(screen.getByText('确认余额')).toBeInTheDocument()
    expect(screen.getByText('发送首个请求')).toBeInTheDocument()
    expect(screen.getByText(/deepseek-v4/)).toBeInTheDocument()
  })

  it('stays out of the way once a key exists', () => {
    mockKeys.mockReturnValue(keysResult(1))
    render(
      <MemoryRouter>
        <GettingStarted />
      </MemoryRouter>,
    )

    expect(screen.queryByText('开始使用')).not.toBeInTheDocument()
  })

  it('can be dismissed for this session', async () => {
    const user = userEvent.setup()
    mockKeys.mockReturnValue(keysResult(0))
    render(
      <MemoryRouter>
        <GettingStarted />
      </MemoryRouter>,
    )

    await user.click(screen.getByRole('button', { name: '收起指引' }))
    expect(screen.queryByText('开始使用')).not.toBeInTheDocument()
  })
})
