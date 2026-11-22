import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';
import { viteSingleFile } from 'vite-plugin-singlefile';

/**
 * 目录约定：
 *   web/app/         前端源码（Vite root，入口 app/index.html）
 *   web/index.html   构建产物：自包含单文件，交给 nginx 或静态站点托管
 *
 * 产物必须是一个自包含的 HTML：JS / CSS 全部内联，部署时只需一个文件。
 * 因此 outDir 指向 web/ 根目录，且不能清空它（否则会把源码和 package.json 删掉）。
 *
 * 后端不托管它：gin 只出 /api/v1 与 /ws。
 */
export default defineConfig({
  root: 'app',
  // .env 放在 web/（与 package.json 同级），而不是 Vite root（web/app）
  envDir: '..',
  plugins: [vue(), viteSingleFile()],
  build: {
    outDir: '../',
    emptyOutDir: false,
    target: 'es2020',
    cssCodeSplit: false,
    assetsInlineLimit: 1024 * 1024,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8080', ws: true },
    },
  },
});
