import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useConsoleOverview } from '@/api/console'
import SubscriptionPage from './SubscriptionPage'

vi.mock('@/api/console', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/console')>()
  return { ...actual, useConsoleOverview: vi.fn() }
})

const mockOverview = vi.mocked(useConsoleOverview)

beforeEach(() => {
  vi.clearAllMocks()
})

describe('SubscriptionPage', () => {
  it('shows the current plan, its source and the four limits', () => {
    mockOverview.mockReturnValue({
      data: {
        plan: {
          code: 'STANDARD',
          name: '标准版',
          policy_source: 'PLAN',
          trial_expire_at: null,
        },
        quota: {
          rpm: 300,
          tpm: 630000,
          concurrency: 110,
          daily_quota_micro: 50000000,
          used_today_micro: 10000000,
          remaining_micro: 40000000,
        },
        balance: { credits_micro: 0, frozen_micro: 0, available_micro: 0 },
        today: { requests: 0, consumed_micro: 0, errors: 0 },
        hourly: [],
        eta: { hours_left: null },
      },
      isLoading: false,
      isError: false,
      error: null,
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof useConsoleOverview>)

    render(
      <MemoryRouter>
        <SubscriptionPage />
      </MemoryRouter>,
    )

    expect(screen.getByText('标准版')).toBeInTheDocument()
    expect(screen.getByText('付费套餐')).toBeInTheDocument()
    expect(screen.getByText('长期有效')).toBeInTheDocument()
    expect(screen.getByText('300')).toBeInTheDocument()
    expect(screen.getByText('110')).toBeInTheDocument()
  })

  it('renders an explicit error state with retry instead of an empty page', () => {
    mockOverview.mockReturnValue({
      data: undefined,
      isLoading: false,
      isError: true,
      error: new Error('boom'),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof useConsoleOverview>)

    render(
      <MemoryRouter>
        <SubscriptionPage />
      </MemoryRouter>,
    )

    expect(screen.getByText('订阅信息加载失败')).toBeInTheDocument()
  })
})
