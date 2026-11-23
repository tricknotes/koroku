# 開発ガイド

koroku 自体を開発する人向けの文書。使い方は [README](../README.md)、設計は [ARCHITECTURE.md](ARCHITECTURE.md)、実装規約とマイルストーンは [CLAUDE.md](../CLAUDE.md) を参照。

## 道具

必要なものは Docker だけ。Go、Node.js、pnpm、Chromium、restic、shellcheck などの道具は開発用イメージ（[dev/Dockerfile](../dev/Dockerfile)）に入っていて、ホストには何も入れない。コマンドは `bin/dev` 経由でコンテナの中で実行する。イメージは初回と `dev/Dockerfile` を変えたときに自動でビルドされる。

```sh
# ユニットテスト
bin/dev go vet ./...
bin/dev go test ./...

# 実 Docker と restic を使う結合テスト（ホストの Docker デーモンでコンテナを起動する）
bin/dev go test -tags integration ./...

# フロントエンド
bin/dev sh -c 'cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm test'

# プロビジョニングスクリプトの通し実行（使い捨ての Ubuntu コンテナで。既定は 24.04）
bin/dev make build && deploy/test/dry-run.sh bin/koroku
IMAGE=ubuntu:26.04 deploy/test/dry-run.sh bin/koroku

# 対話シェル
bin/dev
```

`bin/dev` はリポジトリをホストと同じパスでマウントし、呼び出したユーザーの UID で動くので、生成物の所有者がずれない。Go と pnpm のキャッシュは Docker の named volume（`koroku-dev-*`）に置く。コンテナはホストのネットワークを共有し、Docker ソケットをマウントしているので、結合テストが起動したアプリの `127.0.0.1:<port>` にそのまま届く。

## ローカルで動かす

```sh
docker compose up
```

リポジトリのルートで実行し、<http://localhost:4200> を開く。[compose.yaml](../compose.yaml) は開発専用で、次のサービスを起動する。

