# 個人用 PaaS（Heroku ライク）アーキテクチャ設計書

Nov 18, 2026 · @tricknotes

## 1. 目的とスコープ

個人用に 3〜5 個の Docker Compose アプリを、VPS 1 台の上で GitHub リポジトリから UI 操作だけでデプロイできるコントロールプレーンを作る。以下、本体デーモンを仮に `paasd`、ベースドメインを `dev.tricknotes.io` と書く。

**作るもの**

- GitHub リポジトリ連携（private 含む）と、UI からのブランチ／コミット指定デプロイ
- `compose.yaml` のデプロイ。`build:` があればホストでビルド、`image:` のみなら pull
- アプリごとの `<slug>.dev.tricknotes.io` 自動割当と TLS、アプリごとの独自ドメイン追加（本番用）
- 環境変数の管理、デプロイログと実行ログの閲覧
- 起動・停止・再起動・削除、過去コミットへの再デプロイ（ロールバック）

**作らないもの（初期）**

- `git push` デプロイ、Addons、buildpack
- マルチノード、マルチユーザー、オートスケール
- 無停止デプロイ（再作成中の数秒の断は許容）
- push 時の自動デプロイ

## 2. 決定事項サマリ

| 項目 | 決定 | 理由 |
| --- | --- | --- |
| 実行基盤 | さくらの VPS 1 台（Ubuntu 24.04、x86_64）。Docker Engine + Compose v2.24.4 以上 | ご回答どおり。override で使う `!override` タグの要件（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)） |
| コントロールプレーン | Go 製シングルバイナリ `paasd`。systemd でホストに直置き、SQLite | 常駐メモリが小さく、配布と更新がファイル 1 つ。詳細は 4 章 |
| フロントエンド | Ember SPA。ビルド成果物を `paasd` に埋め込んで配信 | ご希望どおり。配信用の別プロセスが不要 |
| リバースプロキシ | Caddy。ホストに直置きし、admin API で設定を投入 | ホスト名を渡すだけで証明書の取得と更新まで自動（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)） |
| アプリへの経路 | 公開サービスを `127.0.0.1:<割当ポート>` に publish し、Caddy がそこへ転送 | 共有 Docker ネットワークが不要で、アプリ同士が互いに見えない |
| TLS | ホスト名ごとに個別証明書（HTTP-01）。ワイルドカード「証明書」は見送り | ワイルドカード発行には DNS-01 が要り、DNS 事業者の API 連携が増える（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)） |
| GitHub 連携 | 自分専用の GitHub App。ログインと clone の両方に使う | clone 用トークンが 1 時間で失効し、長寿命の秘密を置かずに済む（5 章） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p paas-<slug>`） | プロジェクト名でリソースが分離される（[How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)） |
| ビルド | `build:` があればホストで build、なければ pull | ご回答どおり（両対応） |
| ジョブ実行 | `paasd` プロセス内キュー、全体で同時実行 1 | ビルドの負荷を直列化。Redis 等の外部依存が不要 |
| UI 認証 | GitHub ログイン + 許可ユーザー ID 1 件の照合 | 個人用途。パスワード管理を持たない |

## 3. 全体アーキテクチャ

常駐プロセスは Caddy と `paasd` の 2 つだけで、どちらもホストの systemd サービスとして動かす。アプリは Compose プロジェクトとして並び、外部への公開は Caddy 経由の 80/443 に限る。

```text
                      Internet
                         | 80 / 443
+------------------------v------------------------------- VPS --+
|  Caddy (systemd)                                              |
|    console.dev.tricknotes.io -> 127.0.0.1:8080  (paasd)       |
|    app-a.dev.tricknotes.io   -> 127.0.0.1:20001 (app-a / web) |
|    app-b.dev.tricknotes.io   -> 127.0.0.1:20002 (app-b / web) |
|    admin API: 127.0.0.1:2019                                  |
|                                                               |
|  paasd (Go, systemd)  listen 127.0.0.1:8080                   |
|    - REST API + SSE / Ember SPA (embed)                       |
|    - Deploy worker (同時実行 1)                                |
|    - SQLite  /var/lib/paas/paas.db                            |
|    - -> docker compose CLI (unix socket)                      |
|    - -> Caddy admin API  (POST /load)                         |
|    - -> GitHub API       (GitHub App)                         |
|                                                               |
|  Compose project: paas-app-a      Compose project: paas-app-b |
|    web    127.0.0.1:20001:3000      web    127.0.0.1:20002:80 |
|    db     (publish なし)             worker (publish なし)      |
|    volume paas-app-a_pgdata                                   |
+---------------------------------------------------------------+
```

**ディレクトリ配置**

| パス | 内容 |
| --- | --- |
| `/usr/local/bin/paasd` | 本体バイナリ（SPA 込み） |
| `/etc/paas/config.toml` | ベースドメイン、GitHub App ID、許可ユーザー ID |
| `/etc/paas/github-app.pem`、`/etc/paas/master.key` | GitHub App 秘密鍵、環境変数暗号化のマスターキー（0600） |
| `/var/lib/paas/paas.db` | SQLite |
| `/var/lib/paas/apps/<slug>/releases/<deploy_id>/` | チェックアウトしたソースと生成した override |
| `/var/lib/paas/apps/<slug>/env` | 展開済み環境変数ファイル（0600） |
| `/var/lib/paas/apps/<slug>/release-state/<deploy_id>/` | そのリリースの環境変数スナップショット（0600）と、使った Compose ファイル名・ビルドしたイメージ名。ロールバック用（7 章） |
| `/var/lib/paas/apps/<slug>/deploys/<deploy_id>.log` | デプロイログ |

## 4. 技術スタック選定

推奨は **Go（API）+ Ember（SPA）+ SQLite**。このシステムの中心は CRUD ではなく「外部プロセスの実行・ログのストリーム配信・状態機械」で、小さな VPS でアプリとメモリを分け合うため、常駐コストと配布の軽さを優先した。

| 観点 | Go | Rails | Rust |
| --- | --- | --- | --- |
| 常駐メモリ（未計測の目安） | 数十 MB | 数百 MB（Web + ジョブワーカー） | 数十 MB 以下 |
| 配布・更新 | バイナリ 1 つを差し替え | Ruby 実行環境かコンテナが必要 | バイナリ 1 つを差し替え |
| Docker との親和性 | 公式 SDK がある | 非公式ライブラリ | 非公式ライブラリ |
| プロセス実行・SSE | 標準ライブラリで足りる | 可能だがスレッド設計に注意 | async ランタイム前提で記述量が増える |
| 保守性 | 依存が少なく、言語仕様が安定 | ご自身の習熟度が最も高い | 学習コストとビルド時間が最大 |

- Docker Engine の公式 SDK は Go と Python のみ（[Docker Engine SDKs](https://docs.docker.com/reference/api/engine/sdk/)）。ただし本設計は `docker compose` CLI を呼ぶ方式を主とし、SDK は必須にしない。
- SQLite ドライバは cgo 不要の [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) を使い、クロスコンパイルと単一バイナリを保つ。
- Rails は次点。VPS が 4GB 以上で、ご自身が頻繁に手で直す前提なら逆転しうる。
- Rust は利点（メモリ）が Go との差で小さく、基盤に載せるには学習リスクが見合わないと判断した。

**構成の詳細**

- Go: 標準 `net/http`、`database/sql`、SQL マイグレーションは埋め込みファイルを起動時に適用。フレームワークは使わない。
- Ember: TypeScript。API は JSON の REST、ログは SSE（`EventSource`）で受ける。
- リポジトリ: `cmd/paasd`、`internal/{api,deploy,compose,caddy,github,store}`、`web/`（Ember）、`deploy/`（プロビジョニング）。

## 5. GitHub 連携

自分のアカウントに非公開の GitHub App を 1 つ登録し、UI ログインとリポジトリ取得の両方に使う。権限は Contents: Read-only と Metadata: Read-only のみ。

| 用途 | 方式 |
| --- | --- |
| UI ログイン | Web application flow でユーザーアクセストークンを得て `GET /user` の ID を許可 ID と照合し、自前のセッション Cookie を発行する。トークンは保存しない（[ユーザーアクセストークンの生成](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)） |
| リポジトリ・ブランチ・コミット一覧 | installation access token で REST API を呼ぶ |
| clone | デプロイのたびに installation access token を発行し、HTTP パスワードとして使う。形式は `https://x-access-token:TOKEN@github.com/owner/repo.git`、有効期限は 1 時間（[installation としての認証](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)） |

