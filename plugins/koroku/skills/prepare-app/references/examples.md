# 例

## Rails + PostgreSQL

### よくある開発向けの Compose（このままでは koroku に向かない）

```yaml
services:
  web:
    build: .
    command: bin/rails server -b 0.0.0.0
    environment:
      RAILS_ENV: development          # koroku の画面で変えられない
      DATABASE_URL: postgres://postgres:password@db/app_development
    ports:
      - "3000:3000"
    volumes:
      - .:/rails                      # 書き込み可能な bind mount（bind_writable）
    depends_on:
      - db
  db:
    image: postgres:17
    container_name: app-db            # staging と本番で衝突する
    environment:
      POSTGRES_PASSWORD: password     # 秘密の直書き
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
```

### koroku 向けに直した Compose

```yaml
services:
  web:
    build: .
    environment:
      RAILS_ENV: ${RAILS_ENV:-production}
      DATABASE_URL: postgres://app:${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}@db:5432/app
      # 値は koroku の env ファイルから入る。ここに並べておくと、koroku が
      # 未設定のものを環境変数の画面とデプロイの前に知らせる
      SECRET_KEY_BASE: ${SECRET_KEY_BASE:?openssl rand -hex 64 で作る}
      APP_HOSTS: ${APP_HOSTS:?公開するホスト名（カンマ区切り）}
      RAILS_MAX_THREADS: ${RAILS_MAX_THREADS:-5}
    expose:
      - "3000"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:3000/up"]
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s
    depends_on:
      db:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully
    volumes:
      - storage:/rails/storage        # Active Storage のローカル保存先
  migrate:
    build: .
    command: bin/rails db:prepare
    environment:
      RAILS_ENV: ${RAILS_ENV:-production}
      DATABASE_URL: postgres://app:${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}@db:5432/app
    restart: "no"                     # 書かないと koroku が unless-stopped を付けて再起動し続ける
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17
    environment:
      POSTGRES_USER: app
      POSTGRES_DB: app
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U app -d app"]
      interval: 5s
      timeout: 3s
      retries: 20
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
  storage:
```

- `migrate` を分けず、`web` の起動コマンドの前で `bin/rails db:prepare` を実行する形でもよい（Rails 7.1 以降の `bin/docker-entrypoint` はそうなっている）。
- `db` の healthcheck は `-h 127.0.0.1` で TCP を確かめる。postgres イメージは初回の起動時に、UNIX ソケットだけで待ち受ける仮のサーバーで初期化してから再起動する。ソケットで確かめると、その間に healthy になり、`migrate` が接続に失敗することがある。
- healthcheck の `curl` はイメージに入っている必要がある。入っていなければ `CMD-SHELL` で `ruby -rnet/http -e ...` などにするか、healthcheck を省いて koroku の HTTP 確認に任せる。

### 手元の開発用（`compose.override.yaml`、koroku は読まない）

```yaml
services:
  web:
    command: bin/rails server -b 0.0.0.0
    environment:
      RAILS_ENV: development
    ports:
      - "3000:3000"
    volumes:
      - .:/rails
  migrate:
    environment:
      RAILS_ENV: development
  db:
    ports:
      - "5432:5432"
```

`POSTGRES_PASSWORD` は手元ではリポジトリ外の `.env`（`.gitignore` 済み）か、シェルの環境変数で渡す。

### Rails の設定

- `config/environments/production.rb`
  - `config.assume_ssl = true` と `config.force_ssl = true`。TLS は Caddy が終端し、`X-Forwarded-Proto: https` を付けて平文で転送する。`force_ssl` のリダイレクトは koroku の HTTP 確認では 3xx として通るが、`config.ssl_options = { redirect: { exclude: ->(request) { request.path == "/up" } } }` で `/up` を外しておくと確認の意味が保てる。
  - `config.hosts` を使うなら、環境変数から組み立てる: `config.hosts = ENV.fetch("APP_HOSTS", "").split(",")` と `config.host_authorization = { exclude: ->(request) { request.path == "/up" } }`（HTTP 確認は Host が `127.0.0.1:<port>` で届くため）。
- `SECRET_KEY_BASE`（または `RAILS_MASTER_KEY`）は koroku の環境変数で渡す。`bin/rails secret` で作る。
- `POSTGRES_PASSWORD` は URL に埋め込むので、記号を含まない値にする（`openssl rand -hex 32`）。
- Dockerfile は Rails 7.1 以降の `rails new` が生成するもの（マルチステージ、`assets:precompile` をビルド時に実行、非 root ユーザー）がそのまま使える。
- ダンプフック: サービス `db`、コマンド `pg_dump -U app app`。ファイルのバックアップから `pgdata` を外す。

## CI がビルドしたイメージを使う

小さい VPS でビルドせずに、GitHub Actions がコミットごとにビルドして GHCR に置いたイメージを pull する。koroku はデプロイするコミットを `KOROKU_COMMIT_SHA`（40 桁）と `KOROKU_COMMIT_SHORT`（先頭 7 桁）で補間に渡す。

```yaml
services:
  web:
    # koroku の外では変数がないので latest を指す
    image: ghcr.io/octo/app:${KOROKU_COMMIT_SHORT:+sha-}${KOROKU_COMMIT_SHORT:-latest}
    # pull_policy: always は書かない（koroku はデプロイのたびに pull する）
```

イメージを作るワークフロー（`.github/workflows/image.yml`）では、`docker/metadata-action` の `type=sha`（既定で `sha-` と 7 桁）でタグを付ける。40 桁の `KOROKU_COMMIT_SHA` を使うなら `type=sha,format=long` にする。

```yaml
on:
  push:
    branches: [main]
permissions:
  contents: read
  packages: write
jobs:
  image:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: meta
        uses: docker/metadata-action@v5
        with:
          images: ghcr.io/octo/app
          tags: |
            type=sha
            type=raw,value=latest,enable={{is_default_branch}}
      - uses: docker/build-push-action@v6
        with:
          push: true
          platforms: linux/amd64
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
```

- private なイメージなら、koroku の設定タブの「コンテナレジストリの認証」に読み取り専用のトークンを入れる。
- push での自動デプロイを使うなら、デプロイタブの「デプロイの設定」の「先に待つワークフロー」に `.github/workflows/image.yml` を入れる。入れないと push の直後にイメージがまだなく、デプロイログに `The registry has no image with that tag (manifest unknown)` と出て失敗する（稼働中の版はそのまま）。

## Node.js（Express、Next.js など）

- `HOST=0.0.0.0` / `PORT` で待ち受ける。Next.js の standalone 出力なら `HOSTNAME=0.0.0.0` が要る。
- `NODE_ENV: ${NODE_ENV:-production}`。`npm run dev` ではなく本番のビルドと起動コマンドにする。
- `app.set("trust proxy", 1)` などで `X-Forwarded-*` を信頼する（ループバックの Caddy からしか届かない）。

## Django

- `ALLOWED_HOSTS` と `CSRF_TRUSTED_ORIGINS` を環境変数から組み立てる。HTTP 確認用に `127.0.0.1` も許可するか、確認用のパスを許可リストの外に置く。
- `SECURE_PROXY_SSL_HEADER = ("HTTP_X_FORWARDED_PROTO", "https")`。
- `runserver` ではなく gunicorn などで起動し、静的ファイルは `collectstatic` をビルド時に実行して WhiteNoise などで配信する。

## 静的サイト

- ビルド済みのファイルを nginx や Caddy のイメージに入れる Dockerfile を書き、`web` として公開する。DB もダンプフックも要らない。
