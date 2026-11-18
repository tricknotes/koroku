import { defineConfig } from 'vite';
import { extensions, classicEmberSupport, ember } from '@embroider/vite';
import { babel } from '@rollup/plugin-babel';

// During development the API is served by a local `paasd serve`.
const apiTarget = process.env.PAASD_URL ?? 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [
    classicEmberSupport(),
    ember(),
    // extra plugins here
    babel({
      babelHelpers: 'runtime',
      extensions,
    }),
  ],
  server: {
    proxy: {
      '/api': apiTarget,
      '/auth': apiTarget,
    },
  },
});
