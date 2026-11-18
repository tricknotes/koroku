# paas-console

Management UI of paasd, written in Ember + TypeScript. The production build
(`dist/`) is embedded into the `paasd` binary.

## Development

```sh
pnpm install
pnpm start        # http://localhost:4200, proxies /api and /auth to paasd on 127.0.0.1:8080
pnpm lint
pnpm test         # needs Google Chrome or Chromium on PATH
pnpm build        # writes dist/
```

Set `PAASD_URL` to proxy to a different paasd address.
