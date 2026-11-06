import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type { AdminModel } from '@/api/admin'
import { ToastViewport } from '@/components'
import { useToastStore } from '@/stores/toast'
import { renderWithProviders } from '@/test/renderConsole'
import AdminModelsPage from './ModelsPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)
const mockedPatch = vi.mocked(api.patch)

const pricing: AdminModel['pricing'] = {
  price_rule_id: 701,
  version: 3,
  effective_from: '2026-10-01T00:00:00Z',
  in_price_micro_per_k: 1000,
  out_price_micro_per_k: 3000,
  per_call_price_micro: 0,
}

function model(overrides: Partial<AdminModel> = {}): AdminModel {
  return {
    id: 12,
    model_name: 'deepseek-chat',
    display_name: 'DeepSeek Chat',
    region: 'DOMESTIC',
    model_type: 'TEXT',
    protocol_id: 2,
    protocol: 'OPENAI_COMPAT',
    primary_channel_id: 2,
    context_length: 65536,
    max_output_tokens: 4096,
    capability_mask: 1,
    capabilities: { stream: true, tool_call: false, vision: false, json_mode: null },
    listing_state: 'CATALOG_ONLY',
    recommend_weight: 0,
    aliases: [],
    pricing: null,
    created_at: '2026-08-20T02:00:00Z',
    updated_at: '2026-10-18T02:00:00Z',
    ...overrides,
  }
}

const unpricedModel = model()

const catalogPriced = model({
  id: 14,
  model_name: 'catalog-priced',
  display_name: '待上架模型',
  listing_state: 'CATALOG_ONLY',
  pricing,
})

const delistedPriced = model({
  id: 15,
  model_name: 'delisted-priced',
  display_name: '已下架模型',
  listing_state: 'DELISTED',
  pricing,
})

const listedModel = model({
  id: 16,
  model_name: 'listed-model',
  display_name: '已上架模型',
  listing_state: 'LISTED',
  recommend_weight: 100,
  pricing,
})

function installGet(items: AdminModel[]) {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/admin/models?')) {
      return Promise.resolve({
        items,
        page: 1,
        page_size: 20,
        total: items.length,
      })
    }
    if (path.startsWith('/api/admin/audit-logs?')) {
      return Promise.resolve({
        items: [],
        page: 1,
        page_size: 50,
        total: 0,
        from: '2026-10-21T00:00:00Z',
        to: '2026-10-28T00:00:00Z',
      })
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

function lastModelsQuery(): URLSearchParams {
  const calls = mockedGet.mock.calls
    .map((call) => call[0] as string)
    .filter((path) => path.startsWith('/api/admin/models?'))
  return new URL(calls.at(-1) ?? '', 'http://localhost').searchParams
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
  mockedPatch.mockReset()
  useToastStore.setState({ toasts: [] })
  installGet([unpricedModel])
  mockedPost.mockResolvedValue({ model: listedModel })
  mockedPatch.mockResolvedValue({ model: listedModel })
})

afterEach(cleanup)