- トークンは `.git/config` やデプロイログに残さない。`GIT_ASKPASS` か `http.extraheader` で渡し、fetch 後に破棄する。
- Webhook は初期は受けない。push 時自動デプロイを足すときに `/webhooks/github` を追加する。

**ログインと保護の細則（M3 の実装で確定、2026-11-18）**

- セッションは 7 日で失効し、操作しても延長しない。Cookie の値は乱数 32 バイトで、DB には SHA-256 のハッシュだけを置く（DB が漏れてもセッションを再利用できないようにするため）。`config.toml` の許可ユーザー ID を変えると、既存のセッションは次のリクエストから無効になる。
- 変更系リクエスト（`POST` / `PUT` / `PATCH` / `DELETE`）は、`Origin` ヘッダーが管理 UI のオリジン（`console_url`、既定は `https://<console_host>`）と完全一致するものだけを通す。ヘッダーがないリクエストも拒否するので、`curl` で叩くときは `-H "Origin: https://console.<base>"` を付ける。
- GitHub App が未設定（`[github]` セクションなし）のときは、誰もログインできず API は 401 を返す。「未設定なら素通し」にはしない。ローカル開発では `paasd serve --insecure-no-auth` で認証と Origin 検証を外せる。設定ファイルではなく起動フラグにしたのは、本番の unit ファイルに紛れ込みにくくするため。
- `[github]` は全項目（`app_id`、`client_id`、`client_secret`、`private_key_file`、`allowed_user_id`）が揃っていなければ起動時にエラーにする。秘密鍵ファイルはグループや他人が読める権限なら拒否する。
- リポジトリの指定が `repo_full_name`（`owner/name`）のアプリは installation token を使って取得する。GitHub App がそのリポジトリにインストールされていなければ、public リポジトリの可能性があるので認証なしで取得を試みる。`repo_url` を明示したアプリには token を使わない。
- アプリ作成時の Compose ファイル検出（`GET /api/repos/{owner}/{repo}/compose`）は、サービス名とポートを画面に下書きするためのもので、YAML パーサーの依存を増やさず行ベースの簡易読み取りで済ませる（アンカーやフロースタイルは読めない。読めなければ手入力）。デプロイ時の判定は従来どおり `docker compose config` の結果で行う。
- 不採用案: fine-grained PAT。設定は簡単だが長寿命の秘密をサーバーに置くことになり、ログイン手段も別途必要になる。

## 6. Compose の扱い

リポジトリの `compose.yaml` は書き換えず、`paasd` が生成する override ファイルを `-f` で重ねて実行する。

**アプリごとの設定（UI で入力）**

- Compose ファイルのパス（既定 `compose.yaml`、なければ `docker-compose.yml`）
- 公開するサービス名（既定 `web`）とコンテナ内ポート
- ヘルスチェックパス（既定 `/`）

**生成する override（例）**

```yaml
# /var/lib/paas/apps/app-a/releases/42/paas.override.yaml
services:
  web:
    ports: !override
      - "127.0.0.1:20001:3000"
    env_file:
      - /var/lib/paas/apps/app-a/env
    restart: unless-stopped
  db:
    ports: !reset []
    env_file:
      - /var/lib/paas/apps/app-a/env
    restart: unless-stopped
```

- 公開サービスの `ports` は `!override` で割当ポートに置き換え、他サービスの `ports` は `!reset []` で消す。通常のマージでは `ports` が追記されてしまうため、この 2 つのタグが必要（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)）。
- ホストポートは 20000 番台からアプリ作成時に 1 つ割り当て、DB で一意に保つ。
- 環境変数は全サービスに `env_file` で注入する（Heroku の config vars と同じ感覚）。`${VAR}` 補間用に `--env-file` でも同じファイルを渡す。
- named volume はプロジェクト名 `paas-<slug>` が接頭辞になるため、リリースディレクトリが変わっても引き継がれる。

**override の細則（M1 の実装と実機確認で確定、2026-11-18）**

