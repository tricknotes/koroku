# koroku

VPS 1 台の上で、GitHub リポジトリの Docker Compose アプリを UI からデプロイする、個人用の Heroku ライクなホスティング基盤です。コントロールプレーンは Go 製シングルバイナリ `paasd`（SQLite、Ember 製 SPA を同梱）で、リバースプロキシに Caddy を使います。

- 設計: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- HTTP API: [docs/API.md](docs/API.md)
- マイルストーンと実装規約: [CLAUDE.md](CLAUDE.md)

## 開発

必要なもの: Go 1.27 以上、Node.js 24 以上、pnpm、（結合テストには）Docker と Compose v2.24.4 以上。

```sh
# ユニットテスト（Docker 不要）
go vet ./...
go test ./...

# 実 Docker を使う結合テスト
go test -tags integration ./...

# フロントエンド（テストには Chrome か Chromium が必要）
cd web && pnpm install && pnpm lint && pnpm test
```

### ローカルで動かす

```sh
cp deploy/config.example.toml config.toml   # listen と data_dir を手元向けに書き換える
make web                                     # Ember をビルドして web/dist に出力
go run ./cmd/paasd serve --config config.toml --insecure-no-auth
curl http://127.0.0.1:8080/api/system
```

`--insecure-no-auth` はログインと Origin 検証を外す開発用のフラグです（到達できる相手は誰でも Docker を操作できるので、手元以外では使わないこと）。手元に Caddy がなければ、`config.toml` の `[caddy]` で `enabled = false` にします。

フロントエンドだけを開発するときは `cd web && pnpm start` で Vite の開発サーバーを起動します。`/api` と `/auth` は `127.0.0.1:8080` の `paasd` に転送されます。

### 配布用バイナリ

```sh
make build    # フロントエンド込みの bin/paasd（linux/amd64）
```

## ディレクトリ構成

| パス | 内容 |
| --- | --- |
| `cmd/paasd/` | エントリポイント |
| `internal/` | API、デプロイ、Compose、Caddy、GitHub、ストアなどの各パッケージ |
| `web/` | Ember アプリ（管理 UI） |
| `deploy/` | 設定例、プロビジョニング |
| `docs/` | 設計資料 |
| `testdata/` | サンプル Compose、golden file |
