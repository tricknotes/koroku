# koroku

VPS 1 台の上で、GitHub リポジトリの Docker Compose アプリを UI からデプロイする、個人用の Heroku ライクなホスティング基盤です。コントロールプレーンは Go 製シングルバイナリ `koroku`（SQLite、Ember 製 SPA を同梱）で、リバースプロキシに Caddy を使います。

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

`bin/dev` はリポジトリをホストと同じパスでマウントし、呼び出したユーザーの UID で動くので、生成物の所有者がずれません。Go と pnpm のキャッシュは Docker の named volume（`koroku-dev-*`）に置きます。コンテナはホストのネットワークを共有し、Docker ソケットをマウントしているので、結合テストが起動したアプリの `127.0.0.1:<port>` にそのまま届きます。

### ローカルで動かす

```sh
docker compose up
```

リポジトリのルートで実行し、<http://localhost:4200> を開きます。[compose.yaml](compose.yaml) は開発専用で、次の 2 つを起動します。

| サービス | 内容 |
| --- | --- |
| `web` | Vite の開発サーバー（development モード）。Ember アプリのソースを変えるとホットリロードされる。`/api` と `/auth` は `koroku` に転送する |
| `koroku` | このチェックアウトからビルドしたコントロールプレーン。ログインなし（`--insecure-no-auth`）で `127.0.0.1:8080` で待ち受ける |
| `caddy` | リバースプロキシ。`127.0.0.1` の 80 と 443 だけで待ち受け、`koroku` が本番と同じく設定を投入する。証明書は Caddy 内蔵の CA で出す |

- アプリは手元の Docker にデプロイされ、`https://<slug>.dev.localhost` で開けます（`*.localhost` は OS やブラウザがループバックに解決します）。管理 UI は Vite の <http://localhost:4200> のほか、`pnpm build` 済みなら `https://console.dev.localhost` でも開けます。
- 証明書の警告を消すには、Caddy の CA のルート証明書をブラウザ側の OS に信頼させます。取り出しは `docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt data/dev/caddy-root.crt`。WSL2 で Windows のブラウザから開く場合は、Windows 側で `certutil -user -addstore Root <root.crt のパス>` を実行します（確認ダイアログが出ます）。CA は volume `koroku-dev-caddy-data` にあり、消すと作り直されるので、入れ直しが必要です。
- 既存の `data/dev/config.toml` は上書きされません。以前に作ったものは `[caddy]` を [dev/serve.sh](dev/serve.sh) が書き出す内容に合わせてください（`enabled = false` にすれば Caddy なしで動き、アプリには `http://127.0.0.1:<ホストポート>` で届きます）。
- GitHub App を設定していないので、アプリ作成では「URL を直接指定」で public リポジトリの URL を入れます。手元のクローンを `file:///...` で指定することもできます（リポジトリのあるディレクトリの配下に限る。コンテナからはそこしか見えません）。
- Go のコードを変えたら `docker compose restart koroku` でビルドし直します。
- 設定と状態は `data/dev/`（git の管理外）に置きます。設定ファイル `data/dev/config.toml` は初回に [dev/serve.sh](dev/serve.sh) が書き出し、以後は上書きしません。ログ退避やバックアップを試すときはここに `[storage]` などを足します。`data/dev/` を消すと最初からやり直せます。
- 別のバックエンドに向けるときは `KOROKU_URL=http://127.0.0.1:<port> docker compose up web` とします。
- `bin/dev` が先にキャッシュ用の volume を作っていると、Compose が「volume already exists but was not created by Docker Compose」と警告しますが、同じキャッシュを共有しているだけで害はありません。

`--insecure-no-auth` はログインを外す開発用のフラグです（到達できる相手は誰でも Docker を操作できるので、手元以外では使わないこと）。ループバックのホスト名でしか応答せず、`[caddy] enabled = true` のままでは起動を拒否します。`curl` で変更系の API を叩くときは `-H 'Content-Type: application/json'` を付けます。

Compose を使わずに動かすこともできます。

```sh
bin/dev dev/serve.sh                                   # koroku（設定は data/dev/config.toml）
bin/dev sh -c 'cd web && pnpm start --port 4200'       # Vite の開発サーバー
```

### 配布用バイナリ

```sh
bin/dev make build    # フロントエンド込みの bin/koroku（linux/amd64）
```

## ディレクトリ構成

| パス | 内容 |
| --- | --- |
| `cmd/koroku/` | エントリポイント |
| `internal/` | API、デプロイ、Compose、Caddy、GitHub、ストアなどの各パッケージ |
| `web/` | Ember アプリ（管理 UI） |
| `deploy/` | 設定例、プロビジョニング |
| `dev/`、`bin/dev`、`compose.yaml` | 開発用の Docker イメージ、その中でコマンドを実行するラッパー、手元で動かすための Compose 設定 |
| `docs/` | 設計資料 |
| `testdata/` | サンプル Compose、golden file |