- `restart: unless-stopped` は、リポジトリ側が `restart` を指定していないサービスにだけ付ける。全サービスに付けると、マイグレーション用のワンショットサービス（`restart: "no"`）が終了後に再起動し続けてしまうため。
- `build:` を持つサービスには、リリースごとのイメージ名 `paas-<slug>-<service>:r<deploy_id>` を override の `image:` で与える。Compose の既定では毎回 `<project>-<service>:latest` に上書きされ、起動に失敗した新版のイメージで旧版のタグが置き換わるので、旧リリースのディレクトリで `up` し直しても壊れたイメージが起動した（結合テストで検出）。リリース固有のタグにしておくと、ロールバックは旧版のイメージそのものを起動できる。イメージはリリースディレクトリと一緒に削除する。
- メモリ上限の決まり方は、優先度の高い順に「アプリ設定のサービス別の値」「アプリ設定の全体値」「リポジトリの Compose が自分で指定した値」「`config.toml` の既定値（512m）」。リポジトリ側が `deploy.resources.limits.memory` で書いている場合は override も同じ形式で書く。`mem_limit` と値が食い違うと Compose が `can't set distinct values` で設定全体を拒否するため。
- override に書くサービスは、リポジトリの Compose を正規化した結果（`config --format json`）に存在するものだけにする。存在しないサービス名を書くと Compose がエラーにするため、正規化 → override 生成 → もう一度正規化してポリシー検査、の 2 回 `config` を実行する。
- 正規化した JSON には `env_file` の中身が展開されて入るので、ログには出さない。
- env ファイルの値は Compose に補間されない形で書く。英数字と一部の記号だけの値はそのまま、それ以外はシングルクォート、シングルクォートか改行を含む値はダブルクォートで `\ \" \$ \n` をエスケープする。`$`・空白・`#`・引用符を含む値がコンテナにそのまま届くことを結合テストで確認している。

**ビルドと pull の判定**

1. `docker compose -p paas-<slug> -f compose.yaml -f paas.override.yaml config --format json` で正規化する。
2. `build` を持つサービスがあれば `docker compose build`、`image` のみのサービスは `docker compose pull`。
3. private レジストリは、初期はホストで `docker login` 済みであることを前提にする。

**ポリシー検査（正規化した JSON に対して実施、違反はデプロイ拒否）**

- `privileged: true`、`network_mode: host`、`pid: host`
- `/var/run/docker.sock` のマウント
- リリースディレクトリ外への bind mount
- 公開サービス名が Compose に存在しない

個人用途なので、アプリ設定の「検査を緩める」フラグで個別に許可できるようにする。

ただし「公開サービス以外のポート公開」と「公開サービスの `127.0.0.1:<割当ポート>` 以外への公開」は、フラグを立てても許可しない。Docker が publish したポートは ufw を通らずに外へ届くので（10 章）、override が効いていることの最終確認として常に検査する。bind mount のパスはシンボリックリンクを解決してから判定する（リポジトリ内のリンクでホストのディレクトリを持ち込めないようにするため）。

## 7. デプロイフロー

デプロイは `queued → running → succeeded | failed | canceled` の状態を持つ 1 ジョブで、次の順に進む。

