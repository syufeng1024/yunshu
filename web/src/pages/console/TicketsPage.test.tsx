import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import type {
  CreatedTicket,
  RequestIDItem,
  RequestIDList,
  Ticket,
  TicketListItem,
  TicketList,
  TicketSLA,
} from '@/api/console'
import { renderWithProviders } from '@/test/renderConsole'
import TicketsPage from './TicketsPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

const REQUEST_ID = '12345678-1234-1234-1234-123456789012'
const TICKET_NO = 'TK202610270001'

function sla(overrides: Partial<TicketSLA> = {}): TicketSLA {
  return {
    first_response_due_at: '2026-10-27T12:00:00Z',
    first_responded_at: null,
    responded: false,
    overdue: false,
    remaining_seconds: 3600,
    overdue_seconds: 0,
    display: '剩余 1 小时',
    ...overrides,
  }
}

function listItem(overrides: Partial<TicketListItem> = {}): TicketListItem {
  return {
    id: 1,
    ticket_no: TICKET_NO,
    ticket_type: 'TECH',
    title: '流式请求偶发中断',
    state: 'OPEN',
    first_response_due_at: '2026-10-27T12:00:00Z',
    first_responded_at: null,
    last_replied_at: null,
    created_at: '2026-10-27T08:00:00Z',
    sla: sla(),
    ...overrides,
  }
}

function ticketList(items: TicketListItem[]): TicketList {
  return { items, page: 1, page_size: 20, total: items.length }
}

function ticketDetail(overrides: Partial<Ticket> = {}): Ticket {
  return {
    id: 1,
    ticket_no: TICKET_NO,
    ticket_type: 'TECH',
    title: '流式请求偶发中断',
    description: '每次重试都在首字节前断开。',
    request_id: REQUEST_ID,
    policy_hit_id: null,
    policy_rule_id: null,
    policy_category: null,
    state: 'RESPONDED',
    first_response_due_at: '2026-10-27T12:00:00Z',
    first_responded_at: '2026-10-27T09:00:00Z',
    last_replied_at: '2026-10-27T09:00:00Z',
    reopen_count: 0,
    created_at: '2026-10-27T08:00:00Z',
    updated_at: '2026-10-27T09:00:00Z',
    sla: sla({
      first_responded_at: '2026-10-27T09:00:00Z',
      responded: true,
      remaining_seconds: 0,
      display: '已响应（用时 1 小时）',
    }),
    replies: [
      {
        id: 1,
        author_type: 'USER',
        content: '第一个提问：重试后仍失败。',
        created_at: '2026-10-27T08:00:00Z',
      },
      {
        id: 2,
        author_type: 'OPS',
        content: '客服答复：已切换路由，请观察。',
        created_at: '2026-10-27T09:00:00Z',
      },
    ],
    ...overrides,
  }
}

const requestItem: RequestIDItem = {
  request_id: REQUEST_ID,
  model_name: 'text-large',
  http_status: 502,
  result: 'FAILED',
  created_at: '2026-10-27T08:00:00Z',
}

function requestIDs(items: RequestIDItem[] = [requestItem]): RequestIDList {
  return {
    items,
    page: 1,
    page_size: 10,
    total: items.length,
    since: '2026-07-29T00:00:00Z',
  }
}

interface MockOptions {
  tickets?: TicketList | ApiError
  ticket?: Ticket | ApiError
  requestIDs?: RequestIDList | ApiError
  created?: CreatedTicket | ApiError
  closed?: Ticket | ApiError
}

function installMock(options: MockOptions = {}) {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/console/request-ids')) {
      if (options.requestIDs instanceof ApiError) return Promise.reject(options.requestIDs)
      return Promise.resolve(options.requestIDs ?? requestIDs())
    }
    if (/^\/api\/console\/tickets\/\d+$/.test(path)) {
      if (options.ticket instanceof ApiError) return Promise.reject(options.ticket)
      return Promise.resolve(options.ticket ?? ticketDetail())
    }
    if (path.startsWith('/api/console/tickets?')) {
      if (options.tickets instanceof ApiError) return Promise.reject(options.tickets)
      return Promise.resolve(
        options.tickets ??
          ticketList([
            listItem({
              state: 'RESPONDED',
              sla: sla({
                responded: true,
                display: '已响应（用时 1 小时）',
                first_responded_at: '2026-10-27T09:00:00Z',
              }),
            }),
          ]),
      )
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })

  mockedPost.mockImplementation((path: string) => {
    if (/^\/api\/console\/tickets\/\d+\/close$/.test(path)) {
      if (options.closed instanceof ApiError) return Promise.reject(options.closed)
      return Promise.resolve(
        options.closed ??
          ticketDetail({
            state: 'CLOSED',
            last_replied_at: null,
            sla: sla({ display: '已关闭（未响应）' }),
          }),
      )
    }
    if (path === '/api/console/tickets') {
      if (options.created instanceof ApiError) return Promise.reject(options.created)
      return Promise.resolve(
        options.created ?? {
          ticket: ticketDetail({ state: 'OPEN' }),
          request_id_source: 'USER',
        },
      )
    }
    return Promise.reject(new Error(`unexpected POST ${path}`))
  })
}

