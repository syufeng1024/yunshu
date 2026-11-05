import { cleanup, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type { ReviewItem } from '@/api/admin'
import { useAuthStore } from '@/stores/auth'
import { renderWithProviders } from '@/test/renderConsole'
import AdminReviewsPage from './ReviewsPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

function review(overrides: Partial<ReviewItem> = {}): ReviewItem {
  return {
    id: 1,
    biz_type: 'QUOTA_APPLY',
    biz_id: 4101,
    biz_no: 'QA20261021001',
    applicant_id: 99,
    applicant_name: '申请人甲',
    amount_micro: 50_000_000,
    summary: '月度调用量上涨，申请提高日额度',
    business_state: 'PENDING',
    state: 'PENDING',
    created_at: '2026-10-21T08:00:00Z',
    anomaly: false,
    ...overrides,
  }
}

const emptyAudit = {
  items: [],
  page: 1,
  page_size: 50,
  total: 0,
  from: '2026-10-21T00:00:00Z',
  to: '2026-10-28T00:00:00Z',
}

function installMock(items: ReviewItem[]) {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/admin/reviews?')) {
      return Promise.resolve({
        items,
        page: 1,
        page_size: 20,
        total: items.length,
      })
    }
    if (path.startsWith('/api/admin/audit-logs?')) return Promise.resolve(emptyAudit)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
  useAuthStore.setState({ user: null })
})

afterEach(() => {
  cleanup()
  useAuthStore.setState({ user: null })
})

describe('ReviewsPage', () => {
  it('ReviewsPage：默认 PENDING 且按提交时间升序', async () => {
    const late = review({ id: 1, biz_no: 'LATE-ROW', created_at: '2026-10-22T08:00:00Z' })
    const early = review({ id: 2, biz_no: 'EARLY-ROW', created_at: '2026-10-21T08:00:00Z' })
    installMock([late, early])

    renderWithProviders(<AdminReviewsPage />)

    const table = await screen.findByRole('table', { name: '审核队列' })
    await screen.findByText(/LATE-ROW/)
    const rows = within(table).getAllByRole('row')
    expect(rows[1]).toHaveTextContent('LATE-ROW')
    expect(rows[2]).toHaveTextContent('EARLY-ROW')

    const firstQuery = mockedGet.mock.calls
      .map((call) => call[0])
      .find((path) => path.startsWith('/api/admin/reviews?'))
    expect(firstQuery).toBeTruthy()
    const params = new URL(firstQuery ?? '', 'http://localhost').searchParams
    expect(params.get('state')).toBe('PENDING')
    expect(params.has('sort')).toBe(false)
  })

  it('ReviewsPage：驳回不填理由不发请求', async () => {
    installMock([review()])
    const user = userEvent.setup()
    renderWithProviders(<AdminReviewsPage />)
    await screen.findByText('申请人甲')

    await user.click(screen.getByRole('button', { name: '驳回' }))
    await user.click(screen.getByRole('button', { name: '确认驳回' }))

    expect(await screen.findByText('驳回理由至少 5 个字。')).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()

    await user.type(screen.getByLabelText(/驳回理由/), '太短')
    await user.click(screen.getByRole('button', { name: '确认驳回' }))
    expect(await screen.findByText('驳回理由至少 5 个字。')).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()
  })

  it('ReviewsPage：自审按钮置灰', async () => {
    // CurrentUser.id is a string in the OpenAPI schema; applicant_id is an
    // int64 number. This is the type-normalisation regression case.
    useAuthStore.setState({
      user: {
        id: '7',
        email: 'ops@example.com',
        display_name: '运营甲',
        role: 'OPS',
        status: 'NORMAL',
        csrf_token: 'test-csrf',
      },
    })
    installMock([review({ applicant_id: 7 })])

    renderWithProviders(<AdminReviewsPage />)
    await screen.findByText('申请人甲')

    expect(screen.getByRole('button', { name: '通过' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '驳回' })).toBeDisabled()
    expect(
      screen.getByText('审批人不得为申请人：请转交其他审核人处理'),
    ).toBeInTheDocument()
  })

  it('ReviewsPage：余额调整展示金额、方向与理由', async () => {
    installMock([
      review({
        biz_type: 'BALANCE_ADJUST',
        biz_no: 'ADJ#9001',
        amount_micro: 50_000_000,
        direction: 'INCREASE',
        summary: '上游故障补偿',
      }),
    ])

    renderWithProviders(<AdminReviewsPage />)
    await screen.findByText('申请人甲')

    expect(screen.getByText('方向：增加')).toBeInTheDocument()
    expect(screen.getByText('上游故障补偿')).toBeInTheDocument()
    expect(screen.getByText('5,000 积分')).toBeInTheDocument()
  })

  it('ReviewsPage：发票审批要求 invoice_url', async () => {
    installMock([
      review({
        biz_type: 'INVOICE_APPLY',
        biz_no: 'INV20261021001',
        summary: '开票申请',
      }),
    ])
    const user = userEvent.setup()
    renderWithProviders(<AdminReviewsPage />)
    await screen.findByText('申请人甲')

    await user.click(screen.getByRole('button', { name: '通过' }))
    await screen.findByLabelText(/发票文件链接/)
    await user.click(screen.getByRole('button', { name: '确认通过' }))

    expect(await screen.findByText('开票审批必须填写发票文件链接。')).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()
  })
})