1. **受付**: UI でブランチかコミットを選ぶ。API がコミット SHA を確定し、`deployments` に `queued` で記録する。
2. **取得**: installation token を発行し、`releases/<deploy_id>/` に `git fetch --depth 1 origin <sha>` でチェックアウトする。
3. **生成**: env ファイル（0600）と `paas.override.yaml` を書き出す。
4. **検証**: `docker compose config --format json` を実行し、6 章のポリシー検査をかける。
5. **ビルド／pull**: 6 章の判定に従う。
6. **起動**: `docker compose up -d --remove-orphans --wait --wait-timeout 300`。`--wait` はサービスが running または healthy になるまで待つ（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。
7. **経路**: Caddy 設定を再生成して投入する（初回と設定変更時）。
8. **確認**: `http://127.0.0.1:<port><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒リトライする。
9. **完了**: `succeeded` にして `current_deployment_id` を更新。リリースディレクトリは直近 3 件を残し、`docker image prune -f` を実行する。

**失敗時**

- 手順 5 までの失敗は稼働中のコンテナに触れないので、`failed` を記録するだけ。
- 手順 6 以降の失敗は、直前の成功リリースのディレクトリで `up` し直して旧版に戻す。
- `paasd` 再起動時に `running` のまま残ったジョブは `failed (interrupted)` にする。
- 直前の成功リリースがない（初回デプロイの）場合は戻す先がないので、`docker compose down` で起動しかけたコンテナを消す（volume は残す）。
- ロールバックでは環境変数も戻す。env ファイルはリリースごとにスナップショットを取り（`release-state/<deploy_id>/env`）、稼働中の `apps/<slug>/env` を書き換えるのは手順 6 の直前にする。こうすると、手順 5 までの失敗では稼働中のリリースの env に触れず、手順 6 以降の失敗では旧版を「当時の環境変数」で起動し直せる。環境変数の変更が原因で起動に失敗した場合にも旧版に戻れるようにするため。
- `paasd` の停止（SIGTERM）と重なったデプロイは `failed (interrupted)` にし、停止処理の中ではロールバックを走らせない。コンテナが中途半端な状態で残りうるので、`paasd` の更新はデプロイ中を避ける。
- `docker compose up --wait` は、ヘルスチェックを持たないコンテナが起動直後に落ちて再起動を繰り返していても成功を返した（実機確認、Compose v5.8.0）。起動失敗の検出は手順 8 の HTTP 確認が担う。

**その他の規則**

- 同時実行は全体で 1。同じアプリに後続のデプロイが積まれたら、古い `queued` は `canceled` にする。
- 各手順の stdout/stderr を行単位でログファイルに追記し、SSE で配信する。環境変数の値とトークンはマスクする。
- ロールバックは「過去の SHA を同じフローで再デプロイ」。DB マイグレーションの巻き戻しは行わない。
- マイグレーション等のリリース処理は Compose 側で表現する（起動コマンド、またはワンショットのサービスと `depends_on`）。基盤側に専用機能は持たない。

**デプロイ時の断の範囲**: 断はデプロイ対象のアプリだけに起きる。`docker compose up` は `-p paas-<slug>` で指定したプロジェクトのコンテナしか操作せず、その中でも設定かイメージが変わったサービスだけを再作成する（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。他アプリへの間接的な影響は 2 つある。

- ビルド中の CPU・メモリ負荷で他アプリの応答が遅くなりうる（停止はしない）。8.2 のメモリ上限とビルド直列化で抑える。
- Caddy 設定の再投入はアプリ作成時とドメイン・ポート変更時だけ行い、通常の再デプロイでは行わない。再投入時に他アプリの接続（特に WebSocket や SSE）が切れないことは M2 で実機確認する。確認結果は 12 章。SSE と通常のリクエストは切れず、WebSocket は 8 章の `stream_close_delay` で猶予を持たせている。

## 8. ルーティングと TLS

DNS にワイルドカードの A レコードを 1 本置き、証明書は Caddy がホスト名ごとに自動取得する。

- **DNS**: `*.dev.tricknotes.io` を VPS の IP に向ける。アプリは `<slug>.dev.tricknotes.io`、管理 UI は `console.dev.tricknotes.io`。
- **証明書**: Caddy は設定にホスト名があると自動 HTTPS を有効にし、証明書の取得・更新と HTTP→HTTPS リダイレクトを行う（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)）。
- **発行上限**: Let's Encrypt は登録ドメインあたり 7 日間で 50 枚まで（[Rate Limits](https://letsencrypt.org/docs/rate-limits/)）。3〜5 アプリでは問題ないが、開発中は staging 環境を使う。
- **設定投入**: `paasd` が DB から Caddy の JSON 設定全体を生成し、admin API の `POST /load` で置き換える（[API Tutorial](https://caddyserver.com/docs/api-tutorial)）。差分更新はしない。`paasd` 起動時にも必ず実行し、DB を唯一の正とする。
- **停止中アプリ**: ルートは残し、503 の固定ページを返す。

**Caddy 設定の細則（M2 の実装と実機確認で確定、2026-11-18）**

- サーバーは `:80` 用と `:443` 用の 2 つに分ける。1 つのサーバーに両方のポートを持たせると、`http://<slug>.<base>` へのアクセスが HTTPS へリダイレクトされず平文のままアプリへ転送された（Caddy が自動リダイレクトを挿入する位置が、ホスト別ルートの後ろになるため）。分けた構成では HTTP は 308 で HTTPS へ、未知のホスト名は `:80` で 404、`:443` では TLS ハンドシェイク失敗になる。
- 一度もデプロイしていないアプリも、停止中アプリと同じ 503 の固定ページにする（転送先がまだないため）。
- 設定の投入は、DB から生成した内容が前回投入したものと同じなら省く。これで通常の再デプロイでは Caddy に触れない。`paasd` 起動時だけは必ず投入する（Caddy 側は同一内容なら `config is unchanged` として何もしない）。投入に失敗したら次回必ず投入し直し、5 分ごとの定期処理でも再試行するので、Caddy が後から起動しても追いつく。
- アプリへの `reverse_proxy` には `stream_close_delay: 5m` を付ける。付けないと、設定を再投入した瞬間に、他アプリを含む WebSocket などのアップグレード済み接続が切れた（下の実機確認）。この指定には Caddy 2.7 以上が要るので、Ubuntu 標準の apt パッケージではなく Caddy 公式リポジトリのものを入れる（プロビジョニングスクリプトで対応）。
- アクセスログはアプリごとに `<access_log_dir>/<slug>.log`（既定 `/var/log/caddy/apps`）へ出す。`paasd` のデータディレクトリは root 専用（0700）で Caddy のユーザーが書けないため、置き場所を分けた。ロガー名は `http.log.access.<slug>.app` とする。Caddy はロガー名を前方一致で選ぶので、slug そのままだと `shop-staging` のログが `shop.log` にも入るため。管理 UI と未知のホスト名のアクセスログは出さない。
- Caddy の admin API は認証がなく設定全体を書き換えられるので、`caddy.admin_url` はループバックアドレスしか受け付けない。

**Squarespace Domains での設定**: `tricknotes.io` の DNS Settings → Custom Records に A レコードを追加する（[Adding DNS records to your domain](https://support.squarespace.com/hc/articles/360002101888)）。Host に `*.dev` を指定できるかは未確認。できない場合は `console.dev` とアプリごとの `<slug>.dev` を個別に登録する。証明書はホスト名ごとに取る設計なので、どちらでも `paasd` 側の実装は変わらない。

ワイルドカード証明書は DNS-01 チャレンジでのみ発行できる（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)）。個別証明書だとサブドメイン名が証明書の公開ログに載る点が気になる場合は、後から DNS 事業者の API 連携を足して切り替える。

### 8.1 独自ドメイン

アプリごとに任意個の独自ドメインを UI から追加できる。自動割当の `<slug>.dev.tricknotes.io` は常に残り、独自ドメインはそこに足される別名として扱う。

1. **登録**: UI でホスト名（例 `example.com`、`www.example.com`）を入力する。状態は `pending`。
2. **DNS 設定（利用者が実施）**: サブドメインは `<slug>.dev.tricknotes.io` への CNAME、apex は VPS の IP への A レコード。UI に設定すべき値を表示する。
3. **検証**: `paasd` がホスト名を名前解決し、`config.toml` の `public_ip` と一致したら `active` にする。「再検証」ボタンと 5 分間隔の自動再試行を持つ。
4. **反映**: `active` のホスト名だけを Caddy 設定に含めて `POST /load` する。証明書は自動割当ドメインと同じく Caddy がホスト名ごとに取得する。

- 検証前のホスト名を Caddy に渡さないのは、DNS が向いていない状態で証明書発行を試み続けるのを避けるため。
- 検証は「名前解決で得た全アドレス（A・AAAA とも）が `public_ip` と一致すること」を条件にする。正しい A レコードに加えて別ホストを指す AAAA が残っていると、一部の通信や証明書の検証がそちらへ向かうため。
- 5 分間隔の自動再試行は `pending` のものだけを対象にする。`active` になったドメインは、後で DNS が変わっても自動では `pending` に戻さない（一時的な名前解決の失敗で本番ドメインを止めないため）。「再検証」ボタンを押したときは結果どおりに更新する。
- プライマリにできるのは `active` のドメインだけ。プライマリのドメインが `pending` に戻ったら、プライマリ指定も外れる（リダイレクト先が応答しなくなるのを避けるため）。
- 追加した直後に 1 回検証するので、DNS を先に設定してあれば待たずに `active` になる。
- CDN を前段に置くなど IP が一致しない構成のために、検証を飛ばす「強制有効化」を用意する。
- アプリごとに「プライマリドメイン」を 1 つ選べる。選んだ場合、他のホスト名へのアクセスはプライマリへ 308 リダイレクトする（`www` ありなしの統一用）。
- ホスト名は全アプリで一意。`*.dev.tricknotes.io` 配下は独自ドメインとして登録できない。

