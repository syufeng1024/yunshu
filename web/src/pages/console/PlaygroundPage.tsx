import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import {
  playgroundChat,
  playgroundCreditsRemaining,
  useConsoleModels,
  useConsoleOverview,
  useKeys,
} from '@/api/console'
import type { KeyListParams, KeyListItem } from '@/api/console'
import {
  Badge,
  Button,
  CopyBlock,
  ErrorState,
  Icon,
  Input,
  PageHeader,
  Select,
} from '@/components'
import { ModelPicker } from '@/components/console'
import { resolveApiBaseUrl } from '@/lib/apiBase'
import { cn } from '@/lib/cn'
import { usePageTitle } from '@/lib/usePageTitle'

/**
 * 在线调试台（对话式单栏）。
 *
 * 结构：工具条（密钥 / 模型 / 高级参数折叠）→ 计费与快捷键提示 → 对话区
 * （flex-1 内部滚动）→ 底部 composer（Enter 发送、Shift+Enter 换行）→ 默认收起
 * 的「请求预览与调用代码」。
 *
 * 两处刻意的取舍，别当成漏做：
 *  1. **不再提供逐条消息的 role 下拉和单条删除**。这里是「一条条聊」的对话台，
 *     不是请求体编辑器；需要拼 few-shot 的用户应该复制「调用代码」去自己的脚本里
 *     改。整段「清空对话」是唯一的撤回手段。
 *  2. **空输入不允许发送**（`发送` 在 composer trim 后为空时禁用），但「无可用
 *     密钥」「余额不足」的提示优先级仍排在输入判断之前——否则空输入会把真正的
 *     阻断原因盖掉。
 */

type MessageRole = 'user' | 'assistant'

/** One rendered turn of the conversation, in send order. */
interface PlaygroundMessage {
  id: string
  role: MessageRole
  content: string
}

interface Example {
  id: string
  title: string
  system: string
  prompt: string
  temperature: string
  maxTokens: string
}

type CodeLanguage = 'curl' | 'python' | 'node'
type ReplyState = 'streaming' | 'done' | 'aborted'

/** Tracks the turn that is currently streaming so only it shows the caret. */
interface AssistantReply {
  id: string
  state: ReplyState
}

/** The selected key list: one page is enough for the debug console. */
const KEY_LIST_PARAMS: KeyListParams = { page: 1, pageSize: 50 }

/** Composer stops growing after ~6 lines; beyond that it scrolls. */
const COMPOSER_MAX_HEIGHT = 144

/**
 * Virtual prompts for guidance (F12-2): clicking one only fills the composer
 * and the advanced parameters. It never sends a request and never bills.
 */
const EXAMPLES: readonly Example[] = [
  {
    id: 'summary',
    title: '总结技术文档',
    system: '你是一名技术编辑，回答使用简体中文，先给结论。',
    prompt: '请把下面的技术文档总结成三条要点：\n',
    temperature: '0.3',
    maxTokens: '512',
  },
  {
    id: 'error',
    title: '排查接口错误',
    system: '你是一名后端工程师，只输出可执行的排查步骤。',
    prompt: '请解释下面这个接口错误码的可能原因，并给出排查步骤：\n',
    temperature: '0.2',
    maxTokens: '800',
  },
  {
    id: 'copy',
    title: '写产品文案',
    system: '你是一名产品文案，语气克制，不使用夸张形容词。',
    prompt: '请为「请求日志」功能写一段面向开发者的介绍：',
    temperature: '0.7',
    maxTokens: '400',
  },
]

const CODE_LANGUAGE_OPTIONS = [
  { value: 'curl', label: 'curl' },
  { value: 'python', label: 'Python' },
  { value: 'node', label: 'Node' },
]

let messageSequence = 0

function newMessage(role: MessageRole, content = ''): PlaygroundMessage {
  messageSequence += 1
  return { id: `playground-message-${messageSequence}`, role, content }
}

function isAbortError(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    (error as { name?: unknown }).name === 'AbortError'
  )
}

function isUsableKey(key: KeyListItem): boolean {
  return key.status === 'ENABLED' && !key.expired
}

