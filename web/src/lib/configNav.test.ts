import { describe, expect, it } from 'vitest'
import { CONFIG_FIELD_META } from './configKeys'
import { CONFIG_NAV, CONFIG_NAV_ITEMS, CONFIG_NAV_ITEM_OF_KEY, firstItemWithKeys } from './configNav'

describe('系统管理菜单（configNav）', () => {
  it('45 个配置键全部有归属，且没有一键两组', () => {
    const allKeys = CONFIG_NAV.flatMap((group) => group.items.flatMap((item) => [...item.keys]))

    // 重复登记会让同一键出现在两个设置项里，改一处另一处还是旧值。
    const seen = new Set<string>()
    const duplicated: string[] = []
    for (const key of allKeys) {
      if (seen.has(key)) duplicated.push(key)
      seen.add(key)
    }
    expect(duplicated).toEqual([])

    // 漏登记 = 键在接口里有、在菜单里找不到，运营永远改不到。
    expect([...allKeys].sort()).toEqual(Object.keys(CONFIG_FIELD_META).sort())
  })

  it('设置项 id 与分组 id 都唯一', () => {
    const itemIds = CONFIG_NAV_ITEMS.map((item) => item.id)
    expect(new Set(itemIds).size).toBe(itemIds.length)
    const groupIds = CONFIG_NAV.map((group) => group.id)
    expect(new Set(groupIds).size).toBe(groupIds.length)
  })

  it('每个键都能反查到唯一设置项，供菜单标「待保存」用', () => {
    expect(CONFIG_NAV_ITEM_OF_KEY.size).toBe(Object.keys(CONFIG_FIELD_META).length)
    for (const [key, itemId] of CONFIG_NAV_ITEM_OF_KEY) {
      const item = CONFIG_NAV_ITEMS.find((entry) => entry.id === itemId)
      expect(item, `${key} 指向了不存在的设置项 ${itemId}`).toBeDefined()
      expect(item?.keys).toContain(key)
    }
  })

  it('默认落在第一个真有配置键的设置项上，不落到空页', () => {
    const first = firstItemWithKeys()
    expect(first.keys.length).toBeGreaterThan(0)
    expect(CONFIG_NAV_ITEMS[0]?.id).toBe(first.id)
  })

  it('菜单里不出现没有配置键的条目（2026-11-05 裁定：空项一律移除）', () => {
    for (const item of CONFIG_NAV_ITEMS) {
      expect(item.keys.length, `${item.title} 没有配置键，不该出现在菜单里`).toBeGreaterThan(0)
      expect(item.description.length).toBeGreaterThan(10)
      expect(item.title.length).toBeGreaterThan(1)
    }
  })

  it('菜单规模钉在 5 组 11 项：改动这里必须同步改文档与产品口径', () => {
    expect(CONFIG_NAV).toHaveLength(5)
    expect(CONFIG_NAV_ITEMS).toHaveLength(11)
  })
})
