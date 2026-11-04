/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_BASE_URL?: string
  readonly VITE_DEV_API_TARGET?: string
  /**
   * 服务端 AUTH_SMS_MODE 的部署期镜像（'true' / 'false'）。
   *
   * 只影响文案与本地拦截：SPA 读不到服务端环境变量，`GET /auth/capabilities`
   * （P2-1）落地后删除。两者必须一起翻，否则页脚会说谎（说支持短信但实际 400）。
   */
  readonly VITE_AUTH_SMS_ENABLED?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