**追加するデータと API**

| 種別 | 内容 |
| --- | --- |
| テーブル `app_domains` | `id`, `app_id`, `hostname`（一意）, `status`（pending / active）, `is_primary`, `force_active`, `last_checked_at`, `last_error` |
| `GET / POST /api/apps/{slug}/domains` | 一覧、追加 |
| `POST /api/apps/{slug}/domains/{id}/verify` | 再検証 |
| `PATCH / DELETE /api/apps/{slug}/domains/{id}` | プライマリ指定・強制有効化、削除 |
| UI | アプリ詳細に「ドメイン」タブ。状態、設定すべき DNS レコード、直近のエラーを表示 |

実装は M2（Caddy 設定生成に複数ホスト名とリダイレクトを含める）と M4（ドメインタブ）に入れる。受け入れシナリオに「独自ドメインを追加し、DNS 設定後に HTTPS で応答する」を加える。

### 8.2 staging と本番の分け方

staging と本番は同じ VPS に置き、「環境ごとに別アプリ」として分ける。環境という概念を基盤に足さず、同じリポジトリを複数のアプリに紐づけられるようにするだけで済む。

|  | staging アプリ | 本番アプリ |
| --- | --- | --- |
| slug の例 | `myapp-staging` | `myapp` |
| URL | `myapp-staging.dev.tricknotes.io` のみ | 独自ドメイン（プライマリ）+ `myapp.dev.tricknotes.io` |
| デプロイ元 | 任意のブランチ | `main` のコミット |
| 環境変数・volume | アプリ単位で独立 | アプリ単位で独立 |

- `apps` に表示用の `environment`（production / staging）列を足し、一覧でラベル表示する。本番アプリのデプロイと削除には確認ダイアログを出す。
- 同居の弱点は、staging のビルド負荷やディスク枯渇が本番に波及すること。ビルドの同時実行 1、サービスごとのメモリ上限（override で既定値を付与、アプリ設定で変更可）、ディスク使用率 85% 超でデプロイ拒否、の 3 つで抑える。
- より強い分離が要るようになったら、同じプロビジョニングスクリプトで 2 台目を立てて `paasd` をもう 1 組動かす。マルチノード対応はしない。

**本番利用で残る制約**: デプロイ時にコンテナ再作成による数秒の断がある。VPS 1 台なのでホスト障害時は全アプリが止まる。デプロイ時の断はデプロイ対象のアプリに限られる（7 章）。ホスト障害時の全停止は許容すると決定済み（2026-11-18）。

## 9. データモデル・API・UI

テーブルは 4 つで足りる。

| テーブル | 主なカラム |
| --- | --- |
| `apps` | `id`, `slug`（一意）, `repo_id`, `repo_full_name`, `default_branch`, `compose_path`, `web_service`, `web_port`, `host_port`（一意）, `healthcheck_path`, `desired_state`（running / stopped）, `relax_policy`, `current_deployment_id` |
| `env_vars` | `app_id`, `key`, `value_encrypted`（`app_id` + `key` で一意） |
| `deployments` | `id`, `app_id`, `ref`, `commit_sha`, `commit_message`, `status`, `error`, `queued_at`, `started_at`, `finished_at` |
| `sessions` | `token_hash`, `github_user_id`, `expires_at` |

**API（すべてセッション Cookie 必須、`/auth/*` を除く）**

| メソッドとパス | 用途 |
| --- | --- |
| `GET /auth/github/login`、`GET /auth/github/callback`、`POST /auth/logout` | ログイン・ログアウト |
| `GET /api/session` | ログイン状態 |
| `GET /api/repos`、`GET /api/repos/{owner}/{repo}/branches`、`GET /api/repos/{owner}/{repo}/commits?ref=` | リポジトリとデプロイ対象の選択 |
| `GET /api/apps`、`POST /api/apps`、`GET / PATCH / DELETE /api/apps/{slug}` | アプリの CRUD。削除は `?volumes=true` で volume も消す |
| `GET / PUT /api/apps/{slug}/env` | 環境変数（一括置換） |
| `GET / POST /api/apps/{slug}/deployments` | 履歴、デプロイ開始（`{ref}` か `{sha}`） |
| `GET /api/deployments/{id}`、`GET /api/deployments/{id}/logs`（SSE）、`POST /api/deployments/{id}/cancel` | デプロイ詳細とライブログ |
| `POST /api/apps/{slug}/start`、`/stop`、`/restart` | 起動制御 |
| `GET /api/apps/{slug}/services`、`GET /api/apps/{slug}/logs?service=&tail=`（SSE） | `compose ps` 相当と実行ログ |
| `GET /api/system` | ディスク・メモリ使用量、Docker バージョン |

**UI 画面**

- アプリ一覧: 状態、URL、最終デプロイのコミットと時刻
- アプリ作成: リポジトリ選択 → Compose ファイル検出 → 公開サービスとポートの指定
- アプリ詳細: 概要／デプロイ（ブランチ・コミット選択、履歴、ライブログ）／ログ／環境変数／設定
- システム: リソース使用量、手動 prune

### 9.1 管理 UI の再設計に伴う API の追加（2026-11-18）

管理 UI を「上部バー + アプリ一覧（ホストの状態を常時表示）+ アプリ詳細（ヘッダーと横タブ: 概要 / デプロイ / ログ / 環境変数 / ドメイン / バックアップ / 設定）+ システム」の構成に作り直す。画面構成の要件に対して、ここまでの API では取れない情報があるので、次を追加する。形は `docs/API.md` に書く。