| サービス | 内容 |
| --- | --- |
| `web` | Vite の開発サーバー（development モード）。Ember アプリのソースを変えるとホットリロードされる。`/api` と `/auth` は `koroku` に転送する |
| `koroku` | このチェックアウトからビルドしたコントロールプレーン。ログインなし（`--insecure-no-auth`）で `127.0.0.1:8080` で待ち受ける |
| `caddy` | リバースプロキシ。`127.0.0.1` の 80 と 443 だけで待ち受け、`koroku` が本番と同じく設定を投入する。証明書は Caddy 内蔵の CA で出す |
| `storage` | S3 互換のオブジェクトストレージ（[Versity S3 Gateway](https://github.com/versity/versitygw)）。本番のオブジェクトストレージの代わりに、ログの退避と restic のバックアップの置き場になる。軽く、環境変数だけで設定でき、中身が volume `koroku-dev-s3-data` にファイルとして並ぶ。`127.0.0.1:9000` で待ち受け、WebUI は <http://127.0.0.1:9001>（アクセスキー `koroku-dev`、シークレット `koroku-dev-secret`） |
| `storage-setup` | `storage` にバケット `koroku` を作って終わる |

- アプリは手元の Docker にデプロイされ、`https://<slug>.dev.localhost` で開ける（`*.localhost` は OS やブラウザがループバックに解決する）。管理 UI は Vite の <http://localhost:4200> のほか、`pnpm build` 済みなら `https://console.dev.localhost` でも開ける。
- 証明書の警告を消すには、Caddy の CA のルート証明書をブラウザ側の OS に信頼させる。取り出しは `docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt data/dev/caddy-root.crt`。WSL2 で Windows のブラウザから開く場合は、Windows 側で `certutil -user -addstore Root <root.crt のパス>` を実行する（確認ダイアログが出る）。CA は volume `koroku-dev-caddy-data` にあり、消すと作り直されるので、入れ直しが必要。
- 設定と状態は `data/dev/`（git の管理外）に置く。設定ファイル `data/dev/config.toml` は初回に [dev/serve.sh](../dev/serve.sh) が書き出し、以後は上書きしない。`dev/serve.sh` が書き出す内容が変わったら、手元の `config.toml` も合わせる（`[caddy]` を `enabled = false` にすれば Caddy なしで動き、アプリには `http://127.0.0.1:<ホストポート>` で届く）。`data/dev/` を消すと最初からやり直せる。
- バックアップは本番と同じく日次 03:00 JST に走り、システムの「定期処理」タブから手動でも実行できる。restic のパスワードは初回に `dev/serve.sh` が `data/dev/restic.key` に作る。消すと既存のバックアップが読めなくなるので、やり直すときは volume `koroku-dev-s3-data` も消す。
- ログの退避は、ローカルでは 1 時間ごと（毎時 10 分）に走る（本番の既定は 4 時間ごと）。`koroku` は起動時に、終わった時間枠までをすぐ退避するので、`docker compose restart koroku` のあと、アプリ詳細の「ログ」タブで過去ログの一覧とダウンロードを試せる。ダウンロードの URL は `http://127.0.0.1:9000/...` の presigned URL。
- アプリの volume のバックアップとリストアは、Docker が volume を置くディレクトリを `koroku` のコンテナから直接読み書きする。そのため `koroku` のコンテナは、`chown`・`dac_override`・`fowner`・`fsetid` の 4 つだけを ambient capability で持つ（`DEV_FILE_CAPS=1`。どれも Docker が既定でコンテナに与える範囲）。`docker info -f '{{.DockerRootDir}}'` が `/var/lib/docker` でない環境（snap 版の Docker など）では、リポジトリ直下の `.env`（git の管理外）に `KOROKU_DOCKER_ROOT=<その値>` を書く。Docker Desktop では volume が VM の中にあるので、volume のバックアップは失敗する（基盤のバックアップとログの退避は動く）。
- GitHub App を設定していないので、アプリ作成では「URL を直接指定」で public リポジトリの URL を入れる。手元のクローンを `file:///...` で指定することもできる（リポジトリのあるディレクトリの配下に限る。コンテナからはそこしか見えない）。
- Go のコードを変えたら `docker compose restart koroku` でビルドし直す。
- 別のバックエンドに向けるときは `KOROKU_URL=http://127.0.0.1:<port> docker compose up web` とする。
- `bin/dev` が先にキャッシュ用の volume を作っていると、Compose が「volume already exists but was not created by Docker Compose」と警告するが、同じキャッシュを共有しているだけで害はない。

`--insecure-no-auth` はログインを外す開発用のフラグ（到達できる相手は誰でも Docker を操作できるので、手元以外では使わない）。ループバックのホスト名（`*.localhost` を含む）でしか応答せず、Caddy が有効で `[caddy] listen_ip` がループバックでないときは起動を拒否する。`curl` で変更系の API を叩くときは `-H 'Content-Type: application/json'` を付ける。

Compose を使わずに動かすこともできる。

```sh
bin/dev dev/serve.sh                                   # koroku（設定は data/dev/config.toml）
bin/dev sh -c 'cd web && pnpm start --port 4200'       # Vite の開発サーバー
```

## 画面のスクリーンショット

```sh
bin/dev sh -c 'cd web && pnpm build && node mock/screenshots.mjs <名前の接頭辞>'
```

モック API（`web/mock/`）のデータで撮り、`web/tmp/shots/`（git の管理外）に出力する。画面を変える PR に添付するものは [CLAUDE.md](../CLAUDE.md) の「画面を変える PR」を参照。

## 配布用バイナリ

```sh
bin/dev make build    # フロントエンド込みの bin/koroku（linux/amd64）
```

- フロントエンドのソース（`web/app`、`web/public`、`web/config`、設定ファイル、lockfile）が前回の `make build` から変わっていなければ、`web/dist` をそのまま使い、Vite のビルドを省く。`pnpm test` など別のものが `web/dist` を書いたあとは、ビルドし直す（テスト用の開発ビルドを埋め込まないため）。必ずビルドし直すには `make clean` してから実行する。

## ディレクトリ構成

| パス | 内容 |
| --- | --- |
| `cmd/koroku/` | エントリポイント（`serve`、`check`、`restore-system`、`version`） |
| `internal/` | API、デプロイ、Compose、Caddy、GitHub、ドメイン、ログ、バックアップ、ストア、設定の点検などの各パッケージ |
| `web/` | Ember アプリ（管理 UI） |
| `deploy/` | 設定例、プロビジョニングスクリプト、systemd unit |
| `dev/`、`bin/dev`、`compose.yaml` | 開発用の Docker イメージ、その中でコマンドを実行するラッパー、手元で動かすための Compose 設定 |
| `docs/` | 設計、API、セットアップ、運用、開発の文書 |
| `testdata/` | サンプル Compose、golden file |
| `.claude-plugin/`、`plugins/` | アプリのリポジトリ向けの Claude Code プラグイン（マーケットプレイスの定義と Skill） |
