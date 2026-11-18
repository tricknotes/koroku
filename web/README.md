# paas-console

Management UI of paasd, written in Ember + TypeScript. The production build
(`dist/`) is embedded into the `paasd` binary.

## Development

Nothing is installed on the host: run the commands inside the development
container with `bin/dev` from the repository root (see the top-level README).

```sh
bin/dev sh -c 'cd web && pnpm install --frozen-lockfile'
bin/dev sh -c 'cd web && pnpm start'   # http://localhost:4200, proxies /api and /auth to paasd on 127.0.0.1:8080
bin/dev sh -c 'cd web && pnpm lint && pnpm test'
bin/dev sh -c 'cd web && pnpm build'   # writes dist/
```

Set `PAASD_URL` to proxy to a different paasd address.