/**
 * Builds the sample snippet for direct gateway calls. The key is replaced by
 * the server-issued mask; no code path in this page ever holds a plaintext key.
 */
function buildSnippet(
  language: CodeLanguage,
  endpoint: string,
  maskedKey: string,
  payload: Record<string, unknown>,
): string {
  const json = JSON.stringify(payload, null, 2)
  if (language === 'curl') {
    // Single-quoted shell strings cannot contain a bare quote.
    const shellJson = json.replace(/'/g, `'\\''`)
    return [
      `curl -N ${endpoint} \\`,
      `  -H "Authorization: Bearer ${maskedKey}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '${shellJson}'`,
    ].join('\n')
  }
  if (language === 'python') {
    // json.loads keeps the payload valid when messages contain quotes or
    // newlines; the quoted literal is produced by JSON.stringify.
    return [
      'import json',
      'import requests',
      '',
      `payload = json.loads(${JSON.stringify(json)})`,
      '',
      'response = requests.post(',
      `    "${endpoint}",`,
      '    headers={',
      `        "Authorization": "Bearer ${maskedKey}",`,
      '        "Content-Type": "application/json",',
      '    },',
      '    data=json.dumps(payload).encode("utf-8"),',
      '    stream=True,',
      ')',
      'for line in response.iter_lines(decode_unicode=True):',
      '    if line:',
      '        print(line)',
    ].join('\n')
  }
  return [
    `const payload = ${json}`,
    '',
    `const response = await fetch("${endpoint}", {`,
    '  method: "POST",',
    '  headers: {',
    `    Authorization: "Bearer ${maskedKey}",`,
    '    "Content-Type": "application/json",',
    '  },',
    '  body: JSON.stringify(payload),',
    '})',
    '',
    'const reader = response.body.getReader()',
    'const decoder = new TextDecoder()',
    'for (;;) {',
    '  const { done, value } = await reader.read()',
    '  if (done) break',
    '  process.stdout.write(decoder.decode(value, { stream: true }))',
    '}',
  ].join('\n')
}