| 画面の要素 | 不足していたもの | 追加する API |
| --- | --- | --- |
| アプリ一覧のホスト状態行、警告バナー | ログ退避の最終成功時刻、ディスク警告のしきい値、Caddy への設定投入の状態 | `GET /api/system` に `disk_limit_percent`、`log_archive`（`configured`、`last_success_at`、`last_error`）、`caddy`（`enabled`、`last_loaded_at`、`last_error`）を足す |
| アプリ一覧の行、詳細ヘッダー | プライマリ URL、検証待ちドメインの有無 | アプリの JSON に `primary_url`（プライマリドメインがあればその URL、なければ自動割当の URL）と `pending_domains`（検証待ちの数）を足す。一覧のためにアプリごとにドメイン API を呼ばずに済ませる |
| 概要タブのサービス一覧 | メモリ使用量と上限 | `GET /api/apps/{slug}/services` の各サービスに `memory_bytes` と `memory_limit_bytes` を足す（`docker stats --no-stream` の値。取れなければ `null`） |
| デプロイタブの手順表示 | 手順の区切りと、手順ごとの所要時間 | デプロイログの手順の見出し行を `==> <RFC 3339 の時刻> <手順のキー> <表題>` の形にする。UI はこの行でログを手順に分け、次の見出し（または終了）までを所要時間とする。手順のキーは `fetch` `prepare` `validate` `build` `pull` `backup` `start` `route` `check` `done` と、失敗時の `failed` `rollback`。DB に手順の表は持たない（ログファイルが唯一の記録で、SSE の再接続とも整合する） |
| バックアップタブ | 次回の実行予定、スナップショットのサイズ | `GET /api/backups` に `next_run_at`、スナップショットに `size_bytes`（restic が記録していれば。なければ `null`）を足す |
| システム画面 | Docker のディスク内訳 | `GET /api/system/docker`（`docker system df` の結果: イメージ、コンテナ、volume、ビルドキャッシュそれぞれの合計と回収可能な量）。数秒かかりうるので `/api/system` とは分ける |

バックアップタブ自体の API（一覧、実行、リストア、ダンプのダウンロード）は 10.2 の内容を M5 で `docs/API.md` に定義済み。「保存して再デプロイ」は環境変数の `PUT` とデプロイの `POST` を UI が続けて呼ぶだけで、専用の API は作らない。状態の自動更新はポーリング（一覧と詳細は数秒間隔、デプロイ中のログは SSE）で行い、状態変化を配信する専用の SSE は作らない（アプリ 3〜5 個の規模では不要）。

## 10. セキュリティと運用

`paasd` は Docker ソケットを操作できるためホストの root 相当であり、管理 UI の認証がこのシステムの唯一の関門になる。

**セキュリティ**

- **信頼モデル**: デプロイするのは自分のリポジトリだけ。他人のコードを動かす前提の隔離（gVisor 等）は持たない。
- **認証**: GitHub ログイン後、ユーザー ID が `config.toml` の許可 ID と一致した場合のみセッションを発行する。Cookie は `HttpOnly`、`Secure`、`SameSite=Lax`。変更系 API は `Origin` ヘッダーを検証する。
- **待受**: `paasd` と Caddy admin API は 127.0.0.1 のみ。外部公開は 22、80、443。
- **ポート公開の抑止**: Docker が publish したポートの通信は ufw の設定を通らずに届く（[Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)）。ufw に頼らず、6 章の override で全サービスの `ports` を消すか 127.0.0.1 に限定する。
- **シークレット**: 環境変数の値は AES-256-GCM で暗号化して SQLite に保存する。マスターキーは `/etc/paas/master.key`。UI は既定でマスク表示。

**運用**

- **コンテナログ**: Docker の `json-file` ドライバを `max-size=10m`、`max-file=3` でローテートする（`daemon.json`）。長期保管は 10.1 の収集機構が担う。
- **ディスク**: デプロイ成功後に `docker image prune -f`、週 1 回 `docker builder prune --filter until=168h`。使用率はシステム画面に出す。
- **メモリ**: ビルド時のピークに備えて swap を 2GB 置く。VPS は 2 vCPU、4GB RAM、40GB ディスクを出発点にする（未計測の目安）。
- **バックアップ**: 基盤の設定とアプリのデータを日次で R2 に退避する（10.2）。
- **paasd の更新**: バイナリを差し替えて `systemctl restart paasd`。アプリのコンテナは影響を受けない。
- **起動制御と削除（M5 の実装で確定、2026-11-18）**
  - 停止は `docker compose stop`、起動は稼働中リリースのファイルで `up -d --wait`（設定を確実に揃えるため。リリースのファイルが残っていなければ `docker compose start`）、再起動は `docker compose restart`。停止・起動のたびに Caddy 設定を再投入し、停止中は 503 の固定ページになる。
  - デプロイと手動操作はアプリ単位で直列化する。デプロイが `queued` か `running` のアプリへの操作は 409（`busy`）で断り、操作の実行中に始まったデプロイは操作の完了を待つ。同じアプリに対して `docker compose` を同時に走らせないため。
  - 削除は `docker compose down`（`?volumes=true` なら `--volumes` に加え、プロジェクトのラベルが付いた volume を明示的に消す）→ リリース用イメージの削除 → DB の行とアプリのディレクトリの削除 → Caddy 設定の再投入、の順。`down` に失敗したら DB には触れない。volume を残して削除したあとに同じ slug でアプリを作り直すと、プロジェクト名が同じなので残した volume がそのまま使われる（結合テストで確認）。
  - `docker builder prune --filter until=168h` は毎日実行する。対象は「1 週間使われていないビルドキャッシュ」なので効果は週 1 回と同じで、`paasd` を再起動しても実行の機会を逃さない。
  - Caddy 設定は 5 分ごとに、内容が変わっていなくても投入し直す。Caddy が単独で再起動すると投入済みの設定を忘れるため。同一内容の投入は Caddy 側で何もしないので負荷にならない（12 章の確認結果）。
- **プロビジョニング**: 新規 VPS を 1 本のスクリプトで構築する（Docker、Caddy、`paasd` の unit、swap、ログ設定）。

### 10.1 ログ収集

ログは `paasd` 内蔵のコレクタがファイルに蓄積し、4 時間ごとに R2 に退避する。Loki などのログ基盤は、常駐メモリが増えるので 3〜5 アプリの規模では採用しない。

