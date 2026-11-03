import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useKeys } from '@/api/console'
import { usePublicModels } from '@/api/public'
import { Button } from '@/components/Button'
import { resolveGatewayBaseUrl } from '@/lib/apiBase'
import { ROUTES } from '@/lib/constants'

/**
 * 「开始使用」三步指引。
 *
 * 参考的同类产品把上手流程放在概览页首屏，而不是散落在文档里。我们原来的概览
 * 只有余额与用量图：一个刚注册、没有密钥、没有流量的账号打开看到的是三个 0 和
 * 一条空曲线，看不出下一步该做什么。
 *
 * 只在**还没有密钥**时出现（有密钥说明已经上手了），并且可以手动收起——否则
 * 老用户每次进概览都要划过一个他们早已完成的东西。
 */
export function GettingStarted() {
  // 只在本会话收起：check:copy 禁止生产代码直接引用浏览器存储，而这个偏好
  // 不值得为它开一个存储封装——刷新后再出现一次并不会造成困扰，反而是提醒。
  const [hidden, setHidden] = useState(false)

  const keys = useKeys({ page: 1, pageSize: 1 })
  const models = usePublicModels()

  // 加载中不闪一下再消失；有密钥就不出现。
  if (hidden || keys.isLoading || keys.isError) return null
  if ((keys.data?.total ?? 0) > 0) return null

  const gateway = resolveGatewayBaseUrl()
  const exampleModel = models.data?.data?.[0]?.model_name ?? '<模型名>'
  const curl = `curl ${gateway || 'https://你的调用地址/v1'}/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer sk-你的密钥" \\
  -d '{"model":"${exampleModel}","messages":[{"role":"user","content":"你好"}]}'`

  const hide = () => setHidden(true)

  return (
    <section className="rounded-xl border border-border bg-surface p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-ink">开始使用</h2>
          <p className="mt-1 text-xs text-ink-secondary">三步发出第一次调用。</p>
        </div>
        <Button variant="ghost" size="sm" onClick={hide}>
          收起指引
        </Button>
      </div>

      <ol className="mt-4 grid gap-3 sm:grid-cols-3">
        {[
          {
            step: '1',
            title: '创建 API 密钥',
            body: '在密钥管理里生成一把密钥，只在创建时显示一次。',
            to: ROUTES.consoleKeys,
            cta: '去创建',
          },
          {
            step: '2',
            title: '确认余额',
            body: '充值或领取体验积分后再调用，余额不足会返回 402。',
            to: ROUTES.consoleRecharge,
            cta: '去充值',
          },
          {
            step: '3',
            title: '发送首个请求',
            body: '复制下方命令，把密钥换成你自己的。',
            to: ROUTES.docs,
            cta: '看文档',
          },
        ].map((item) => (
          <li key={item.step} className="rounded-lg border border-border bg-canvas p-4">
            <div className="flex items-center gap-2">
              <span className="flex h-6 w-6 items-center justify-center rounded-full border border-border-strong text-xs font-medium text-ink-secondary">
                {item.step}
              </span>
              <h3 className="text-sm font-medium text-ink">{item.title}</h3>
            </div>
            <p className="mt-2 text-xs leading-relaxed text-ink-secondary">{item.body}</p>
            <Link
              to={item.to}
              className="mt-2 inline-block text-xs font-medium text-primary hover:text-primary-hover"
            >
              {item.cta} →
            </Link>
          </li>
        ))}
      </ol>

      <div className="mt-4 rounded-lg border border-border bg-canvas p-4">
        <div className="flex items-center justify-between gap-2">
          <p className="text-xs font-medium text-ink-secondary">示例请求</p>
          {gateway ? <code className="text-xs text-ink-muted">{gateway}</code> : null}
        </div>
        <pre className="mt-2 overflow-x-auto whitespace-pre text-xs leading-relaxed text-ink">
          <code className="font-mono">{curl}</code>
        </pre>
      </div>
    </section>
  )
}
