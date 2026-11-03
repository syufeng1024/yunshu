import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ModelPicker } from './ModelPicker'

const options = [
  { value: 'text-large', label: 'Text Large' },
  { value: 'glm-5', label: 'GLM-5' },
  { value: 'claude-sonnet', label: 'Claude Sonnet' },
]

describe('ModelPicker', () => {
  it('filters the list by keyword and picks the matched model', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<ModelPicker label="模型" value="text-large" onChange={onChange} options={options} />)

    const input = screen.getByLabelText('模型')
    expect(input).toHaveValue('text-large')

    await user.click(input)
    expect(screen.getByRole('option', { name: /GLM-5/ })).toBeInTheDocument()

    await user.type(input, 'claude')
    expect(screen.queryByRole('option', { name: /GLM-5/ })).not.toBeInTheDocument()

    await user.click(screen.getByRole('option', { name: /Claude Sonnet/ }))
    expect(onChange).toHaveBeenCalledWith('claude-sonnet')
    // 选中后列表收起，输入框回到「显示当前值」的状态。
    expect(screen.queryByRole('option', { name: /Claude Sonnet/ })).not.toBeInTheDocument()
  })

  it('says so when nothing matches instead of showing an empty box', async () => {
    const user = userEvent.setup()
    render(<ModelPicker label="模型" value="" onChange={vi.fn()} options={options} />)

    const input = screen.getByLabelText('模型')
    await user.click(input)
    await user.type(input, 'zzz')
    expect(screen.getByText('没有匹配的模型')).toBeInTheDocument()
  })

  it('closes on Escape without changing the selection', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<ModelPicker label="模型" value="glm-5" onChange={onChange} options={options} />)

    const input = screen.getByLabelText('模型')
    await user.click(input)
    await user.type(input, 'cla')
    await user.keyboard('{Escape}')

    expect(screen.queryByRole('option', { name: /Claude Sonnet/ })).not.toBeInTheDocument()
    expect(input).toHaveValue('glm-5')
    expect(onChange).not.toHaveBeenCalled()
  })
})
