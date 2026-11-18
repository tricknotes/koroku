# koroku

VPS 1 台の上で、GitHub リポジトリの Docker Compose アプリを UI からデプロイする、個人用の Heroku ライクなホスティング基盤です。コントロールプレーンは Go 製シングルバイナリ `paasd`（SQLite、Ember 製 SPA を同梱）で、リバースプロキシに Caddy を使います。

- 設計: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- HTTP API: [docs/API.md](docs/API.md)
- マイルストーンと実装規約: [CLAUDE.md](CLAUDE.md)

## 開発

必要なものは Docker だけです。Go、Node.js、pnpm、Chromium、restic などの道具は開発用イメージ（[dev/Dockerfile](dev/Dockerfile)）に入っていて、ホストには何も入れません。コマンドは `bin/dev` 経由でコンテナの中で実行します。イメージは初回と `dev/Dockerfile` を変えたときに自動でビルドされます。

```sh
# ユニットテスト
bin/dev go vet ./...
bin/dev go test ./...

# 実 Docker を使う結合テスト（ホストの Docker デーモンでコンテナを起動する）
bin/dev go test -tags integration ./...

# フロントエンド
bin/dev sh -c 'cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm test'

# 対話シェル
bin/dev
```

`bin/dev` はリポジトリをホストと同じパスでマウントし、呼び出したユーザーの UID で動くので、生成物の所有者がずれません。Go と pnpm のキャッシュは Docker の named volume（`paasd-dev-*`）に置きます。コンテナはホストのネットワークを共有し、Docker ソケットをマウントしているので、結合テストが起動したアプリの `127.0.0.1:<port>` にそのまま届きます。

### ローカルで動かす

```sh
cp deploy/config.example.toml config.toml   # listen と data_dir を手元向けに書き換え、[caddy] は enabled = false にする
bin/dev make web                             # Ember をビルドして web/dist に出力
bin/dev go run ./cmd/paasd serve --config config.toml --insecure-no-auth
curl http://127.0.0.1:8080/api/system
```

`--insecure-no-auth` はログインを外す開発用のフラグです（到達できる相手は誰でも Docker を操作できるので、手元以外では使わないこと）。`[caddy] enabled = true` のままでは起動を拒否します。`curl` で変更系の API を叩くときは `-H 'Content-Type: application/json'` を付けます。

フロントエンドだけを開発するときは `bin/dev sh -c 'cd web && pnpm start'` で Vite の開発サーバーを起動します。`/api` と `/auth` は `127.0.0.1:8080` の `paasd` に転送されます。

### 配布用バイナリ

```sh
bin/dev make build    # フロントエンド込みの bin/paasd（linux/amd64）
```

## ディレクトリ構成

| パス | 内容 |
| --- | --- |
| `cmd/paasd/` | エントリポイント |
| `internal/` | API、デプロイ、Compose、Caddy、GitHub、ストアなどの各パッケージ |
| `web/` | Ember アプリ（管理 UI） |
| `deploy/` | 設定例、プロビジョニング |
| `dev/`、`bin/dev` | 開発用の Docker イメージと、その中でコマンドを実行するラッパー |
| `docs/` | 設計資料 |
| `testdata/` | サンプル Compose、golden file |