| 項目 | 仕様 |
| --- | --- |
| 対象 | 全サービスの stdout / stderr、Caddy のアクセスログ（アプリ別）、デプロイログ |
| 収集 | アプリごとに `docker compose logs --follow --timestamps --no-color` を読み続け、`/var/lib/paas/logs/<slug>/<service>/YYYY-MM-DD.log` に追記 |
| 再開 | `paasd` 再起動時は最後に書いた時刻を `--since` に渡す。直近分は Docker 側に残っているので短い停止は埋まる |
| ローカル保持 | 14 日。日付が変わったファイルは gzip 圧縮 |
| R2 への退避 | 4 時間ごと（00:10 JST 起点）に、前回以降の差分を gzip して `logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz` へアップロード |
| R2 側の保持 | プレフィックス `logs/` に 90 日で削除するライフサイクルルールを設定（[Object lifecycles](https://developers.cloudflare.com/r2/buckets/object-lifecycles)） |
| 閲覧 | ライブ tail（SSE）。過去 14 日は日付・サービス・キーワードで絞り込み。それ以前は presigned URL でダウンロード（[Presigned URLs](https://developers.cloudflare.com/r2/api/s3/presigned-urls/)） |

- API: `GET /api/apps/{slug}/logs?service=&from=&to=&q=`、`GET /api/apps/{slug}/logs/archives`、`GET /api/apps/{slug}/logs/archives/url?key=`。
- 制約: 直近の区間は VPS 上にしかない。ホストを失うと最大 4 時間分のログが消える。間隔は設定値（既定 4 時間、最短 1 時間）。1 回の処理はサービスごとに差分テキストの圧縮と 1 オブジェクトのアップロードだけで、常駐負荷は増えない。

### 10.2 バックアップ

バックアップは restic で R2 に取る。R2 は S3 互換 API を持ち（[S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)）、restic は S3 互換ストレージをリポジトリにできる（[restic documentation](https://restic.readthedocs.io/)）。重複排除・暗号化・世代管理を自前で書かずに済む。

| 対象 | 方法 |
| --- | --- |
| 基盤（`paas.db`、`/etc/paas`） | SQLite は `VACUUM INTO` で整合したコピーを作ってから `restic backup` |
| アプリの named volume | `docker volume inspect` で得たマウントポイントを稼働中のまま `restic backup`。アップロードファイルなど向け |
| アプリの DB | アプリ設定の「ダンプフック」（サービス名 + コマンド。例: `db` / `pg_dump -U postgres app`）を `docker compose exec -T` で実行し、標準出力を `restic backup --stdin` に流す（[restic manual](https://restic.readthedocs.io/en/stable/manual_rest.html)） |

- 稼働中の DB のデータディレクトリをファイルコピーしても整合性は保証されない。DB はダンプフックで取り、その volume はアプリ設定で対象から外せるようにする。
- **スケジュール**: 毎日 03:00 JST。UI から手動実行もできる。「デプロイ前に取得」はアプリ単位のオプション（既定オフ）。
- **世代**: `restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune`。スナップショットには `app:<slug>` と `kind:system|volume|dump` のタグを付ける。
- **監視**: 週 1 回 `restic check`。最終成功時刻を UI に出し、24 時間以上成功がなければ警告表示する。外部通知は後回し。
- **バケット**: 1 バケットを `restic/` と `logs/` に分ける。ライフサイクルルールは `logs/` だけに掛ける（`restic/` を期限削除するとリポジトリが壊れる）。
- **S3 への切替**: `config.toml` の endpoint・region・バケット名を変えるだけにする。R2 固有の API は使わない。

**リストア**

1. volume: UI でスナップショットを選ぶ → アプリ停止 → 現在の中身を `/var/lib/paas/restore-trash/` に退避（7 日保持）→ `restic restore` → アプリ起動。
2. DB ダンプ: UI からダウンロードするところまでを提供する。流し込みは手動（手順を画面に表示）。
3. ホスト全損: 新しい VPS をプロビジョニング → `paasd restore-system` で `paas.db` と `/etc/paas` を戻す → 各アプリを再デプロイして volume をリストア → DNS を新 IP に向ける。

**事前準備と秘密情報**

- R2 にバケットを作り、そのバケットに限定した API トークン（Access Key ID / Secret Access Key）を発行して `/etc/paas/config.toml` に置く。
- restic のリポジトリパスワードは `/etc/paas/restic.key`。これと R2 の認証情報は VPS の外（パスワードマネージャなど）にも必ず控える。失うとホスト全損時に復元できない。

実装は M5。受け入れシナリオに「バックアップ取得 → volume を空にする → リストアでデータが戻る」と「4 時間以上前のログが R2 にあり UI からダウンロードできる」を加える。

## 11. 実装フェーズ（Claude Code 向け）

> この章は初版の計画。独自ドメイン・ログ・バックアップを含む最新のマイルストーンと受け入れシナリオは、リポジトリ直下の `CLAUDE.md` を正とする。

デプロイエンジンを先に固め、UI は最後に載せる。各フェーズは単体で動作確認できる状態で終える。

| フェーズ | 内容 | 完了条件 |
| --- | --- | --- |
| M0 骨格 | リポジトリ構成、設定読込、SQLite マイグレーション、Ember の空アプリを embed 配信、CI | `paasd` が起動し `/api/system` が応答する |
| M1 デプロイエンジン | public リポジトリの URL と SHA を受け、取得 → override 生成 → 検証 → build/pull → up → 確認。ログファイル出力 | サンプル 2 種（`build:` あり／`image:` のみ）が API 経由で起動し、失敗時に旧版へ戻る |
| M2 ルーティング | ポート割当、Caddy 設定生成と `POST /load`、TLS（staging） | `https://<slug>.<base>` で応答する |
| M3 GitHub App | ログイン、セッション、リポジトリ／ブランチ／コミット一覧、private clone | 許可 ID 以外はログイン不可。private リポジトリをデプロイできる |
| M4 UI | 一覧、作成、詳細、デプロイと SSE ライブログ、環境変数 | ブラウザ操作だけで新規アプリを公開できる |
| M5 運用 | 実行ログ、start/stop/restart、削除、prune、システム画面、プロビジョニング、バックアップ | 新規 VPS がスクリプト 1 本で立ち上がる |

**テスト方針**

- ユニット: override 生成、ポリシー検査、Caddy 設定生成、状態遷移、ログのマスク。入力と出力がファイルで決まる部分は golden file で固定する。
- 結合: 実 Docker を使い、サンプル Compose をデプロイして HTTP 応答まで確認する。GitHub と Caddy はインターフェースで差し替える。
- 外部コマンド呼び出しは 1 つの `Runner` インターフェースに集約し、引数をテストで検証できるようにする。

**受け入れシナリオ**

1. private リポジトリの Rails + PostgreSQL の Compose を UI からデプロイし、`https://<slug>.<base>` が応答する。
2. 環境変数を変更して再デプロイすると反映され、DB のデータは残る。
3. 起動に失敗するコミットをデプロイすると `failed` になり、旧版が応答し続ける。
4. アプリを 2 つ並べても、互いの DB ポートがホスト外から到達できない。

## 12. 未決事項

実装開始の前提は揃った。残りは既定案のまま進め、該当フェーズで確認する。

**決定済み（2026-11-18）**

- ベースドメインは `dev.tricknotes.io`。DNS は Squarespace Domains で管理する。
- VPS はさくらの VPS、OS は Ubuntu 24.04、アーキテクチャは x86_64。標準 OS の Ubuntu は amd64 のみ提供されている（[サービス仕様](https://vps.sakura.ad.jp/vps_resource/pdf/spec.pdf)）。
- private な GHCR イメージは、ホストで `docker login` しておく方式とする。

**契約時の注意**: さくらの VPS は Linux カーネル脆弱性への対応で一部の受付を止めている。2026-11-13 追記時点で、大阪リージョンの Linux 通常容量プランの新規申し込みと、スケールアップが停止中（[対応状況](https://vps.sakura.ad.jp/contents/info/202610-pendingapply/)）。東京か石狩で、後から上げられない前提で最初から 4GB プランを選ぶ。構築直後にカーネルを更新する。

**残り**

- [ ] Squarespace の Custom Records で Host `*.dev` が登録できるか（M2 で確認。不可なら個別 A レコード）
- [x] `--env-file` 指定時にリポジトリ内の `.env` がどう扱われるか（M1 で実機確認済み、下記）
- [ ] バックアップとログの保持期間（既定案: バックアップは日次 7・週次 4・月次 6、ログは R2 に 90 日）
- [ ] 起動失敗時の自動ロールバックを既定で有効にするか（既定案: 有効）
- [ ] push 時の自動デプロイを将来入れるか（入れるなら Webhook の受け口を M3 で用意する）

**実機確認の結果**

- `--env-file` とリポジトリ内の `.env`（2026-11-18、Docker 29.8.0 / Compose v5.8.0、ローカル環境）: `--env-file` を指定すると、プロジェクトディレクトリの `.env` は `${VAR}` の補間に使われない（`.env` にしかない変数は未定義扱いになり、`${VAR:-default}` は default になる）。一方、サービスが自分で `env_file: .env` と書いている場合、その内容はコンテナの環境変数に入る。override で足した `paasd` の env ファイルはリストの後ろに付くので、同じ名前の変数は `paasd` 側の値が勝つ。つまり、補間は `paasd` の環境変数だけで決まり、リポジトリの `.env` は「サービスが明示的に読む」ときだけ効く。`testdata/apps/build-app` と結合テストで固定している。

- 設定再投入時の接続（2026-11-18、Caddy 2.11.9 の公式 Docker イメージ、ローカル環境。**VPS 上では未確認**）: SSE の `curl -N` と、`Upgrade: websocket` で 101 を受けたあと 1 秒ごとに 1 行流す接続を保ったまま `POST /load` した。
  - 同一内容の投入は Caddy が `config is unchanged` として何もしない。
  - 内容が変わる投入（アプリを 1 つ追加）と強制再読込では、サーバーが作り直されるが、SSE は 24 秒間の観測で 1 行も欠けなかった（平文、および TLS + HTTP/2 のどちらでも）。新しいルートは投入の 1 秒後には応答した。
  - アップグレード済み接続は、`stream_close_delay` なし（Caddy の既定）だと再読込の瞬間に切れた。`stream_close_delay: 10s` では再読込の 10.0 秒後に切れた。つまりこの指定は切断をなくすのではなく先送りする。生成する設定では 5 分にしているので、アプリの作成・ドメインやポートの変更・起動や停止のたびに、他アプリの WebSocket は最長 5 分後に 1 回切れる（クライアントの再接続に任せる）。5 分ちょうどで切れるところまでは待って確認していない。
  - 実際の ACME 発行（Let's Encrypt staging を含む）は VPS と DNS が要るので未確認。

## 出典

- [Compose: Merge](https://docs.docker.com/reference/compose-file/merge/)（`!reset`、`!override`）
- [Compose: How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)（プロジェクト名）
- [docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)（`--wait`）
- [Docker Engine SDKs](https://docs.docker.com/reference/api/engine/sdk/)
- [Docker: Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)
- [Caddy: Automatic HTTPS](https://caddyserver.com/docs/automatic-https)
- [Caddy: API Tutorial](https://caddyserver.com/docs/api-tutorial)
- [Let's Encrypt: Rate Limits](https://letsencrypt.org/docs/rate-limits/)
- [Let's Encrypt: Challenge Types](https://letsencrypt.org/docs/challenge-types/)
- [GitHub Docs: Authenticating as a GitHub App installation](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)
- [GitHub Docs: Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)

* [Cloudflare R2: S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)
* [Cloudflare R2: Object lifecycles](https://developers.cloudflare.com/r2/buckets/object-lifecycles)
* [Cloudflare R2: Presigned URLs](https://developers.cloudflare.com/r2/api/s3/presigned-urls/)
* [restic documentation](https://restic.readthedocs.io/)
* [Squarespace: Adding DNS records to your domain](https://support.squarespace.com/hc/articles/360002101888)
* [さくらの VPS: サービス仕様](https://vps.sakura.ad.jp/vps_resource/pdf/spec.pdf)
* [さくらの VPS: 脆弱性への対応状況](https://vps.sakura.ad.jp/contents/info/202610-pendingapply/)
