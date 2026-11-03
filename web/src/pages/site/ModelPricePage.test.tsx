import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { usePublicModels } from '@/api/public'
import type { PublicModel } from '@/api/public'
import ModelPricePage from './ModelPricePage'

vi.mock('@/api/public', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/public')>()
  return { ...actual, usePublicModels: vi.fn() }
})

const mockModels = vi.mocked(usePublicModels)

function model(overrides: Partial<PublicModel> = {}): PublicModel {
  return {
    id: '1',
    model_name: 'deepseek-v4',
    display_name: 'DeepSeek V4',
    model_type: 'TEXT',
    listing_state: 'LISTED',
    context_window: 128_000,
    input_price_micro_per_k: 1_500,
    output_price_micro_per_k: 6_000,
    protocols: ['OPENAI'],
    recommend_weight: 100,
    published_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function install(models: PublicModel[]) {
  mockModels.mockReturnValue({
    data: { data: models },
    isLoading: false,
    isError: false,
    error: null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof usePublicModels>)
}

beforeEach(() => {
  vi.clearAllMocks()
})

function renderPage() {
  return render(
    <MemoryRouter>
      <ModelPricePage />
    </MemoryRouter>,
  )
}

describe('ModelPricePage', () => {
  it('shows each model with its input/output unit price and context window', () => {
    install([model(), model({ id: '2', model_name: 'glm-5', display_name: 'GLM-5', model_type: 'IMAGE' })])
    renderPage()

    expect(screen.getByText('DeepSeek V4')).toBeInTheDocument()
    expect(screen.getByText('deepseek-v4')).toBeInTheDocument()
    // MICRO_PER_CREDIT = 10_000, so 1_500 micro per 1k = 0.15 credits per 1k
    expect(screen.getAllByText('0.15 积分/千 token').length).toBeGreaterThan(0)
    expect(screen.getByText('共 2 个模型')).toBeInTheDocument()
  })

  it('filters by keyword and by type', async () => {
    const user = userEvent.setup()
    install([
      model(),
      model({ id: '2', model_name: 'glm-5', display_name: 'GLM-5', model_type: 'IMAGE' }),
    ])
    renderPage()

    await user.type(screen.getByLabelText('搜索模型'), 'glm')
    expect(screen.queryByText('DeepSeek V4')).not.toBeInTheDocument()
    expect(screen.getByText('GLM-5')).toBeInTheDocument()

    await user.clear(screen.getByLabelText('搜索模型'))
    await user.click(screen.getByRole('button', { name: '图像' }))
    expect(screen.queryByText('DeepSeek V4')).not.toBeInTheDocument()
    expect(screen.getByText('GLM-5')).toBeInTheDocument()
  })

  it('says so explicitly when nothing is listed', () => {
    install([])
    renderPage()
    expect(screen.getByText('没有匹配的模型')).toBeInTheDocument()
    expect(screen.getByText('当前没有已上架的模型。')).toBeInTheDocument()
  })
})
