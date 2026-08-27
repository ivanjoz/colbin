import adapter from '@sveltejs/adapter-static'
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte'

/** @type {import('@sveltejs/kit').Config} */
export default {
  preprocess: vitePreprocess(),
  compilerOptions: { runes: true },
  kit: {
    // Fully static: every route is prerendered, so a path that is not in the
    // catalog is a 404 from GitHub Pages rather than a router guess.
    adapter: adapter({ pages: 'build-site', assets: 'build-site', precompress: false, strict: true }),
    // Empty on purpose. The site is served from the root of colbin.un.pe, so
    // the /colbin prefix a github.io/<repo> deploy would need does not apply,
    // and hard-coding it would break every asset.
    paths: { base: '' },
    prerender: { handleHttpError: 'fail' },
  },
}
