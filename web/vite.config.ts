import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// 开发期把后端的全部对外通道代理到本地服务：
// /api 是 REST，/ws 是 WebSocket（必须 ws:true，否则升级请求不被转发），
// /sse 是 EventSource 备选通道。三者都要代理，否则 realtime store 在 dev 下连不上。
// 生产形态是 embed 进单二进制同源提供（设计文档 §5.8），不走这条代理。
// 构建产物直接落 internal/web/dist——embed 指令只能收包目录内的文件，
// dist 必须和 internal/web/embed_dashboard.go 同层，npm run build 后即可 go build -tags dashboard。
// outDir 在 vite root 之外，emptyOutDir 必须显式声明，否则 vite 只警告不清理。
const backend = 'http://localhost:8080'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5177,
    proxy: {
      '/api': { target: backend, changeOrigin: true },
      '/sse': { target: backend, changeOrigin: true },
      '/ws': { target: backend, ws: true },
    },
  },
})