describe('ModelsPage', () => {
  it('ModelsPage：未配价上架给出前置提示', async () => {
    renderWithProviders(<AdminModelsPage />)

    await screen.findByText('DeepSeek Chat')
    expect(screen.getByTestId('pricing-required-hint')).toHaveTextContent(
      '先配置价格才能上架',
    )
    expect(screen.getByRole('button', { name: '上架' })).toBeDisabled()
  })

  it('ModelsPage：上下架成功提示公告草稿，DELISTED 不谎报', async () => {
    installGet([catalogPriced, delistedPriced])
    const user = userEvent.setup()
    renderWithProviders(
      <>
        <AdminModelsPage />
        <ToastViewport />
      </>,
    )
    await screen.findByText('待上架模型')

    const firstTable = screen.getByRole('table', { name: '模型主数据列表' })
    const catalogRow = within(firstTable).getByText('待上架模型').closest('tr')
    await user.click(within(catalogRow as HTMLElement).getByRole('button', { name: '上架' }))
    expect(await screen.findByText('已生成待发布公告（草稿）')).toBeInTheDocument()

    const secondTable = screen.getByRole('table', { name: '模型主数据列表' })
    const delistedRow = within(secondTable).getByText('已下架模型').closest('tr')
    await user.click(within(delistedRow as HTMLElement).getByRole('button', { name: '上架' }))

    expect(await screen.findByText('上架状态已更新')).toBeInTheDocument()
    expect(screen.getByText('该状态转换不生成公告草稿。')).toBeInTheDocument()
    expect(screen.queryAllByText('已生成待发布公告（草稿）').length).toBeLessThanOrEqual(1)
  })

  it('F22-4: 来源地 / 类型 / 上架状态各自把对应 query 发给列表接口', async () => {
    installGet([listedModel, catalogPriced, delistedPriced])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('已上架模型')

    await user.selectOptions(screen.getByLabelText('来源地'), 'OVERSEAS')
    await waitFor(() => expect(lastModelsQuery().get('region')).toBe('OVERSEAS'))
    expect(lastModelsQuery().get('page')).toBe('1')

    await user.selectOptions(screen.getByLabelText('类型'), 'IMAGE')
    await waitFor(() => expect(lastModelsQuery().get('model_type')).toBe('IMAGE'))
    expect(lastModelsQuery().get('region')).toBe('OVERSEAS')

    await user.selectOptions(screen.getByLabelText('上架状态'), 'CATALOG_ONLY')
    await waitFor(() => expect(lastModelsQuery().get('listing_state')).toBe('CATALOG_ONLY'))
    expect(lastModelsQuery().get('model_type')).toBe('IMAGE')
    expect(lastModelsQuery().get('region')).toBe('OVERSEAS')
  })

  it('F22-4: 仅目录可见 行使用独立的上架状态标识', async () => {
    installGet([catalogPriced, listedModel, delistedPriced])
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('待上架模型')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const catalogRow = within(table).getByText('待上架模型').closest('tr') as HTMLElement
    const listedRow = within(table).getByText('已上架模型').closest('tr') as HTMLElement
    const delistedRow = within(table).getByText('已下架模型').closest('tr') as HTMLElement

    expect(within(catalogRow).getByText('仅目录可见')).toHaveClass('bg-warning-soft')
    expect(within(listedRow).getByText('已上架')).toHaveClass('bg-success-soft')
    expect(within(delistedRow).getByText('已下架')).toHaveClass('bg-surface-muted')
    expect(within(listedRow).queryByText('仅目录可见')).not.toBeInTheDocument()
  })

  it('ModelsPage：推荐权重负数或超上限在字段上拦截', async () => {
    installGet([listedModel])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('已上架模型')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const row = within(table).getByText('已上架模型').closest('tr')
    await user.click(within(row as HTMLElement).getByRole('button', { name: '推荐权重' }))

    const input = await screen.findByLabelText('推荐权重')
    await user.clear(input)
    await user.type(input, '-1')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('推荐权重不能为负数。')).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()

    await user.clear(input)
    await user.type(input, '4294967296')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(
      await screen.findByText(/推荐权重不能超过 4294967295/),
    ).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()
  })

  // T46: max_output_tokens 是模型自身的输出上限（0 = 未配置）。列表要能看到它，
  // 编辑弹窗要能改它，提交体要带上它，超上下文长度要在字段上拦截。
  it('T46: 列表展示输出上限，编辑弹窗可改并随提交体下发', async () => {
    installGet([unpricedModel])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('DeepSeek Chat')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const row = within(table).getByText('DeepSeek Chat').closest('tr') as HTMLElement
    expect(within(row).getByText('4,096 tokens')).toBeInTheDocument()

    await user.click(within(row).getByRole('button', { name: '编辑' }))
    const input = await screen.findByLabelText('输出上限（tokens）')
    expect(input).toHaveValue('4096')

    await user.clear(input)
    await user.type(input, '2048')
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    const [path, body] = mockedPatch.mock.calls[0]
    expect(path).toBe('/api/admin/models/12')
    expect(body).toMatchObject({ max_output_tokens: 2048, context_length: 65536 })
  })

  it('T46: 输出上限超过上下文长度时在字段上拦截', async () => {
    installGet([unpricedModel])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('DeepSeek Chat')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const row = within(table).getByText('DeepSeek Chat').closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '编辑' }))

    const input = await screen.findByLabelText('输出上限（tokens）')
    await user.clear(input)
    await user.type(input, '70000')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(
      await screen.findByText('输出上限不能超过上下文长度（当前 65536）。'),
    ).toBeInTheDocument()
    expect(mockedPatch).not.toHaveBeenCalled()
  })

  // T48: context_length 与 max_output_tokens 是一对，反方向也要拦。把上下文改小
  // 到存量输出上限之下、又没动输出上限，库里那一行就自相矛盾（网关估算会按存量
  // 上限预留，超出模型真实产出）。后端会 400 并指向 context_length，前端先拦一次，
  // 运营不必提交后才吃到错误；文案带具体数值，与后端同口径。
  it('T48: 只下调上下文到存量输出上限以下时在上下文字段拦截', async () => {
    installGet([unpricedModel])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('DeepSeek Chat')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const row = within(table).getByText('DeepSeek Chat').closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '编辑' }))

    const contextInput = await screen.findByLabelText('上下文长度（tokens）')
    await user.clear(contextInput)
    await user.type(contextInput, '100')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(
      await screen.findByText(/上下文长度不能小于当前输出上限（4096）/),
    ).toBeInTheDocument()
    // 拦截，不发请求。
    expect(mockedPatch).not.toHaveBeenCalled()
  })

  // T48: 两个字段一起改是合法的修复路径，必须放行，且同一次提交里带上两个值。
  it('T48: 上下文与输出上限一起下调时放行并同批下发', async () => {
    installGet([unpricedModel])
    const user = userEvent.setup()
    renderWithProviders(<AdminModelsPage />)
    await screen.findByText('DeepSeek Chat')

    const table = screen.getByRole('table', { name: '模型主数据列表' })
    const row = within(table).getByText('DeepSeek Chat').closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '编辑' }))

    const contextInput = await screen.findByLabelText('上下文长度（tokens）')
    const outputInput = await screen.findByLabelText('输出上限（tokens）')
    await user.clear(contextInput)
    await user.type(contextInput, '100')
    await user.clear(outputInput)
    await user.type(outputInput, '100')
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    const [path, body] = mockedPatch.mock.calls[0]
    expect(path).toBe('/api/admin/models/12')
    expect(body).toMatchObject({ context_length: 100, max_output_tokens: 100 })
  })
})
