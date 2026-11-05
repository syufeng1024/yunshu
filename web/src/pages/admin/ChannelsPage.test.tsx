import { cleanup, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type { Channel, ChannelListResponse } from '@/api/admin'
import { renderWithProviders } from '@/test/renderConsole'
import ChannelsPage from './ChannelsPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPatch = vi.mocked(api.patch)

const channel: Channel = {
  id: 1,
  name: '百炼-主线路',
  protocol_id: 1,
  protocol: 'BAILIAN',
  region: 'DOMESTIC',
  base_url: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
  upstream_key_mask: 'sk-ab****3f9c',
  group_scope_mode: 'ALL',
  dedicated_scope_mode: 'ALL',
  fallback_allowed: true,
  weight: 100,
  priority: 100,
  timeout_connect_ms: 3000,
  timeout_first_byte_ms: 60000,
  timeout_total_ms: 120000,
  retry_max: 2,
  stream_supported: true,
  stream_usage_mode: 'NATIVE',
  upstream_model_name_map: { 'qwen-max': 'qwen-max' },
  health_state: 'CLOSED',
  success_rate_bp: 9850,
  enabled: true,
  created_at: '2026-08-01T02:00:00Z',
  updated_at: '2026-10-20T06:30:00Z',
}

const list: ChannelListResponse = {
  items: [channel],
  page: 1,
  page_size: 20,
  total: 1,
}

const emptyAudit = {
  items: [],
  page: 1,
  page_size: 50,
  total: 0,
  from: '2026-10-21T00:00:00Z',
  to: '2026-10-28T00:00:00Z',
}

function installMock() {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/admin/channels?')) return Promise.resolve(list)
    if (path === '/api/admin/channels/1') return Promise.resolve({ channel })
    if (path.startsWith('/api/admin/audit-logs?')) return Promise.resolve(emptyAudit)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
  mockedPatch.mockResolvedValue({ channel })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPatch.mockReset()
})

afterEach(cleanup)

describe('ChannelsPage', () => {
  it('ChannelsPage：编辑不回填上游 Key 明文', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<ChannelsPage />)

    await screen.findByText('百炼-主线路')
    expect(screen.getByText('98.5%')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '编辑' }))

    const mask = await screen.findByTestId('upstream-key-mask')
    expect(mask).toHaveTextContent('sk-ab****3f9c')
    expect(screen.getByRole('button', { name: '重新填写' })).toBeInTheDocument()

    // No plaintext input exists until 「重新填写」 is clicked.
    expect(screen.queryByLabelText(/新的上游密钥/)).not.toBeInTheDocument()
    const inputValues = Array.from(document.querySelectorAll('input')).map(
      (input) => (input as HTMLInputElement).value,
    )
    expect(inputValues.some((value) => value.startsWith('sk-'))).toBe(false)

    // Saving without re-entering the key must omit the field entirely.
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    const body = mockedPatch.mock.calls[0]?.[1] as Record<string, unknown>
    expect(body).not.toHaveProperty('upstream_key')
    expect(body).not.toHaveProperty('enabled')
    expect(body.weight).toBe(100)
  })
})
