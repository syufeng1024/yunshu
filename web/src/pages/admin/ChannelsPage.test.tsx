import { cleanup, configure, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type { Channel, ChannelListResponse } from '@/api/admin'
import { renderWithProviders } from '@/test/renderConsole'
import ChannelsPage, { upsertModelMapLine } from './ChannelsPage'

// findBy*/waitFor 默认只等 1s。这台机器上经常同时跑别人的 Playwright + 十几个 chrome，
// 1s 会偶发超时（实测：正常 5.5s 跑完，被浏览器占满时 19.7s，其中 3 条因等待超时而红，
// 重跑全绿）。放宽到 3s，别让"机器忙"伪装成"功能坏"。
// 这是机器争抢的补偿，不是放宽功能断言：真回归仍然会红（元素根本不在/条件不成立时，
// 只是最多晚 3s 才报出来），判定结果不变；看耗时变长时别误判成性能退化。
configure({ asyncUtilTimeout: 3000 })

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPatch = vi.mocked(api.patch)
const mockedPost = vi.mocked(api.post)

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

/** 新建成功后的回包。id=2，与列表里的渠道区分开，便于断言后续详情是按新 id 取的。 */
const createdChannel: Channel = {
  ...channel,
  id: 2,
  name: '备线-新渠',
  upstream_key_mask: 'sk-ne****a1b2',
  upstream_model_name_map: {},
}

const emptyAudit = {
  items: [],
  page: 1,
  page_size: 50,
  total: 0,
  from: '2026-10-21T00:00:00Z',
  to: '2026-10-28T00:00:00Z',
}

function installMock(options: { detailPendingId?: number } = {}) {
  mockedGet.mockImplementation((path: string) => {
    // 故意不回：详情 GET 若悬着，没预填缓存时抽屉正文会长期停在
    // 「正在读取渠道详情…」，下面那条用例就能抓到"表单被替换"。
    if (options.detailPendingId !== undefined &&
      path === `/api/admin/channels/${options.detailPendingId}`) {
      return new Promise<never>(() => {})
    }
    if (path.startsWith('/api/admin/channels?')) return Promise.resolve(list)
    if (path === '/api/admin/channels/1') return Promise.resolve({ channel })
    if (path === '/api/admin/channels/2') return Promise.resolve({ channel: createdChannel })
    if (path.startsWith('/api/admin/audit-logs?')) return Promise.resolve(emptyAudit)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
  mockedPatch.mockResolvedValue({ channel })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPatch.mockReset()
  mockedPost.mockReset()
})

afterEach(cleanup)

/** 新建时填的明文，用来在整个页面里搜「明文有没有露出来」。 */
const CREATED_KEY = 'sk-new-secret-0001'

/**
 * 走一遍「新建渠道 → 填最小必填项 → 保存」，停在切好的编辑态上。
 * 后几条用例共用这段，避免每条都抄一遍表单填写。
 */
async function createAndSave(user: ReturnType<typeof userEvent.setup>) {
  renderWithProviders(<ChannelsPage />)
  await screen.findByText('百炼-主线路')
  await user.click(screen.getByRole('button', { name: '新建渠道' }))

  await user.type(screen.getByLabelText(/渠道名称/), '备线-新渠')
  await user.type(screen.getByLabelText(/协议编号/), '1')
  await user.type(screen.getByLabelText(/接入地址/), 'https://bailian.example.com/v1')
  await user.type(screen.getByLabelText(/上游密钥/), CREATED_KEY)

  await user.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(mockedPost).toHaveBeenCalledTimes(1))
  await screen.findByRole('heading', { name: '编辑渠道' })
}

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

  // 「从上游获取模型名」要用渠道自己的密钥去查上游，新建态还没有 id。按钮只能是灰的，
  // 但灰按钮不派发鼠标事件、title 弹不出来，理由必须能在页面上读到。
  it('ChannelsPage：新建态的上游模型名按钮给出可见理由', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<ChannelsPage />)

    await screen.findByText('百炼-主线路')
    await user.click(screen.getByRole('button', { name: '新建渠道' }))

    expect(screen.getByRole('heading', { name: '新建渠道' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '从上游获取模型名' })).toBeDisabled()
    expect(screen.getByText(/新建渠道需先保存一次/)).toBeInTheDocument()
  })

  it('ChannelsPage：保存成功后留在抽屉里，上游模型名按钮就地可用', async () => {
    // 详情 GET 悬着不回：保存后若没有预填详情缓存，正文会被「正在读取渠道详情…」
    // 长期占位，下面的断言就抓得到。这是"表单不被替换"的反证条件。
    installMock({ detailPendingId: 2 })
    mockedPost.mockResolvedValue({ channel: createdChannel })
    const user = userEvent.setup()
    await createAndSave(user)

    expect(mockedPost.mock.calls[0]?.[1]).toMatchObject({
      name: '备线-新渠',
      upstream_key: CREATED_KEY,
    })
    // 新建走 POST，不该顺手又发一次 PATCH。
    expect(mockedPatch).not.toHaveBeenCalled()

    // 切编辑态不能把刚填好的表单整片换成加载文案（详情 GET 仍悬着，缓存已预填）。
    expect(screen.queryByText(/正在读取渠道详情/)).not.toBeInTheDocument()
    expect(screen.getByLabelText(/渠道名称/)).toHaveValue('备线-新渠')

    // 按钮就地可用，说明文字消失；密钥区已回到掩码（明文不再显示）。
    const fetchModels = await screen.findByRole('button', { name: '从上游获取模型名' })
    await waitFor(() => expect(fetchModels).toBeEnabled())
    expect(screen.queryByText(/新建渠道需先保存一次/)).not.toBeInTheDocument()
    expect(screen.getByTestId('upstream-key-mask')).toHaveTextContent('sk-ne****a1b2')
  })

  // 改造「留在抽屉」时最容易出两件事：切编辑态时指向了错的渠道，以及顺手删掉 create
  // 分支的 openAudit（删掉它没有任何别的用例会变红，审计打不开是静默缺陷）。
  // 下面两条分别盯住：二次保存打到刚建的渠道 id 上、且不回传明文；审计请求带着新渠道 id。
  // 注意这里**没有**断言"表单 state 里没有明文"——那件事不可观测，见 ChannelsPage.tsx
  // create 分支里 updateField('upstreamKey', '') 处的注释。
  it('ChannelsPage：保存成功后二次保存指向新渠道且不回传密钥', async () => {
    installMock()
    mockedPost.mockResolvedValue({ channel: createdChannel })
    const user = userEvent.setup()
    await createAndSave(user)

    expect(screen.getByTestId('upstream-key-mask')).toHaveTextContent('sk-ne****a1b2')

    // 「重新填写」展开后拿到的是空白输入框——这一条由按钮自己的 onClick 清空保证，
    // 与 create 分支里清 state 的那行无关（那行见下一条注释）。
    await user.click(screen.getByRole('button', { name: '重新填写' }))
    expect(screen.getByLabelText(/新的上游密钥/)).toHaveValue('')
    // 整页扫 input：只能证明"当前没有把明文渲染到 DOM"，**不能**证明 state 里没有明文。
    // 真正清 state 的是 create 分支里的 updateField('upstreamKey', '')，它在 DOM 与请求
    // 体上都不可观测，没有用例钉得住（见 ChannelsPage.tsx 该处注释）。这一扫是给"编辑态
    // 开始挂载明文输入框"那类改动留的下限保护——该改动的具体触发条件与届时必须补的断言，
    // 也写在 ChannelsPage.tsx 那处注释里。
    const values = Array.from(document.querySelectorAll('input')).map((input) => input.value)
    expect(values.some((value) => value.includes(CREATED_KEY))).toBe(false)

    // 回到「留空即保留」：二次保存打到新渠道 id 上，且不再把明文传一遍。
    await user.click(screen.getByRole('button', { name: '取消重新填写' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    expect(String(mockedPatch.mock.calls[0]?.[0])).toBe('/api/admin/channels/2')
    expect(mockedPatch.mock.calls[0]?.[1]).not.toHaveProperty('upstream_key')
  })

  it('ChannelsPage：保存成功后审计按新渠道 id 拉取', async () => {
    installMock()
    mockedPost.mockResolvedValue({ channel: createdChannel })
    const user = userEvent.setup()
    await createAndSave(user)

    const auditPaths = () =>
      mockedGet.mock.calls
        .map(([path]) => String(path))
        .filter((path) => path.startsWith('/api/admin/audit-logs?'))

    await waitFor(() => {
      const calls = auditPaths()
      const latest = calls[calls.length - 1]
      if (!latest) throw new Error('保存成功后没有拉审计记录')
      expect(latest).toContain('target_type=t_channel')
      expect(latest).toContain('target_id=2')
    })
  })
})

// 「从上游获取模型名」写回表单的规则。选择器给的是两个确定的字符串，唯一要保证的
// 是：同名覆盖而不是追加出两条互相矛盾的映射，其余行原样保留。
describe('模型名映射写入规则', () => {
  it('同名覆盖，其余行顺序不变', () => {
    expect(upsertModelMapLine('a=up-a\nb=up-b', 'a', 'up-a2')).toBe('a=up-a2\nb=up-b')
  })

  it('新模型追加到末尾', () => {
    expect(upsertModelMapLine('a=up-a', 'c', 'up-c')).toBe('a=up-a\nc=up-c')
  })

  it('全角等号与空行都被归一，不会留下空映射', () => {
    expect(upsertModelMapLine('  a ＝ up-a  \n\n', 'b', 'up-b')).toBe('a=up-a\nb=up-b')
  })

  it('同一个键只保留一条', () => {
    expect(upsertModelMapLine('a=1\na=2', 'a', '3')).toBe('a=3')
  })
})