export default function PlaygroundPage() {
  usePageTitle('在线调试台')

  const queryClient = useQueryClient()

  const [model, setModel] = useState('')
  const [system, setSystem] = useState('')
  const [messages, setMessages] = useState<PlaygroundMessage[]>([])
  const [temperature, setTemperature] = useState('0.7')
  const [maxTokens, setMaxTokens] = useState('1024')
  const [stream, setStream] = useState(true)

  const [keyId, setKeyId] = useState<number | null>(null)
  const [draft, setDraft] = useState('')
  const [reply, setReply] = useState<AssistantReply | null>(null)
  const [pending, setPending] = useState(false)
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [devOpen, setDevOpen] = useState(false)
  const [sendError, setSendError] = useState<string | null>(null)
  const [sendHint, setSendHint] = useState<string | null>(null)
  const [insufficientError, setInsufficientError] = useState(false)
  const [creditsUnknown, setCreditsUnknown] = useState(false)
  const [lastRemaining, setLastRemaining] = useState<number | null>(null)
  const [codeLanguage, setCodeLanguage] = useState<CodeLanguage>('curl')

  const abortRef = useRef<AbortController | null>(null)
  const logRef = useRef<HTMLDivElement | null>(null)
  const composerRef = useRef<HTMLTextAreaElement | null>(null)

  const models = useConsoleModels()
  const overview = useConsoleOverview({ poll: false })
  const keys = useKeys(KEY_LIST_PARAMS)

  useEffect(() => {
    if (!model && models.data && models.data.length > 0) {
      setModel(models.data[0].model_name)
    }
  }, [model, models.data])

  const keyItems = useMemo(() => keys.data?.items ?? [], [keys.data])
  const usableKeys = useMemo(() => keyItems.filter(isUsableKey), [keyItems])
  const selectedKey = useMemo(
    () => keyItems.find((item) => item.id === keyId) ?? null,
    [keyItems, keyId],
  )

  // Pick the first usable key once the list arrives; keep the user's choice as
  // long as it is still present.
  useEffect(() => {
    if (keyId !== null && keyItems.some((item) => item.id === keyId)) return
    setKeyId(usableKeys[0]?.id ?? null)
  }, [keyId, keyItems, usableKeys])

  // A refetch after a recharge must clear the sticky 402 banner.
  const balance = overview.data?.balance
  const balanceMicro = balance?.micro
  useEffect(() => {
    if (balanceMicro !== undefined && balanceMicro > 0) setInsufficientError(false)
  }, [balanceMicro])

  const modelOptions = useMemo(
    () => [
      { value: '', label: '请选择模型' },
      ...(models.data ?? []).map((item) => ({
        value: item.model_name,
        label: item.display_name || item.model_name,
      })),
    ],
    [models.data],
  )

  const keyOptions = useMemo(
    () =>
      keyItems.length === 0
        ? [{ value: '', label: keys.isLoading ? '加载中…' : '暂无密钥', disabled: true }]
        : keyItems.map((item) => ({
            value: String(item.id),
            // Name + mask only: the full key never leaves the server.
            label: `${item.key_name} · ${item.key_masked}`,
            disabled: !isUsableKey(item),
          })),
    [keyItems, keys.isLoading],
  )

  const draftText = draft.trim()

  /**
   * Conversation sent to the gateway: the finished turns plus the composer
   * text (which has not been moved into `messages` yet).
   */
  const payloadMessages = useMemo(
    () => [
      ...(system.trim() ? [{ role: 'system', content: system }] : []),
      ...messages
        .filter((message) => message.content.trim().length > 0)
        .map((message) => ({ role: message.role, content: message.content })),
      ...(draftText ? [{ role: 'user', content: draftText }] : []),
    ],
    [system, messages, draftText],
  )

  /**
   * One construction used by the preview, the request and the generated code.
   * `key_id` is deliberately absent: it belongs to the outer envelope only.
   */
  const requestBody = useMemo<Record<string, unknown>>(
    () => ({
      model: model || null,
      messages: payloadMessages,
      temperature: temperature.trim() === '' ? null : Number(temperature),
      max_tokens: maxTokens.trim() === '' ? null : Number(maxTokens),
      stream,
    }),
    [model, payloadMessages, temperature, maxTokens, stream],
  )

  const preview = useMemo(() => JSON.stringify(requestBody, null, 2), [requestBody])

  const insufficient = (balance !== undefined && balance.micro <= 0) || insufficientError

  const overviewError = overview.isError ? errorMessage(overview.error) : null
  const overviewHint = overview.isError ? errorHint(overview.error) : null
  const modelsError = models.isError ? errorMessage(models.error) : null
  const modelsHint = models.isError ? errorHint(models.error) : null
  const keysError = keys.isError ? errorMessage(keys.error) : null
  const keysHint = keys.isError ? errorHint(keys.error) : null

  // 空输入不允许发送（纯空格也算空）；生成中由「停止」接管，不走这个条件。
  const canSend =
    !pending &&
    selectedKey !== null &&
    isUsableKey(selectedKey) &&
    !insufficient &&
    model.trim() !== '' &&
    draftText !== ''

  const hasConversation = messages.length > 0

  // Keep the newest turn in view while a stream grows.
  useEffect(() => {
    const element = logRef.current
    if (!element) return
    element.scrollTop = element.scrollHeight
  }, [messages, reply, sendError, sendHint])

  // Grow the composer with its content, up to COMPOSER_MAX_HEIGHT.
  useEffect(() => {
    const element = composerRef.current
    if (!element) return
    element.style.height = 'auto'
    element.style.height = `${Math.min(element.scrollHeight, COMPOSER_MAX_HEIGHT)}px`
  }, [draft])

  const applyExample = (example: Example) => {
    setSystem(example.system)
    setTemperature(example.temperature)
    setMaxTokens(example.maxTokens)
    setDraft(example.prompt)
    composerRef.current?.focus()
  }

  const handleStop = () => {
    abortRef.current?.abort()
  }

  // Abort on unmount so a half-read stream cannot keep the pre-deduction open.
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const handleClear = () => {
    if (pending) return
    setMessages([])
    setReply(null)
    setDraft('')
    setSendError(null)
    setSendHint(null)
    setInsufficientError(false)
    setCreditsUnknown(false)
    setLastRemaining(null)
    composerRef.current?.focus()
  }

  const handleSend = async () => {
    if (pending) return
    setSendError(null)
    setSendHint(null)
    setInsufficientError(false)
    setCreditsUnknown(false)

    if (selectedKey === null || !isUsableKey(selectedKey)) {
      setSendError('请先选择可用的密钥。')
      setSendHint('在密钥管理页创建或启用一个密钥后再发送。')
      return
    }
    if (insufficient) {
      setInsufficientError(true)
      return
    }
    if (!requestBody.model) {
      setSendError('请先选择模型。')
      return
    }
    // 空输入排在密钥 / 余额 / 模型之后：真正的阻断原因不能被「请输入内容」盖掉。
    if (!draftText) {
      setSendError('请输入消息内容后再发送。')
      setSendHint('Enter 发送，Shift+Enter 换行。')
      return
    }

    // Snapshot before appending the turn so the outgoing body is exactly the
    // conversation the user saw when pressing send.
    const body = requestBody
    const key = selectedKey
    const text = draftText
    const assistantId = newMessage('assistant').id
    const controller = new AbortController()
    abortRef.current = controller

    setPending(true)
    setDraft('')
    setMessages((current) => [
      ...current,
      ...(text ? [{ id: newMessage('user').id, role: 'user' as MessageRole, content: text }] : []),
      { id: assistantId, role: 'assistant' as MessageRole, content: '' },
    ])
    setReply({ id: assistantId, state: 'streaming' })

    try {
      const result = await playgroundChat({
        keyId: key.id,
        body,
        signal: controller.signal,
        onDelta: (delta) => {
          if (!delta) return
          setMessages((current) =>
            current.map((message) =>
              message.id === assistantId
                ? { ...message, content: message.content + delta }
                : message,
            ),
          )
        },
        onDone: () => {
          setReply((current) =>
            current && current.id === assistantId ? { ...current, state: 'done' } : current,
          )
        },
      })

      // Non-streaming responses deliver the text in the resolved value only.
      if (result.content) {
        setMessages((current) =>
          current.map((message) =>
            message.id === assistantId && message.content === ''
              ? { ...message, content: result.content }
              : message,
          ),
        )
      }

      // Balance comes from the response header or the overview endpoint only.
      if (result.creditsRemaining !== null) {
        setLastRemaining(result.creditsRemaining)
      } else {
        setCreditsUnknown(true)
      }
      setReply((current) =>
        current && current.id === assistantId ? { ...current, state: 'done' } : current,
      )
      void queryClient.invalidateQueries({ queryKey: ['console', 'overview'] })
    } catch (error) {
      if (isAbortError(error)) {
        // Keep what already arrived and label the turn instead of discarding it.
        setReply((current) =>
          current && current.id === assistantId ? { ...current, state: 'aborted' } : current,
        )
        return
      }

      setReply((current) =>
        current && current.id === assistantId ? { ...current, state: 'done' } : current,
      )
      // Drop an assistant bubble that never received a character.
      setMessages((current) =>
        current.filter((message) => message.id !== assistantId || message.content !== ''),
      )

      const remaining = playgroundCreditsRemaining(error)
      if (remaining !== null) setLastRemaining(remaining)

      if (error instanceof ApiError) {
        if (error.status === 402 || error.code === 'insufficient_credits') {
          setInsufficientError(true)
          void queryClient.invalidateQueries({ queryKey: ['console', 'overview'] })
        }
        setSendError(error.message)
        setSendHint(error.hint || null)
      } else {
        setSendError(errorMessage(error))
        setSendHint(errorHint(error))
      }
    } finally {
      setPending(false)
      abortRef.current = null
    }
  }

  const handleComposerKeyDown = (event: ReactKeyboardEvent<HTMLTextAreaElement>) => {
    // Enter sends, Shift+Enter inserts a newline; never intercept IME commits.
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return
    event.preventDefault()
    if (!canSend) return
    void handleSend()
  }

  const maskedKey = selectedKey?.key_masked ?? 'sk-****'
  const codeSnippet = useMemo(
    () =>
      buildSnippet(
        codeLanguage,
        `${resolveApiBaseUrl()}/v1/chat/completions`,
        maskedKey,
        requestBody,
      ),
    [codeLanguage, maskedKey, requestBody],
  )

  const balanceCard = (
    <div
      aria-label="可用余额"
      className="w-full rounded-xl border border-border bg-surface px-3 py-1 shadow-card sm:w-auto sm:min-w-[12rem] sm:py-2"
    >
      {/* 窄屏一行「标签 + 数值」，否则余额块会把输入框挤出首屏。 */}
      <div className="flex flex-wrap items-baseline gap-x-2 sm:block">
        <p className="text-xs text-ink-muted">可用余额</p>
        {overview.isLoading ? (
          <p className="text-sm text-ink-muted sm:mt-0.5">加载中…</p>
        ) : overviewError ? (
          <div className="w-full space-y-1 sm:mt-0.5">
            <p className="text-sm text-ink">--</p>
            <p className="text-xs text-danger">{overviewError}</p>
            {overviewHint ? <p className="text-xs text-ink-secondary">{overviewHint}</p> : null}
            <Button size="sm" variant="secondary" onClick={() => void overview.refetch()}>
              重试
            </Button>
          </div>
        ) : balance ? (
          <p className="text-sm text-ink sm:mt-0.5">
            {balance.credits} 积分
            <span className="ml-1 text-xs text-ink-secondary">¥{balance.cny}</span>
          </p>
        ) : (
          <p className="text-sm text-ink sm:mt-0.5">--</p>
        )}
      </div>

      {lastRemaining !== null ? (
        <p className="mt-1 text-xs text-ink-secondary">
          本次调用后余额：{lastRemaining} 积分（X-Credits-Remaining）
        </p>
      ) : null}
      {creditsUnknown ? (
        <p className="mt-1 text-xs text-ink-secondary">本次消耗以用量页为准。</p>
      ) : null}

      {insufficient ? (
        <p
          role="alert"
          className="mt-2 rounded-lg border border-danger bg-danger-soft px-2 py-1 text-xs text-danger"
        >
          余额不足，请先
          <a href="/console/recharge" className="underline">
            充值
          </a>
          后再调试。
        </p>
      ) : null}
    </div>
  )

  return (
    <div className="flex min-h-[calc(100dvh-13rem)] flex-col gap-2 sm:gap-3 md:min-h-[calc(100dvh-11rem)]">
      <PageHeader
        className="pb-4 sm:pb-5"
        title="在线调试台"
        description="选择密钥与模型后直接对话，响应流式返回。"
        actions={balanceCard}
      />

      {/* 顶部工具条：密钥 / 模型 / 高级参数。窄屏自动换行堆叠。 */}
      <div className="flex flex-wrap items-end gap-2">
        <div className="min-w-[10rem] flex-1 basis-56 space-y-1.5">
          <Select
            label="密钥"
            value={keyId !== null ? String(keyId) : ''}
            onChange={(event) => setKeyId(event.target.value ? Number(event.target.value) : null)}
            options={keyOptions}
          />
          {keysError ? (
            <ErrorState
              compact
              message={keysError}
              hint={keysHint}
              onRetry={() => void keys.refetch()}
            />
          ) : null}
          {!keys.isLoading && !keys.isError && usableKeys.length === 0 ? (
            <p role="alert" className="text-xs text-danger">
              请先在
              <a href="/console/keys" className="underline">
                密钥管理页
              </a>
              创建密钥。
            </p>
          ) : null}
        </div>

        <div className="min-w-[10rem] flex-1 basis-56 space-y-1.5">
          <ModelPicker
            label="模型"
            value={model}
            onChange={setModel}
            options={modelOptions}
            placeholder="搜索模型名"
          />
          {modelsError ? (
            <ErrorState
              compact
              message={modelsError}
              hint={modelsHint}
              onRetry={() => void models.refetch()}
            />
          ) : null}
        </div>

        <Button
          variant="secondary"
          size="sm"
          className="shrink-0"
          aria-expanded={advancedOpen}
          aria-controls="playground-advanced"
          onClick={() => setAdvancedOpen((open) => !open)}
        >
          高级参数
          <Icon
            name="chevron-down"
            className={cn('h-3.5 w-3.5 transition-transform', advancedOpen && 'rotate-180')}
          />
        </Button>
      </div>

      <p className="text-xs text-ink-muted">
        费用按实际调用计入当前账号；Enter 发送，Shift+Enter 换行。
      </p>

      {advancedOpen ? (
        <section
          id="playground-advanced"
          aria-label="高级参数"
          className="space-y-3 rounded-xl border border-border bg-surface p-4 shadow-card"
        >
          <label className="block space-y-1.5">
            <span className="block text-sm font-medium text-ink">System 提示词</span>
            <textarea
              value={system}
              onChange={(event) => setSystem(event.target.value)}
              rows={2}
              className="w-full rounded-lg border border-border-strong bg-surface px-3 py-2 text-sm text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
              placeholder="设定助手角色与回答约束"
              aria-label="System 提示词"
            />
          </label>

          <div className="grid gap-3 sm:grid-cols-3">
            <Input
              label="temperature"
              type="number"
              min={0}
              max={2}
              step="0.1"
              value={temperature}
              onChange={(event) => setTemperature(event.target.value)}
            />
            <Input
              label="max_tokens"
              type="number"
              min={1}
              step={1}
              value={maxTokens}
              onChange={(event) => setMaxTokens(event.target.value)}
            />
            <label className="flex h-full items-center gap-2 pt-6 text-sm text-ink">
              <input
                type="checkbox"
                checked={stream}
                onChange={(event) => setStream(event.target.checked)}
                className="h-4 w-4 rounded border-border-strong text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
              />
              流式输出（stream）
            </label>
          </div>
        </section>
      ) : null}

      {/* 对话区：占满剩余高度，内部滚动，首屏即可见。 */}
      <section
        aria-label="对话"
        className="flex min-h-[12rem] flex-1 flex-col overflow-hidden rounded-xl border border-border bg-surface shadow-card md:min-h-[16rem]"
      >
        <div
          ref={logRef}
          role="log"
          aria-live="polite"
          aria-label="对话内容"
          className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-3 py-4 sm:px-4"
        >
          {hasConversation ? null : (
            <div className="m-auto w-full max-w-md space-y-3">
              {/* 空状态不用虚线框：那在国内产品里读作「这块还没做完」，
                  而这里要表达的是「对话区本身就是空的」。无边框、只居中排版，
                  让它融进对话区背景。 */}
              <div className="flex flex-col items-center gap-2 text-center">
                <div className="flex h-10 w-10 items-center justify-center rounded-full bg-surface-muted text-ink-secondary">
                  <Icon name="terminal" className="h-5 w-5" />
                </div>
                <p className="text-sm font-medium text-ink">开始对话</p>
                <p className="text-xs text-ink-muted">发送一条消息，开始测试模型回复。</p>
              </div>
              <div className="space-y-2">
                <p className="text-center text-xs text-ink-muted">
                  试试示例：只填入内容，不会发送，也不会计费。
                </p>
                <div className="flex flex-wrap justify-center gap-2">
                  {EXAMPLES.map((example) => (
                    <Button
                      key={example.id}
                      variant="secondary"
                      size="sm"
                      onClick={() => applyExample(example)}
                    >
                      {example.title}
                    </Button>
                  ))}
                </div>
              </div>
            </div>
          )}

          {messages.map((message) => {
            const active = reply?.id === message.id
            const aborted = active && reply?.state === 'aborted'
            const isUser = message.role === 'user'
            return (
              <div
                key={message.id}
                className={cn('flex flex-col gap-1', isUser ? 'items-end' : 'items-start')}
              >
                <span className="px-1 text-xs text-ink-muted">{isUser ? '你' : '助手'}</span>
                <div
                  className={cn(
                    'max-w-[85%] whitespace-pre-wrap break-words rounded-xl px-3 py-2 text-sm text-ink',
                    isUser
                      ? 'bg-primary-soft'
                      : 'border border-border bg-surface-muted',
                  )}
                >
                  {active ? (
                    <span data-testid="playground-reply-content">{message.content}</span>
                  ) : (
                    <span>{message.content}</span>
                  )}
                  {active && reply?.state === 'streaming' ? (
                    <span aria-hidden="true" className="animate-pulse">
                      ▍
                    </span>
                  ) : null}
                </div>
                {aborted ? (
                  <div className="flex flex-wrap items-center gap-2 px-1">
                    <Badge variant="neutral">已中止</Badge>
                    <span className="text-xs text-ink-secondary">已保留中止前收到的内容。</span>
                  </div>
                ) : null}
              </div>
            )
          })}

          {sendError ? (
            <div
              role="alert"
              className="space-y-1 rounded-xl border border-danger bg-danger-soft px-3 py-2"
            >
              <p className="text-sm text-danger">{sendError}</p>
              {sendHint ? <p className="text-xs text-ink-secondary">{sendHint}</p> : null}
            </div>
          ) : null}
        </div>

        {/* 底部输入区：贴底常驻。 */}
        <form
          className="border-t border-border bg-surface px-3 py-3 sm:px-4"
          onSubmit={(event) => {
            event.preventDefault()
            void handleSend()
          }}
        >
          <textarea
            ref={composerRef}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={handleComposerKeyDown}
            aria-label="消息输入"
            rows={2}
            placeholder="输入消息，Enter 发送，Shift+Enter 换行"
            className="max-h-36 w-full resize-none rounded-lg border border-border-strong bg-surface px-3 py-2 text-sm text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
          />
          <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
            {/* 费用说明只在顶部提示行出现一次，这里不重复；仅在流式中给出状态提示。 */}
            {pending ? (
              <p className="hidden text-xs text-ink-muted sm:block">
                正在生成，可随时停止并保留已收到的内容。
              </p>
            ) : null}
            <div className="ml-auto flex items-center gap-2">
              {pending ? (
                <Button variant="secondary" onClick={handleStop}>
                  停止
                </Button>
              ) : (
                <Button type="submit" disabled={!canSend}>
                  发送
                </Button>
              )}
              <Button
                variant="ghost"
                aria-label="清空对话"
                onClick={handleClear}
                disabled={pending || (!hasConversation && draft === '' && !sendError)}
              >
                清空对话
              </Button>
            </div>
          </div>
        </form>
      </section>

      {/* 开发者信息：默认收起，避免挤占首屏对话区。 */}
      <div className="rounded-xl border border-border bg-surface shadow-card">
        <button
          type="button"
          onClick={() => setDevOpen((open) => !open)}
          aria-expanded={devOpen}
          aria-controls="playground-dev"
          className="flex w-full items-center justify-between gap-2 px-4 py-2.5 text-sm font-medium text-ink transition-colors duration-fast hover:bg-surface-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        >
          请求预览与调用代码
          <Icon
            name="chevron-down"
            className={cn(
              'h-4 w-4 text-ink-muted transition-transform',
              devOpen && 'rotate-180',
            )}
          />
        </button>
        {devOpen ? (
          <div id="playground-dev" className="space-y-4 border-t border-border p-4">
            <section aria-label="请求预览" className="space-y-2">
              <h2 className="text-sm font-semibold text-ink">请求预览</h2>
              <p className="text-xs text-ink-secondary">
                以下为将要发送的请求体，仅用于核对参数。
              </p>
              <CopyBlock code={preview} language="json" label="请求预览" />
            </section>

            <section aria-label="调用代码" className="space-y-2">
              <h2 className="text-sm font-semibold text-ink">调用代码</h2>
              <p className="text-xs text-ink-secondary">
                密钥位置使用掩码占位，复制后请替换为你自己的完整密钥。
              </p>
              <div className="w-48">
                <Select
                  label="调用代码语言"
                  value={codeLanguage}
                  onChange={(event) => setCodeLanguage(event.target.value as CodeLanguage)}
                  options={CODE_LANGUAGE_OPTIONS}
                />
              </div>
              <CopyBlock
                code={codeSnippet}
                language={codeLanguage}
                label={`调用代码（${codeLanguage}）`}
              />
            </section>
          </div>
        ) : null}
      </div>
    </div>
  )
}