function lastTicketsParams(): URLSearchParams {
  const paths = mockedGet.mock.calls
    .map((call) => call[0] as string)
    .filter((path) => path.startsWith('/api/console/tickets?'))
  return new URL(paths.at(-1) ?? '', 'http://localhost').searchParams
}

function lastRequestIDParams(): URLSearchParams {
  const paths = mockedGet.mock.calls
    .map((call) => call[0] as string)
    .filter((path) => path.startsWith('/api/console/request-ids'))
  return new URL(paths.at(-1) ?? '', 'http://localhost').searchParams
}

async function fillCommonFields(dialog: HTMLElement) {
  const user = userEvent.setup()
  await user.type(within(dialog).getByLabelText(/标题/), '请求一直失败')
  await user.type(within(dialog).getByLabelText('正文'), '同一个请求重试两次都失败。')
  return user
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
})

afterEach(cleanup)

describe('TicketsPage', () => {
  it('F19-1: the picker only sends q/page/page_size and renders backend rows', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(screen.getByRole('button', { name: '新建工单' }))

    const dialog = await screen.findByRole('dialog', { name: '新建工单' })
    await user.selectOptions(within(dialog).getByLabelText(/类型/), 'TECH')

    expect(
      await within(dialog).findByRole('button', { name: `选择请求 ${REQUEST_ID}` }),
    ).toBeInTheDocument()
    expect(within(dialog).getByText(/可选范围自/)).toBeInTheDocument()

    let params = lastRequestIDParams()
    expect(params.get('page')).toBe('1')
    expect(params.get('page_size')).toBe('10')
    expect(params.has('from')).toBe(false)
    expect(params.has('to')).toBe(false)
    expect(params.has('since')).toBe(false)

    await user.type(within(dialog).getByLabelText('搜索可关联请求'), 'req')
    await waitFor(
      () => {
        params = lastRequestIDParams()
        expect(params.get('q')).toBe('req')
        expect(params.has('from')).toBe(false)
        expect(params.has('to')).toBe(false)
      },
      { timeout: 2000 },
    )

    await user.click(
      within(dialog).getByRole('button', { name: `选择请求 ${REQUEST_ID}` }),
    )
    expect(within(dialog).getByText('已选择关联请求')).toBeInTheDocument()
    expect(
      within(dialog).getByRole('button', { name: '清除关联请求' }),
    ).toBeInTheDocument()
  })

  it('F19-1: a rejected linked request shows the backend 404 message and hint', async () => {
    installMock({
      created: new ApiError({
        status: 404,
        type: 'invalid_request_error',
        code: 'resource_not_found',
        message: '资源不存在或已被删除',
        hint: '刷新列表后重试',
      }),
    })
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(screen.getByRole('button', { name: '新建工单' }))
    const dialog = await screen.findByRole('dialog', { name: '新建工单' })
    await user.selectOptions(within(dialog).getByLabelText(/类型/), 'TECH')
    await user.click(
      await within(dialog).findByRole('button', { name: `选择请求 ${REQUEST_ID}` }),
    )
    await fillCommonFields(dialog)
    await user.click(within(dialog).getByRole('button', { name: '提交工单' }))

    expect(await within(dialog).findByText('资源不存在或已被删除')).toBeInTheDocument()
    expect(within(dialog).getByText('刷新列表后重试')).toBeInTheDocument()
  })

  it('F19-2: state and type filters go to the backend; detail shows the backend SLA copy', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.selectOptions(screen.getByLabelText('状态'), 'RESPONDED')
    await user.selectOptions(screen.getByLabelText('类型'), 'TECH')

    await waitFor(() => {
      const params = lastTicketsParams()
      expect(params.get('state')).toBe('RESPONDED')
      expect(params.get('type')).toBe('TECH')
      expect(params.get('page')).toBe('1')
    })

    expect(
      await screen.findByText('已响应（用时 1 小时）'),
    ).toBeInTheDocument()

    await user.click(
      await screen.findByRole('button', { name: `查看工单 ${TICKET_NO}` }),
    )
    const dialog = await screen.findByRole('dialog', { name: '工单详情' })

    expect(await within(dialog).findByText('已响应（用时 1 小时）')).toBeInTheDocument()

    const replies = within(dialog).getByRole('list', { name: '往来记录列表' })
    const items = within(replies).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    expect(within(items[0]).getByText('用户')).toBeInTheDocument()
    expect(within(items[0]).getByText('第一个提问：重试后仍失败。')).toBeInTheDocument()
    expect(within(items[1]).getByText('客服')).toBeInTheDocument()
    expect(within(items[1]).getByText('客服答复：已切换路由，请观察。')).toBeInTheDocument()
  })

  it('marks an overdue ticket with the SLA warning badge', async () => {
    installMock({
      tickets: ticketList([
        listItem({
          sla: sla({
            overdue: true,
            remaining_seconds: 0,
            overdue_seconds: 7200,
            display: '已超时 2 小时',
          }),
        }),
      ]),
    })
    renderWithProviders(<TicketsPage />)

    expect(await screen.findByText('处理超时')).toBeInTheDocument()
    expect(screen.getByText('已超时 2 小时')).toBeInTheDocument()
  })

  it('F19-3: content appeals have no rule-ID input and show the derived values after 201', async () => {
    installMock({
      created: {
        ticket: ticketDetail({
          ticket_type: 'CONTENT_APPEAL',
          state: 'OPEN',
          policy_hit_id: 99,
          policy_rule_id: 'kw_violence_v3',
          policy_category: 'VIOLENCE',
          request_id: REQUEST_ID,
        }),
        request_id_source: 'POLICY_HIT',
        notice: '已按命中的违规记录校正关联 request_id（以命中记录为准）',
      },
    })
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(screen.getByRole('button', { name: '新建工单' }))
    const dialog = await screen.findByRole('dialog', { name: '新建工单' })
    await user.selectOptions(within(dialog).getByLabelText(/类型/), 'CONTENT_APPEAL')

    expect(within(dialog).getByLabelText('关联请求 ID')).toBeInTheDocument()
    expect(within(dialog).queryByLabelText(/规则 ID/)).not.toBeInTheDocument()
    expect(within(dialog).queryByLabelText('搜索可关联请求')).not.toBeInTheDocument()

    await user.type(within(dialog).getByLabelText('关联请求 ID'), REQUEST_ID)
    await fillCommonFields(dialog)
    await user.click(within(dialog).getByRole('button', { name: '提交工单' }))

    expect(await within(dialog).findByText('kw_violence_v3')).toBeInTheDocument()
    expect(within(dialog).getByText('VIOLENCE')).toBeInTheDocument()
    expect(
      within(dialog).getByText(
        '已按命中的违规记录校正关联 request_id（以命中记录为准）',
      ),
    ).toBeInTheDocument()

    const [, body] = mockedPost.mock.calls[0]
    expect(body).toMatchObject({
      ticket_type: 'CONTENT_APPEAL',
      request_id: REQUEST_ID,
      title: '请求一直失败',
    })
    expect(body).not.toHaveProperty('policy_rule_id')
    expect(body).not.toHaveProperty('policy_hit_id')
  })

  it('closes a ticket after an inline second confirmation', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(
      await screen.findByRole('button', { name: `查看工单 ${TICKET_NO}` }),
    )
    const dialog = await screen.findByRole('dialog', { name: '工单详情' })

    await user.click(await within(dialog).findByRole('button', { name: '关闭工单' }))
    await user.click(within(dialog).getByRole('button', { name: '确认关闭' }))

    await waitFor(() => {
      expect(
        within(dialog).queryByRole('button', { name: '关闭工单' }),
      ).not.toBeInTheDocument()
    })
    expect(mockedPost).toHaveBeenCalledWith('/api/console/tickets/1/close', {})
  })

  it('shows the backend 409 message and hint when closing an already closed ticket', async () => {
    installMock({
      closed: new ApiError({
        status: 409,
        type: 'conflict_error',
        code: 'resource_state_conflict',
        message: '当前状态不允许该操作：工单已关闭',
        hint: '刷新后重试；若单据状态异常请联系客服',
      }),
    })
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(
      await screen.findByRole('button', { name: `查看工单 ${TICKET_NO}` }),
    )
    const dialog = await screen.findByRole('dialog', { name: '工单详情' })

    await user.click(await within(dialog).findByRole('button', { name: '关闭工单' }))
    await user.click(within(dialog).getByRole('button', { name: '确认关闭' }))

    expect(
      await within(dialog).findByText('当前状态不允许该操作：工单已关闭'),
    ).toBeInTheDocument()
    expect(
      within(dialog).getByText('刷新后重试；若单据状态异常请联系客服'),
    ).toBeInTheDocument()
  })

  it('F19-4: does not invent an outstanding-ticket notice the API never returns', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<TicketsPage />)

    await screen.findByRole('table', { name: '工单列表' })
    await user.click(
      await screen.findByRole('button', { name: `查看工单 ${TICKET_NO}` }),
    )
    const dialog = await screen.findByRole('dialog', { name: '工单详情' })
    await within(dialog).findByText('第一个提问：重试后仍失败。')

    expect(within(dialog).queryByText(/未结工单|无法注销/)).not.toBeInTheDocument()
  })
})
