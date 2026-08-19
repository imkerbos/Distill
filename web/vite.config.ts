import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// 前端固定 4000，后端 10100 —— 本机其他项目占用了 3X00 与 100X0 的其余槽位。
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // 监听全部接口，便于从同网段的其他设备访问。代理目标仍写
    // localhost：它在跑 vite 的这台机器上解析，所以从别的设备访问
    // :4000 时请求照样落到本机的 :10100，浏览器看到的仍是同源，
    // 会话 Cookie 不受影响。
    host: '0.0.0.0',
    port: 4000,
    strictPort: true, // 端口被占时直接失败，而不是静默换一个别人在用的端口
    proxy: {
      // 必须走代理：会话 Cookie 是 HttpOnly + SameSite=Lax，
      // 经代理时浏览器视作同源会自动携带；直连 :10100 属跨站，
      // 需要 SameSite=None + Secure，而 Secure 在 http://localhost 上不生效。
      '/api': {
        target: 'http://localhost:10100',
        changeOrigin: false, // 保持 Host，后端按同源处理 Cookie
      },
    },
  },
})
