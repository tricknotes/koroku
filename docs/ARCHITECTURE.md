# 個人用 PaaS（Heroku ライク）アーキテクチャ設計書

Nov 18, 2026 · @tricknotes

## 1. 目的とスコープ

個人用に 3〜5 個の Docker Compose アプリを、VPS 1 台の上で GitHub リポジトリから UI 操作だけでデプロイできるコントロールプレーンを作る。以下、本体デーモンを `koroku`、ベースドメインを `dev.tricknotes.io` と書く。

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
| 実行基盤 | さくらの VPS 1 台（Ubuntu 24.04、x86_64）。Docker Engine + Compose v2.30 以上 | ご回答どおり。override で使う `!override` タグ（2.24.4 以上）と、ポリシー検査で使う `config --no-env-resolution`（2.30 以上、6 章）の要件（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)） |
| コントロールプレーン | Go 製シングルバイナリ `koroku`。systemd でホストに直置き、SQLite | 常駐メモリが小さく、配布と更新がファイル 1 つ。詳細は 4 章 |
| フロントエンド | Ember SPA。ビルド成果物を `koroku` に埋め込んで配信 | ご希望どおり。配信用の別プロセスが不要 |
| リバースプロキシ | Caddy。ホストに直置きし、admin API で設定を投入 | ホスト名を渡すだけで証明書の取得と更新まで自動（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)） |
| アプリへの経路 | 公開サービスを `127.0.0.1:<割当ポート>` に publish し、Caddy がそこへ転送 | 共有 Docker ネットワークが不要で、アプリ同士が互いに見えない |
| TLS | ホスト名ごとに個別証明書（HTTP-01）。ワイルドカード「証明書」は見送り | ワイルドカード発行には DNS-01 が要り、DNS 事業者の API 連携が増える（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)） |
| GitHub 連携 | 自分専用の GitHub App。ログインと clone の両方に使う | clone 用トークンが 1 時間で失効し、長寿命の秘密を置かずに済む（5 章） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p koroku-<slug>`） | プロジェクト名でリソースが分離される（[How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)） |
| ビルド | `build:` があればホストで build、なければ pull | ご回答どおり（両対応） |
| ジョブ実行 | `koroku` プロセス内キュー、全体で同時実行 1 | ビルドの負荷を直列化。Redis 等の外部依存が不要 |
| UI 認証 | GitHub ログイン + 許可ユーザー ID 1 件の照合 | 個人用途。パスワード管理を持たない |

## 3. 全体アーキテクチャ

常駐プロセスは Caddy と `koroku` の 2 つだけで、どちらもホストの systemd サービスとして動かす。アプリは Compose プロジェクトとして並び、外部への公開は Caddy 経由の 80/443 に限る。

```text
                      Internet
                         | 80 / 443
+------------------------v--------------------------------- VPS --+
|  Caddy (systemd)                                                |
|    console.dev.tricknotes.io -> 127.0.0.1:8080  (koroku)        |
|    app-a.dev.tricknotes.io   -> 127.0.0.1:20001 (app-a / web)   |
|    app-b.dev.tricknotes.io   -> 127.0.0.1:20002 (app-b / web)   |
|    admin API: 127.0.0.1:2019                                    |
|                                                                 |
|  koroku (Go, systemd)  listen 127.0.0.1:8080                    |
|    - REST API + SSE / Ember SPA (embed)                         |
|    - Deploy worker (同時実行 1)                                  |
|    - SQLite  /var/lib/koroku/koroku.db                          |
|    - -> docker compose CLI (unix socket)                        |
|    - -> Caddy admin API  (POST /load)                           |
|    - -> GitHub API       (GitHub App)                           |
|                                                                 |
|  Compose project: koroku-app-a    Compose project: koroku-app-b |
|    web    127.0.0.1:20001:3000      web    127.0.0.1:20002:80   |
|    db     (publish なし)             worker (publish なし)        |
|    volume koroku-app-a_pgdata                                   |
+-----------------------------------------------------------------+
```

**ディレクトリ配置**

| パス | 内容 |
| --- | --- |
| `/usr/local/bin/koroku` | 本体バイナリ（SPA 込み） |
| `/etc/koroku/config.toml` | ベースドメイン、GitHub App ID、許可ユーザー ID |
| `/etc/koroku/github-app.pem`、`/etc/koroku/master.key` | GitHub App 秘密鍵、環境変数暗号化のマスターキー（0600） |
| `/var/lib/koroku/koroku.db` | SQLite |
| `/var/lib/koroku/apps/<slug>/releases/<deploy_id>/` | チェックアウトしたソースと生成した override |
| `/var/lib/koroku/apps/<slug>/env` | 展開済み環境変数ファイル（0600） |
| `/var/lib/koroku/apps/<slug>/release-state/<deploy_id>/` | そのリリースの環境変数スナップショット（0600）と、使った Compose ファイル名・ビルドしたイメージ名。ロールバック用（7 章） |
| `/var/lib/koroku/apps/<slug>/deploys/<deploy_id>.log` | デプロイログ |

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
- リポジトリ: `cmd/koroku`、`internal/{api,deploy,compose,caddy,github,store}`、`web/`（Ember）、`deploy/`（プロビジョニング）。

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
- GitHub App が未設定（`[github]` セクションなし）のときは、誰もログインできず API は 401 を返す。「未設定なら素通し」にはしない。ローカル開発では `koroku serve --insecure-no-auth` で認証と Origin 検証を外せる。設定ファイルではなく起動フラグにしたのは、本番の unit ファイルに紛れ込みにくくするため。
- `[github]` は全項目（`app_id`、`client_id`、`client_secret`、`private_key_file`、`allowed_user_id`）が揃っていなければ起動時にエラーにする。秘密鍵ファイルはグループや他人が読める権限なら拒否する。
- リポジトリの指定が `repo_full_name`（`owner/name`）のアプリは installation token を使って取得する。GitHub App がそのリポジトリにインストールされていなければ、public リポジトリの可能性があるので認証なしで取得を試みる。`repo_url` を明示したアプリには token を使わない。
- アプリ作成時の Compose ファイル検出（`GET /api/repos/{owner}/{repo}/compose`）は、サービス名とポートを画面に下書きするためのもので、YAML パーサーの依存を増やさず行ベースの簡易読み取りで済ませる（アンカーやフロースタイルは読めない。読めなければ手入力）。デプロイ時の判定は従来どおり `docker compose config` の結果で行う。
- 不採用案: fine-grained PAT。設定は簡単だが長寿命の秘密をサーバーに置くことになり、ログイン手段も別途必要になる。

## 6. Compose の扱い

リポジトリの `compose.yaml` は書き換えず、`koroku` が生成する override ファイルを `-f` で重ねて実行する。

**アプリごとの設定（UI で入力）**

- Compose ファイルのパス（既定 `compose.yaml`、なければ `docker-compose.yml`）
- 公開するサービス名（既定 `web`）とコンテナ内ポート
- ヘルスチェックパス（既定 `/`）

**生成する override（例）**

```yaml
# /var/lib/koroku/apps/app-a/releases/42/koroku.override.yaml
services:
  web:
    ports: !override
      - "127.0.0.1:20001:3000"
    env_file:
      - /var/lib/koroku/apps/app-a/env
    restart: unless-stopped
  db:
    ports: !reset []
    env_file:
      - /var/lib/koroku/apps/app-a/env
    restart: unless-stopped
```

- 公開サービスの `ports` は `!override` で割当ポートに置き換え、他サービスの `ports` は `!reset []` で消す。通常のマージでは `ports` が追記されてしまうため、この 2 つのタグが必要（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)）。
- ホストポートは 20000 番台からアプリ作成時に 1 つ割り当て、DB で一意に保つ。
- 環境変数は全サービスに `env_file` で注入する（Heroku の config vars と同じ感覚）。`${VAR}` 補間用に `--env-file` でも同じファイルを渡す。
- named volume はプロジェクト名 `koroku-<slug>` が接頭辞になるため、リリースディレクトリが変わっても引き継がれる。

**override の細則（M1 の実装と実機確認で確定、2026-11-18）**

- `restart: unless-stopped` は、リポジトリ側が `restart` を指定していないサービスにだけ付ける。全サービスに付けると、マイグレーション用のワンショットサービス（`restart: "no"`）が終了後に再起動し続けてしまうため。
- `build:` を持つサービスには、リリースごとのイメージ名 `koroku-<slug>-<service>:r<deploy_id>` を override の `image:` で与える。Compose の既定では毎回 `<project>-<service>:latest` に上書きされ、起動に失敗した新版のイメージで旧版のタグが置き換わるので、旧リリースのディレクトリで `up` し直しても壊れたイメージが起動した（結合テストで検出）。リリース固有のタグにしておくと、ロールバックは旧版のイメージそのものを起動できる。イメージはリリースディレクトリと一緒に削除する。
- メモリ上限の決まり方は、優先度の高い順に「アプリ設定のサービス別の値」「アプリ設定の全体値」「リポジトリの Compose が自分で指定した値（`config.toml` の既定値以下のときだけ）」「`config.toml` の既定値（512m）」。既定値は、リポジトリが自分で名乗れる上限でもある。それより大きくしたいときはアプリ設定で指定する（Compose に `mem_limit: 64g` と書くだけで上限を外せてしまうのを防ぐため。脆弱性調査での対応）。リポジトリ側が `deploy.resources.limits.memory` で書いている場合は override も同じ形式で書く。`mem_limit` と値が食い違うと Compose が `can't set distinct values` で設定全体を拒否するため。
- override に書くサービスは、リポジトリの Compose を正規化した結果（`config --format json`）に存在するものだけにする。存在しないサービス名を書くと Compose がエラーにするため、正規化 → override 生成 → もう一度正規化してポリシー検査、の 2 回 `config` を実行する。
- 正規化した JSON には `env_file` の中身が展開されて入るので、ログには出さない。
- env ファイルの値は Compose に補間されない形で書く。英数字と一部の記号だけの値はそのまま、それ以外はシングルクォート、シングルクォートか改行を含む値はダブルクォートで `\ \" \$ \n` をエスケープする。`$`・空白・`#`・引用符を含む値がコンテナにそのまま届くことを結合テストで確認している。

**ビルドと pull の判定**

1. `docker compose -p koroku-<slug> -f compose.yaml -f koroku.override.yaml config --format json` で正規化する。
2. `build` を持つサービスがあれば `docker compose build`、`image` のみのサービスは `docker compose pull`。
3. private レジストリは、初期はホストで `docker login` 済みであることを前提にする。

**ポリシー検査（正規化した JSON に対して実施、違反はデプロイ拒否）**

- `privileged: true`、`network_mode: host`、`pid: host`
- `/var/run/docker.sock` のマウント
- リリースディレクトリ外への bind mount
- 公開サービス名が Compose に存在しない

個人用途なので、アプリ設定の「検査を緩める」フラグで個別に許可できるようにする。

**検査項目の追加（脆弱性調査での対応、2026-11-18）**

当初の一覧は代表的なものだけで、Compose には同じ結果になる書き方がほかにもある。実物の `docker compose config` で、次がどれも素通りすることを確かめたので、検査に加えた（いずれも「検査を緩める」フラグで許可できる）。

| ルール | 拒否する内容 | 理由 |
| --- | --- | --- |
| `volume_host_path` | `driver_opts` の `device` にホストのパスを指定した named volume | bind mount の検査を volume の形で回避できる |
| `foreign_volume`、`foreign_network` | `external: true`、または名前が `koroku-<slug>_` で始まらない volume / ネットワーク | 他アプリの volume やネットワークを名前で指して使える（アプリ同士を分離するという前提が崩れる） |
| `foreign_namespace` | `network_mode` / `pid` / `ipc` の `container:<名前>`、`volumes_from` の `container:<名前>` | 他アプリのコンテナの名前空間や volume に入れる。同じアプリ内の `service:<名前>` は許可 |
| `host_namespace` | `ipc` / `uts` / `userns_mode` / `cgroup` の `host` | `pid: host` や `network_mode: host` と同類 |
| `capability` | Docker の既定の権限と `IPC_LOCK`・`SYS_NICE` 以外の `cap_add` | `SYS_ADMIN` などは `privileged` に近い |
| `device` | `devices`、`device_cgroup_rules` | ホストのデバイスに触れる |
| `security_opt` | `seccomp` / `apparmor` の `unconfined`、ラベルの無効化など | コンテナの隔離を弱める |
| `file_outside_release` | リリースディレクトリ外の `env_file`、`secrets` / `configs` の `file`、ビルドコンテキスト、Dockerfile | ホストのファイル（他アプリの env ファイルなど）をコンテナやイメージに取り込める。基盤が注入する env ファイルだけは例外 |
| `bind_writable` | 読み取り専用（`:ro`）でない bind mount | 下記 |
| `host_network`、`privileged`（ビルド） | `build.network: host`、`build.privileged: true` | ビルド中のコマンドが、ホストのループバックだけで待ち受けているもの（Caddy の admin API など）に届く |
| `runtime` | `runc` 以外の `runtime` | 隔離の弱いランタイムを選べる |
| `memory_guard` | `oom_kill_disable: true`、`memswap_limit: -1` | メモリ上限を実質的に無効にできる |

**書き込み可能な bind mount を禁止する理由**: チェックアウト（リリースディレクトリ）に書き込めるコンテナは、別の bind mount の元になっているパスをホストの任意の場所へのシンボリックリンクに置き換えられる。Docker はコンテナの次回起動時（`restart: unless-stopped` による自動再起動を含む）にリンクをたどるので、アプリのコードを実行できるだけの攻撃者がホストのファイルシステムに届いてしまう。また `koroku` が起動・ロールバックで再利用する Compose ファイルや override も書き換えられる。設定ファイルを渡すための bind は `:ro` を付け、データは named volume に置く（バックアップの対象も named volume だけ）。

- `env_file` のパスを検査するため、`docker compose config` には `--no-env-resolution` を付ける（付けないと env ファイルの中身が `environment` に展開され、パスが消える）。この指定で、正規化した JSON にアプリの環境変数の値が入らなくなる利点もある。Compose 2.30 以上が要る。
- リリースのファイルを再利用する `up`（アプリの起動、ロールバック）の直前にも、`config` とポリシー検査をやり直す。デプロイ時の検査のあとでファイルが書き換えられていたら、起動せずに拒否する（起動は 422 `policy_violation`、ロールバックは「戻せない」とログに書く）。
- bind mount の元のパスがまだ存在しない場合（Docker が作る）も、存在する最も深い親までシンボリックリンクを解決してから内外を判定する。
- override ファイルは一時ファイルに書いてから rename で置く。チェックアウトの中身はリポジトリ由来で、同名のシンボリックリンクが置かれていると、リンク先（ホスト上の任意のファイル）を root 権限で上書きしてしまうため。Compose ファイルのパスも、途中のディレクトリがシンボリックリンクでリリース外へ出ていないことを確かめる。

ただし「公開サービス以外のポート公開」と「公開サービスの `127.0.0.1:<割当ポート>` 以外への公開」は、フラグを立てても許可しない。Docker が publish したポートは ufw を通らずに外へ届くので（10 章）、override が効いていることの最終確認として常に検査する。bind mount のパスはシンボリックリンクを解決してから判定する（リポジトリ内のリンクでホストのディレクトリを持ち込めないようにするため）。

## 7. デプロイフロー

デプロイは `queued → running → succeeded | failed | canceled` の状態を持つ 1 ジョブで、次の順に進む。

1. **受付**: UI でブランチかコミットを選ぶ。API がコミット SHA を確定し、`deployments` に `queued` で記録する。
2. **取得**: installation token を発行し、`releases/<deploy_id>/` に `git fetch --depth 1 origin <sha>` でチェックアウトする。
3. **生成**: env ファイル（0600）と `koroku.override.yaml` を書き出す。
4. **検証**: `docker compose config --format json` を実行し、6 章のポリシー検査をかける。
5. **ビルド／pull**: 6 章の判定に従う。
6. **起動**: `docker compose up -d --remove-orphans --wait --wait-timeout 300`。`--wait` はサービスが running または healthy になるまで待つ（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。
7. **経路**: Caddy 設定を再生成して投入する（初回と設定変更時）。
8. **確認**: `http://127.0.0.1:<port><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒リトライする。
9. **完了**: `succeeded` にして `current_deployment_id` を更新。リリースディレクトリは直近 3 件を残し、`docker image prune -f` を実行する。

**失敗時**

- 手順 5 までの失敗は稼働中のコンテナに触れないので、`failed` を記録するだけ。
- 手順 6 以降の失敗は、直前の成功リリースのディレクトリで `up` し直して旧版に戻す。
- `koroku` 再起動時に `running` のまま残ったジョブは `failed (interrupted)` にする。
- 直前の成功リリースがない（初回デプロイの）場合は戻す先がないので、`docker compose down` で起動しかけたコンテナを消す（volume は残す）。
- 手順 6 の `up` か手順 8 の確認で失敗したら、戻す前に `docker compose logs --tail 30` の出力をデプロイログのその手順に書く（見出しは `Last output of the containers:`）。起動に失敗した理由はたいていコンテナの出力にあるが、ロールバックでコンテナが置き換わるとアプリのログ画面からしか追えなくなり、デプロイ画面で原因が分からなかった（2026-11-18、開発用の Compose 環境で実際に失敗させて確認）。この出力もデプロイログのマスクを通る。読み出しに失敗してもデプロイの結果は変えない。キャンセルや `koroku` の停止による失敗では読まない。
- ロールバックでは環境変数も戻す。env ファイルはリリースごとにスナップショットを取り（`release-state/<deploy_id>/env`）、稼働中の `apps/<slug>/env` を書き換えるのは手順 6 の直前にする。こうすると、手順 5 までの失敗では稼働中のリリースの env に触れず、手順 6 以降の失敗では旧版を「当時の環境変数」で起動し直せる。環境変数の変更が原因で起動に失敗した場合にも旧版に戻れるようにするため。
- `koroku` の停止（SIGTERM）と重なったデプロイは `failed (interrupted)` にし、停止処理の中ではロールバックを走らせない。コンテナが中途半端な状態で残りうるので、`koroku` の更新はデプロイ中を避ける。
- `docker compose up --wait` は、ヘルスチェックを持たないコンテナが起動直後に落ちて再起動を繰り返していても成功を返した（実機確認、Compose v5.8.0）。起動失敗の検出は手順 8 の HTTP 確認が担う。

**その他の規則**

- 同時実行は全体で 1。同じアプリに後続のデプロイが積まれたら、古い `queued` は `canceled` にする。
- 各手順の stdout/stderr を行単位でログファイルに追記し、SSE で配信する。環境変数の値とトークンはマスクする。
- ロールバックは「過去の SHA を同じフローで再デプロイ」。DB マイグレーションの巻き戻しは行わない。
- マイグレーション等のリリース処理は Compose 側で表現する（起動コマンド、またはワンショットのサービスと `depends_on`）。基盤側に専用機能は持たない。

**デプロイ時の断の範囲**: 断はデプロイ対象のアプリだけに起きる。`docker compose up` は `-p koroku-<slug>` で指定したプロジェクトのコンテナしか操作せず、その中でも設定かイメージが変わったサービスだけを再作成する（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。他アプリへの間接的な影響は 2 つある。

- ビルド中の CPU・メモリ負荷で他アプリの応答が遅くなりうる（停止はしない）。8.2 のメモリ上限とビルド直列化で抑える。
- Caddy 設定の再投入はアプリ作成時とドメイン・ポート変更時だけ行い、通常の再デプロイでは行わない。再投入時に他アプリの接続（特に WebSocket や SSE）が切れないことは M2 で実機確認する。確認結果は 12 章。SSE と通常のリクエストは切れず、WebSocket は 8 章の `stream_close_delay` で猶予を持たせている。

## 8. ルーティングと TLS

DNS にワイルドカードの A レコードを 1 本置き、証明書は Caddy がホスト名ごとに自動取得する。

- **DNS**: `*.dev.tricknotes.io` を VPS の IP に向ける。アプリは `<slug>.dev.tricknotes.io`、管理 UI は `console.dev.tricknotes.io`。
- **証明書**: Caddy は設定にホスト名があると自動 HTTPS を有効にし、証明書の取得・更新と HTTP→HTTPS リダイレクトを行う（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)）。
- **発行上限**: Let's Encrypt は登録ドメインあたり 7 日間で 50 枚まで（[Rate Limits](https://letsencrypt.org/docs/rate-limits/)）。3〜5 アプリでは問題ないが、開発中は staging 環境を使う。
- **設定投入**: `koroku` が DB から Caddy の JSON 設定全体を生成し、admin API の `POST /load` で置き換える（[API Tutorial](https://caddyserver.com/docs/api-tutorial)）。差分更新はしない。`koroku` 起動時にも必ず実行し、DB を唯一の正とする。
- **停止中アプリ**: ルートは残し、503 の固定ページを返す。

**Caddy 設定の細則（M2 の実装と実機確認で確定、2026-11-18）**

- サーバーは `:80` 用と `:443` 用の 2 つに分ける。1 つのサーバーに両方のポートを持たせると、`http://<slug>.<base>` へのアクセスが HTTPS へリダイレクトされず平文のままアプリへ転送された（Caddy が自動リダイレクトを挿入する位置が、ホスト別ルートの後ろになるため）。分けた構成では HTTP は 308 で HTTPS へ、未知のホスト名は `:80` で 404、`:443` では TLS ハンドシェイク失敗になる。
- 一度もデプロイしていないアプリも、停止中アプリと同じ 503 の固定ページにする（転送先がまだないため）。
- 設定の投入は、DB から生成した内容が前回投入したものと同じなら省く。これで通常の再デプロイでは Caddy に触れない。`koroku` 起動時だけは必ず投入する（Caddy 側は同一内容なら `config is unchanged` として何もしない）。投入に失敗したら次回必ず投入し直し、5 分ごとの定期処理でも再試行するので、Caddy が後から起動しても追いつく。
- アプリへの `reverse_proxy` には `stream_close_delay: 5m` を付ける。付けないと、設定を再投入した瞬間に、他アプリを含む WebSocket などのアップグレード済み接続が切れた（下の実機確認）。この指定には Caddy 2.7 以上が要るので、Ubuntu 標準の apt パッケージではなく Caddy 公式リポジトリのものを入れる（プロビジョニングスクリプトで対応）。
- アクセスログはアプリごとに `<access_log_dir>/<slug>.log`（既定 `/var/log/caddy/apps`）へ出す。`koroku` のデータディレクトリは root 専用（0700）で Caddy のユーザーが書けないため、置き場所を分けた。ロガー名は `http.log.access.<slug>.app` とする。Caddy はロガー名を前方一致で選ぶので、slug そのままだと `shop-staging` のログが `shop.log` にも入るため。管理 UI と未知のホスト名のアクセスログは出さない。
- Caddy の admin API は認証がなく設定全体を書き換えられるので、`caddy.admin_url` はループバックアドレスしか受け付けない。
- `koroku` がデプロイの成功を記録したあとにも、もう一度設定を同期する。ルートの更新（手順 7）はデプロイが「現在のリリース」になる前に走るので、そこだけでは初回デプロイ（と停止中からのデプロイ）のアプリが 503 の固定ページのまま残り、5 分ごとの定期処理まで届かなかった（ローカル環境で検出、2026-11-19）。

**ローカル環境の Caddy（2026-11-19）**

手元でも本番と同じ経路（Caddy → アプリ）で `https://<slug>.dev.localhost` を開けるように、開発用の `compose.yaml` に Caddy を加えた。`koroku` 側の生成ロジックは本番と共通で、`config.toml` の 2 項目だけで切り替える。

| 項目 | 内容 |
| --- | --- |
| `[caddy] listen_ip` | `:80` と `:443` を、このアドレスだけで待ち受ける（ローカルは `127.0.0.1`）。空なら全インターフェース（本番） |
| `[caddy] internal_ca` | ACME の代わりに Caddy 内蔵の CA で証明書を出す。`*.localhost` には公的な証明書が出ないため。CA のルート証明書は利用者がブラウザ側の OS に手で入れる（コンテナの中の Caddy にはホストの信頼ストアを変えられないので、`install_trust` は切る） |

- `--insecure-no-auth` は、Caddy が有効でも `listen_ip` がループバックなら起動を許す。拒否していた理由は「Caddy 経由で管理 UI がネットワークに出る」ことで、ループバックだけで待ち受けるならその心配がない。
- 認証なしモードの Host 検査は `*.localhost` を受け付ける（`console.dev.localhost` で開くため）。`.localhost` 配下の名前は常にループバックに解決される（RFC 6761）ので、DNS リバインディングの対策は崩れない。変更系の `Origin` は `http://<Host>` に加えて `https://<Host>` も許す。同じ `dev.localhost` 配下のアプリからの要求は、従来どおり `Sec-Fetch-Site: same-site` と `Origin` の不一致で拒否する。
- アクセスログは出さない（`access_log_dir = ""`）。Caddy はコンテナ内で root として動き、ログファイルを `koroku`（利用者の UID）が読めないため。

**Squarespace Domains での設定**: `tricknotes.io` の DNS Settings → Custom Records に A レコードを追加する（[Adding DNS records to your domain](https://support.squarespace.com/hc/articles/360002101888)）。Host に `*.dev` を指定できるかは未確認。できない場合は `console.dev` とアプリごとの `<slug>.dev` を個別に登録する。証明書はホスト名ごとに取る設計なので、どちらでも `koroku` 側の実装は変わらない。

ワイルドカード証明書は DNS-01 チャレンジでのみ発行できる（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)）。個別証明書だとサブドメイン名が証明書の公開ログに載る点が気になる場合は、後から DNS 事業者の API 連携を足して切り替える。

### 8.1 独自ドメイン

アプリごとに任意個の独自ドメインを UI から追加できる。自動割当の `<slug>.dev.tricknotes.io` は常に残り、独自ドメインはそこに足される別名として扱う。

1. **登録**: UI でホスト名（例 `example.com`、`www.example.com`）を入力する。状態は `pending`。
2. **DNS 設定（利用者が実施）**: サブドメインは `<slug>.dev.tricknotes.io` への CNAME、apex は VPS の IP への A レコード。UI に設定すべき値を表示する。
3. **検証**: `koroku` がホスト名を名前解決し、`config.toml` の `public_ip` と一致したら `active` にする。「再検証」ボタンと 5 分間隔の自動再試行を持つ。
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
- より強い分離が要るようになったら、同じプロビジョニングスクリプトで 2 台目を立てて `koroku` をもう 1 組動かす。マルチノード対応はしない。

**本番利用で残る制約**: デプロイ時にコンテナ再作成による数秒の断がある。VPS 1 台なのでホスト障害時は全アプリが止まる。デプロイ時の断はデプロイ対象のアプリに限られる（7 章）。ホスト障害時の全停止は許容すると決定済み（2026-11-18）。

## 9. データモデル・API・UI

テーブルは 4 つで足りる。

| テーブル | 主なカラム |
| --- | --- |
| `apps` | `id`, `slug`（一意）, `repo_id`, `repo_full_name`, `default_branch`, `compose_path`, `web_service`, `web_port`, `host_port`（一意）, `healthcheck_path`, `desired_state`（running / stopped）, `relax_policy`, `current_deployment_id` |
| `env_vars` | `app_id`, `key`, `value_encrypted`（`app_id` + `key` で一意） |
| `deployments` | `id`, `app_id`, `ref`, `commit_sha`, `commit_message`, `status`, `error`, `queued_at`, `started_at`, `finished_at` |
| `sessions` | `token_hash`, `id`, `github_user_id`, `expires_at`, `last_seen_at`, `ip`, `user_agent`（10.3） |

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
| デプロイタブの手順表示 | 手順の区切りと、手順ごとの所要時間 | デプロイログの手順の見出し行を `==> <RFC 3339 の時刻> <手順のキー> <表題>` の形にする。UI はこの行でログを手順に分け、次の見出し（または終了）までを所要時間とする。手順のキーは `fetch` `prepare` `validate` `build` `pull` `backup` `start` `route` `check` `done` `cleanup` と、失敗時の `failed` `rollback`。DB に手順の表は持たない（ログファイルが唯一の記録で、SSE の再接続とも整合する）。自動ロールバックの結果も同じ理由でログの行から読み取り、デプロイの JSON にフィールドは足さない（UI が解釈する行は `docs/API.md`） |
| バックアップタブ | 次回の実行予定、スナップショットのサイズ | `GET /api/backups` に `next_run_at`、スナップショットに `size_bytes`（restic が記録していれば。なければ `null`）を足す |
| システム画面 | Docker のディスク内訳 | `GET /api/system/docker`（`docker system df` の結果: イメージ、コンテナ、volume、ビルドキャッシュそれぞれの合計と回収可能な量）。数秒かかりうるので `/api/system` とは分ける |

システム画面を作る段階で、さらに 2 つ足した（2026-11-18）。どちらも既存の値からは求められない。

- `GET /api/system` に `swap`（`total_bytes`、`used_bytes`。swap がない、または読めなければ `null`）。ビルド時のピークに備えて swap を 2GB 置いているので（10 章）、メモリと並べて使われ具合を見られるようにする。`/proc/meminfo` の `SwapTotal` と `SwapFree` から求める。
- `log_archive` に `next_run_at`（次にログ退避が走る時刻。未設定なら `null`）。退避の間隔は設定値（既定 4 時間）で、UI からは分からないため。バックアップの `next_run_at` と対にして、システム画面の「次回」に出す。

バックアップタブで「どの volume を取るか」を一覧から選べるようにするため、もう 1 つ足した（2026-11-18）。

- `GET /api/apps/{slug}/volumes`: そのアプリの named volume の一覧（名前、バックアップから外しているか、Docker に実在するか）。それまでの UI は、過去のスナップショットの対象と除外リストから一覧を組み立てていたので、一度もスナップショットを取っていない volume は名前を手で入力しないと外せなかった。DB のデータディレクトリのように「最初のバックアップより前に外したい」volume こそ、この穴に落ちる。
  - 「アプリの volume」の定義はバックアップと同じにする（10.2 の細則）。バックアップが使う列挙の処理（`internal/backup`）をそのまま呼び、別の定義を作らない。
  - 列挙は `docker volume ls` と `docker volume inspect` だけで、restic もオブジェクトストレージも使わない。バックアップが未設定のホストでも応答する。
  - 一度もデプロイしていないアプリは volume がまだないので、空の一覧を返す（エラーにしない）。
  - 除外リストにあるが Docker に実在しない名前（先に外しておいた、あるいは volume を消した）も `exists: false` として一覧に含める。UI に出ないと対象へ戻せないため。
  - volume の大きさは返さない。求めるにはディレクトリを歩く（`docker system df -v` も同じ）必要があり、タブを開くたびに走らせるには重い。スナップショットの `size_bytes` が目安になる。

バックアップタブ自体の API（一覧、実行、リストア、ダンプのダウンロード）は 10.2 の内容を M5 で `docs/API.md` に定義済み。「保存して再デプロイ」は環境変数の `PUT` とデプロイの `POST` を UI が続けて呼ぶだけで、専用の API は作らない。状態の自動更新はポーリング（一覧と詳細は数秒間隔、デプロイ中のログは SSE）で行い、状態変化を配信する専用の SSE は作らない（アプリ 3〜5 個の規模では不要）。

## 10. セキュリティと運用

`koroku` は Docker ソケットを操作できるためホストの root 相当であり、管理 UI の認証がこのシステムの唯一の関門になる。

**セキュリティ**

- **信頼モデル**: デプロイするのは自分のリポジトリだけ。他人のコードを動かす前提の隔離（gVisor 等）は持たない。
- **認証**: GitHub ログイン後、ユーザー ID が `config.toml` の許可 ID と一致した場合のみセッションを発行する。Cookie は `HttpOnly`、`Secure`、`SameSite=Lax`。変更系 API は `Origin` ヘッダーを検証する。GitHub の 2 段階認証の必須化、危険な操作の前の再ログイン、セッションの一覧とログイン履歴は 10.3。
- **待受**: `koroku` と Caddy admin API は 127.0.0.1 のみ。外部公開は 22、80、443。
- **残っている前提（脆弱性調査、2026-11-18）**: ポリシー検査は「自分のリポジトリを誤って危険な構成でデプロイしない」ための検査で、悪意あるリポジトリを安全に動かす隔離ではない。「検査を緩める」フラグを立てたアプリには、上の保護の多くが効かない（フラグは 1 つで、項目ごとには選べない）。Caddy の admin API は認証なしの TCP（ループバック）なので、ホストのネットワークに入れたコンテナからは設定を書き換えられる。ログのマスクは環境変数の値そのものとの一致だけで、3 文字未満の値や、base64 などに変換された値は隠せない。
- **ポート公開の抑止**: Docker が publish したポートの通信は ufw の設定を通らずに届く（[Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)）。ufw に頼らず、6 章の override で全サービスの `ports` を消すか 127.0.0.1 に限定する。
- **シークレット**: 環境変数の値は AES-256-GCM で暗号化して SQLite に保存する。マスターキーは `/etc/koroku/master.key`。UI は既定でマスク表示。

**管理 UI の保護の細則（脆弱性調査での対応、2026-11-18）**

- **アプリは管理 UI の兄弟サブドメインで動く**（`<slug>.<base>` と `console.<base>`）。ブラウザーから見ると same-site なので、`SameSite=Lax` はアプリからのリクエストを止めない。そのため次を重ねている。
  - Cookie 名に `__Host-` プレフィックスを付ける（HTTPS のとき）。兄弟サブドメインのページは親ドメイン向けの Cookie を置けるが、`__Host-` 付きの Cookie はそのホスト自身からしか設定できないので、アプリがセッションやログイン用 state の Cookie を差し込んだり上書きしたりできない。
  - 変更系リクエストは `Origin` の完全一致を要求する（5 章）。加えて、ブラウザーが `Sec-Fetch-Site` で同一オリジン以外と申告した API リクエストは、参照系も含めて拒否する（応答は読めなくても、`docker system df` や restic の一覧を走らせる負荷はかけられるため）。
  - JSON 以外の Content-Type のボディは拒否する（`text/plain` などの POST は CORS のプリフライトなしで送れるため）。
- **レスポンスヘッダー**: `Content-Security-Policy`（スクリプトは自サイトのファイルのみ。ビルド成果物にインラインスクリプトはない）、`Strict-Transport-Security`（HTTPS のとき）、`X-Frame-Options: DENY`、`Cross-Origin-Opener-Policy` / `Cross-Origin-Resource-Policy: same-origin`、`X-Content-Type-Options: nosniff`。
- **待受アドレス**: `listen` はループバックアドレスしか受け付けない（起動時に検査）。
- **`--insecure-no-auth`**: `[caddy] enabled = true` のときは起動を拒否する。このモードでも、Host がループバック名でないリクエスト（DNS リバインディング対策）と、Origin が自分自身でない変更系リクエストは拒否する。ログインがない分、起動した本人のブラウザーが攻撃面になるため。
- **設定ファイル**: 秘密（GitHub の client secret、ストレージの secret key）を含む `config.toml` が所有者以外から読める権限だと起動を拒否する。
- **OAuth のコールバック**: 1 分あたり 10 回までしか GitHub への問い合わせを行わない（誰でも叩けるエンドポイントで、叩くたびに client secret 付きのリクエストが飛ぶため）。
- **ログの大きさ**: デプロイログは 1 行 64 KiB、1 デプロイ 50 MiB で打ち切る（改行のない出力や終わらない出力で、メモリや全アプリ共有のディスクを使い切らないため）。
- **ファイルの権限**: `koroku.db` は 0600 にする。オブジェクトストレージの endpoint は、ループバック以外では https だけを受け付ける。
- **デプロイログの配信**: 行の途中の CR は取り除く。SSE では CR も行の区切りなので、ログの内容で `event: end` などのフィールドを偽装できてしまうため。
- **コンソールのホスト名と同じ slug・独自ドメイン**は登録できない。

**運用**

- **コンテナログ**: Docker の `json-file` ドライバを `max-size=10m`、`max-file=3` でローテートする（`daemon.json`）。長期保管は 10.1 の収集機構が担う。
- **ディスク**: デプロイ成功後に `docker image prune -f`、週 1 回 `docker builder prune --filter until=168h`。使用率はシステム画面に出す。
- **メモリ**: ビルド時のピークに備えて swap を 2GB 置く。VPS は 2 vCPU、4GB RAM、40GB ディスクを出発点にする（未計測の目安）。
- **バックアップ**: 基盤の設定とアプリのデータを日次で R2 に退避する（10.2）。
- **koroku の更新**: バイナリを差し替えて `systemctl restart koroku`。アプリのコンテナは影響を受けない。
- **起動制御と削除（M5 の実装で確定、2026-11-18）**
  - 停止は `docker compose stop`、起動は稼働中リリースのファイルで `up -d --wait`（設定を確実に揃えるため。リリースのファイルが残っていなければ `docker compose start`）、再起動は `docker compose restart`。停止・起動のたびに Caddy 設定を再投入し、停止中は 503 の固定ページになる。
  - デプロイと手動操作はアプリ単位で直列化する。デプロイが `queued` か `running` のアプリへの操作は 409（`busy`）で断り、操作の実行中に始まったデプロイは操作の完了を待つ。同じアプリに対して `docker compose` を同時に走らせないため。
  - 削除は `docker compose down`（`?volumes=true` なら `--volumes` に加え、プロジェクトのラベルが付いた volume を明示的に消す）→ リリース用イメージの削除 → DB の行とアプリのディレクトリの削除 → Caddy 設定の再投入、の順。`down` に失敗したら DB には触れない。volume を残して削除したあとに同じ slug でアプリを作り直すと、プロジェクト名が同じなので残した volume がそのまま使われる（結合テストで確認）。
  - `docker builder prune --filter until=168h` は毎日実行する。対象は「1 週間使われていないビルドキャッシュ」なので効果は週 1 回と同じで、`koroku` を再起動しても実行の機会を逃さない。
  - Caddy 設定は 5 分ごとに、内容が変わっていなくても投入し直す。Caddy が単独で再起動すると投入済みの設定を忘れるため。同一内容の投入は Caddy 側で何もしないので負荷にならない（12 章の確認結果）。
- **プロビジョニング**: 新規 VPS を 1 本のスクリプトで構築する（Docker、Caddy、`koroku` の unit、swap、ログ設定）。

### 10.1 ログ収集

ログは `koroku` 内蔵のコレクタがファイルに蓄積し、4 時間ごとに R2 に退避する。Loki などのログ基盤は、常駐メモリが増えるので 3〜5 アプリの規模では採用しない。

| 項目 | 仕様 |
| --- | --- |
| 対象 | 全サービスの stdout / stderr、Caddy のアクセスログ（アプリ別）、デプロイログ |
| 収集 | アプリごとに `docker compose logs --follow --timestamps --no-color` を読み続け、`/var/lib/koroku/logs/<slug>/<service>/YYYY-MM-DD.log` に追記 |
| 再開 | `koroku` 再起動時は最後に書いた時刻を `--since` に渡す。直近分は Docker 側に残っているので短い停止は埋まる |
| ローカル保持 | 14 日。日付が変わったファイルは gzip 圧縮 |
| R2 への退避 | 4 時間ごと（00:10 JST 起点）に、前回以降の差分を gzip して `logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz` へアップロード |
| R2 側の保持 | プレフィックス `logs/` に 90 日で削除するライフサイクルルールを設定（[Object lifecycles](https://developers.cloudflare.com/r2/buckets/object-lifecycles)） |
| 閲覧 | ライブ tail（SSE）。過去 14 日は日付・サービス・キーワードで絞り込み。それ以前は presigned URL でダウンロード（[Presigned URLs](https://developers.cloudflare.com/r2/api/s3/presigned-urls/)） |

- API: `GET /api/apps/{slug}/logs?service=&from=&to=&q=`、`GET /api/apps/{slug}/logs/archives`、`GET /api/apps/{slug}/logs/archives/url?key=`。

**ログ収集の細則（M5 の実装と実機確認で確定、2026-11-18）**

- ローカルのファイルは 1 行が `<UTC の時刻（ナノ秒まで）> <本文>`。日付の区切りと退避の時間枠は `config.toml` の `timezone`（既定 `Asia/Tokyo`）で決める。
- サービス名は `docker compose logs` の行頭（コンテナ名からプロジェクト名を除いたもの）から末尾の `-<番号>` を落として求める。`container_name` を独自に指定したサービスはその名前で記録される（Compose がそう出力するため）。
- 再開時は、サービスごとに最後に書いた時刻の 30 秒前から `--since` で読み直し、「サービス・時刻・本文」が一致する行を数えて重複を落とす。同じ時刻の別の行は残す。Docker 側に行が残っている限り欠落しない。
- 1 回の `logs --follow` の中で、同じコンテナの「時刻・本文」が一致する行がもう一度出たら捨てる（コンテナごとに直近 4096 行を覚えておく）。`docker compose logs --follow` は、コンテナの起動と重なって開始されると同じコンテナに二重に接続し、以後その行をすべて 2 回ずつ出すことがある（12 章の実機確認）。停止中のアプリを待っているフォロワーはデプロイのたびにこの状況になるので、対処しないと保存されるログと R2 への退避が倍になる。Docker は行ごとにナノ秒の時刻を付けるので、同じコンテナで時刻も本文も同じ行はアプリが 2 回出力したものではない。別レプリカ（`web-1` と `web-2`）の同じ行は残す。
- 1 行は 16 KiB で切り、不正な UTF-8 と制御文字は置き換える。ログの本文は `koroku` 自身のログやエラーメッセージに出さない（アプリの出力に何が入るか分からないため）。
- Caddy のアクセスログは、同じアプリの擬似サービス `_access` として取り込む。`koroku` が止まっている間にローテートされて消えた分は読めない。
- 退避は「日ごとのファイルのどこまで送ったか」をバイト位置で記録し、送る範囲を保存してからアップロードする。失敗や再起動のあとは同じ範囲を同じキーへ送り直すので、重複も欠落も起きない。失敗した時間枠は次回（および 10 分後の再試行）にその時間枠のキーで送る。空の時間枠は何も送らない。
- デプロイログは、30 分間更新がなければ完了とみなして `logs/<slug>/_deploy/<id>.log.gz` に 1 回だけ退避する。
- オブジェクトストレージが未設定でもログの収集・検索・ライブ表示は動く。退避と、退避済みログの一覧・ダウンロードだけが無効になる。
- 過去ログの検索は `/logs/history` に分けた（`docs/API.md`）。SSE と JSON を同じパスで出し分けると扱いにくいため。
- 制約: 直近の区間は VPS 上にしかない。ホストを失うと最大 4 時間分のログが消える。間隔は設定値（既定 4 時間、最短 1 時間）。1 回の処理はサービスごとに差分テキストの圧縮と 1 オブジェクトのアップロードだけで、常駐負荷は増えない。

### 10.2 バックアップ

バックアップは restic で R2 に取る。R2 は S3 互換 API を持ち（[S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)）、restic は S3 互換ストレージをリポジトリにできる（[restic documentation](https://restic.readthedocs.io/)）。重複排除・暗号化・世代管理を自前で書かずに済む。

| 対象 | 方法 |
| --- | --- |
| 基盤（`koroku.db`、`/etc/koroku`） | SQLite は `VACUUM INTO` で整合したコピーを作ってから `restic backup` |
| アプリの named volume | `docker volume inspect` で得たマウントポイントを稼働中のまま `restic backup`。アップロードファイルなど向け |
| アプリの DB | アプリ設定の「ダンプフック」（サービス名 + コマンド。例: `db` / `pg_dump -U postgres app`）を `docker compose exec -T` で実行し、標準出力を `restic backup --stdin` に流す（[restic manual](https://restic.readthedocs.io/en/stable/manual_rest.html)） |

- 稼働中の DB のデータディレクトリをファイルコピーしても整合性は保証されない。DB はダンプフックで取り、その volume はアプリ設定で対象から外せるようにする。
- **スケジュール**: 毎日 03:00 JST。UI から手動実行もできる。「デプロイ前に取得」はアプリ単位のオプション（既定オフ）。
- **世代**: `restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune`。スナップショットには `app:<slug>` と `kind:system|volume|dump` のタグを付ける。
- **監視**: 週 1 回 `restic check`。最終成功時刻を UI に出し、24 時間以上成功がなければ警告表示する。外部通知は後回し。
- **バケット**: 1 バケットを `restic/` と `logs/` に分ける。ライフサイクルルールは `logs/` だけに掛ける（`restic/` を期限削除するとリポジトリが壊れる）。
- **S3 への切替**: `config.toml` の endpoint・region・バケット名を変えるだけにする。R2 固有の API は使わない。

**リストア**

1. volume: UI でスナップショットを選ぶ → アプリ停止 → 現在の中身を `/var/lib/koroku/restore-trash/` に退避（7 日保持）→ `restic restore` → アプリ起動。
2. DB ダンプ: UI からダウンロードするところまでを提供する。流し込みは手動（手順を画面に表示）。
3. ホスト全損: 新しい VPS をプロビジョニング → `koroku restore-system` で `koroku.db` と `/etc/koroku` を戻す → 各アプリを再デプロイして volume をリストア → DNS を新 IP に向ける。

**事前準備と秘密情報**

- R2 にバケットを作り、そのバケットに限定した API トークン（Access Key ID / Secret Access Key）を発行して `/etc/koroku/config.toml` に置く。
- restic のリポジトリパスワードは `/etc/koroku/restic.key`。これと R2 の認証情報は VPS の外（パスワードマネージャなど）にも必ず控える。失うとホスト全損時に復元できない。

**バックアップの細則（M5 の実装と実機確認で確定、2026-11-18）**

- restic は 0.17 以上が要る（リポジトリが未作成かどうかを終了コード 10 で判定するため）。初期化はバックアップのときだけ行い、一覧やリストアでは行わない。バケットそのものは `koroku` も restic も作らない。存在しないバケットを指定すると restic が数分リトライしたあと失敗する。
- タグは、基盤が `system` と `kind:system`、アプリが `app:<slug>`・`kind:volume|dump`・`target:<volume 名かダンプフックのサービス名>`。ホスト名は固定の `koroku`。volume と基盤はディレクトリの中から `.` として取るので、ホストやパスが変わってもリストアできる。世代管理は `forget --group-by tags` で、volume・ダンプ・基盤それぞれが自分の世代（日次 7・週次 4・月次 6）を持つ。
- 「アプリの volume」は、そのアプリの Compose プロジェクト（`koroku-<slug>`）のラベル `com.docker.compose.project` が付いた named volume のうち、Docker に実在するもの。名前は Compose ファイルに書いた名前（ラベル `com.docker.compose.volume`。`koroku-blog_pgdata` ではなく `pgdata`）で扱い、除外リストもスナップショットの `target` もこの名前を使う。`external` の volume はラベルを持たないので対象外（他者の持ち物）。Compose ファイルから消えても Docker に残っている volume は対象のまま。`GET /api/apps/{slug}/volumes`（9.1）も同じ列挙を使う。
- ダンプフックは `docker compose exec -T <サービス> sh -c <コマンド>` の標準出力をパイプで restic に渡す。コマンドが失敗したら、restic が入力の終わりを見る前に止めるので、途中までのダンプがスナップショットとして残ることはない。出力が空だった場合も失敗にする。停止中のアプリではダンプフックを実行しない（コンテナがないため）。
- volume のリストアは、アプリ停止 → 現在の中身を `/var/lib/koroku/restore-trash/<slug>-<volume>-<時刻>/` へ移動（7 日保持）→ 復元 → アプリ起動。復元に失敗したら途中まで書いた内容を消し、退避した中身を戻してアプリを起動する。もともと停止していたアプリは停止のままにする。退避先は volume と同じファイルシステムにある必要があり、アプリを止める前に確かめる。
- 実行は同時に 1 つ。定期実行のあとに `forget --prune`、日曜はさらに `check` を行い、どれかが失敗したらその回は失敗として記録する（UI に伝える手段がこれだけなので）。`koroku` の停止と重なった定期実行はやり直さない。
- 「デプロイ前に取得」は、ビルドと pull のあと、コンテナを入れ替える直前に、そのアプリだけを対象に行う。失敗したらデプロイを中止し、稼働中のリリースには触れない。別のバックアップが実行中の場合も中止する（待たない）。初回デプロイでは取るものがないので行わない。
- 稼働中のアプリの volume を取るとき、読めないファイルがあっても（restic の終了コード 3）スナップショットは残し、警告として記録する。
- restic の認証情報は環境変数で渡し、引数・エラー・ログには出さない。キャッシュは `/var/lib/koroku/restic-cache` に置く（systemd のサービスには HOME の下のキャッシュがないため）。
- `koroku restore-system` は、基盤のスナップショットを取り出し、`koroku.db` と設定ディレクトリのファイルを置く。既にあるファイルは上書きせず、バックアップにあった版を隣に `<名前>.restored-<時刻>` として置く。`koroku.db` が既にあるホストでは `--force` なしでは実行しない。
- 基盤のバックアップで設定ディレクトリをコピーするとき、その下にあるデータディレクトリ（`data_dir`）と、コピー先の作業ディレクトリ自身は除外する。ローカル環境では両方を `data/dev` の下に置くため、除外しないと作業ディレクトリを何重にもコピーし続けて失敗した（2026-11-19、ローカル環境で検出）。本番は `/etc/koroku` と `/var/lib/koroku` が別なので影響しない。
- volume のリストアで中身を退避する移動は `rename` で行い、`EXDEV`（マウントをまたぐ移動）で失敗したときだけ、所有者・パーミッション・時刻を保ったコピーと元の削除に切り替える。事前に「同じファイルシステムか」は確かめているが、同じファイルシステムでも別の bind mount を経由すると Linux は `rename` を拒む（ローカル環境は、`koroku` のコンテナにデータディレクトリと Docker の volume ディレクトリを別々にマウントしている）。コピーではハードリンクと拡張属性は保たれないが、退避は手作業での救出用なので許容する。ソケット・デバイス・名前付きパイプはコピーせず、リストアを失敗させて元に戻す。本番は同じマウントなので従来どおり `rename` になる。

実装は M5。受け入れシナリオに「バックアップ取得 → volume を空にする → リストアでデータが戻る」と「4 時間以上前のログが R2 にあり UI からダウンロードできる」を加える。

### 10.3 ログインの強化（2026-11-19）

本番運用を前にして、管理 UI の認証を 3 点強めた。許可ユーザー以外がログインできないことは M3 から変わらない。ここで扱うのは「許可ユーザーの GitHub アカウントか、koroku のセッション Cookie が盗まれた場合」の被害を小さくすることと、盗まれたことに気づけるようにすること。管理 UI をネットワークの段階で閉じる案（Tailscale、クライアント証明書など）は別に検討する。

**GitHub の 2 段階認証を必須にする**

- ログイン時の `GET /user` の応答の `two_factor_authentication` が `true` のときだけセッションを発行する。`false` なら `two_factor_required` で拒否する。
- この項目は GitHub のドキュメントでは「private user の応答」にだけあり、GitHub App のユーザーアクセストークンで返るかは書かれていない。項目がない応答は `two_factor_unknown` で拒否する（確かめられないものを通さない）。返らないと実機で分かった場合に締め出されないよう、`[github] require_two_factor = false` で検査を外せる。既定は `true`。外したときは起動時に警告をログに出す。
- 拒否したログインも、GitHub のユーザー ID が分かっているのでログイン履歴に残す（下記）。

**危険な操作の前に再ログインを求める（step-up）**

- セッションの発行（ログイン）から 10 分以内でなければ、次の操作を `403 reauth_required` で断る。UI は再ログインの案内を出し、GitHub のログインを経て元の画面に戻す。
  - アプリの削除
  - 本番アプリ（`environment = production`）のデプロイ
  - 環境変数の保存と、値の表示（下記）
  - volume のリストアと、DB ダンプのダウンロード
  - アプリ設定のうち、`environment`、`relax_policy`、リポジトリ（`repo_full_name`、`repo_url`、`repo_id`）の変更。本番の確認ダイアログを外す、ポリシー検査を外す、別のコードを動かす、のいずれかにつながるため
- 環境変数の `GET` は、10 分を過ぎていても断らず、値を `null` にして `values_hidden: true` を返す。キーの一覧は見られるようにして、タブそのものは使えるようにするため。
- 再ログインは通常のログインと同じ流れで、新しいセッションを発行する。同じブラウザーの古いセッションはそのとき消す。戻り先は `GET /auth/github/login?return_to=<パス>` で渡し、state と同じく短命の Cookie に入れる。`/` で始まり `//` や `\` を含まないパスだけを受け付ける（外部サイトへのリダイレクトに使われないように）。
- 限界: GitHub にログインしたままのブラウザーでは、再ログインはパスワードを聞かれずに終わる。防げるのは「Cookie だけが盗まれて別の場所で使われる」場合で、本人のブラウザーそのものを使われる場合は防げない。GitHub の OAuth には OpenID Connect の `max_age` に当たるものがない。
- 再ログインを求めない操作: 作成、staging のデプロイ、起動制御、ドメイン、バックアップの実行、prune。どれも取り消せるか、データを外へ出さないため。

**セッションの一覧と失効、ログイン履歴**

- `sessions` に `id`（画面と API で使う識別子。乱数 8 バイトの 16 進。Cookie のハッシュは外に出さない）、`last_seen_at`、`ip`、`user_agent` を足す。`last_seen_at` は 1 分以上たっていれば更新する（ポーリングのたびに書き込まないため）。
- クライアントの IP: `koroku` はループバックで待ち受け、Caddy を経由して届く。接続元がループバックのときだけ `X-Forwarded-For` の最後の値を使う。Caddy は既定でクライアントから来た `X-Forwarded-For` を信用せず、接続元の IP で置き換えるので、偽装できない。User-Agent は 512 バイトで切る。
- `login_events` テーブル: 時刻、結果（`succeeded`、`not_allowed`、`two_factor_required`、`two_factor_unknown`）、GitHub のユーザー ID と login、IP、User-Agent。GitHub のユーザーが分かったものだけを残し、state の不一致や GitHub の失敗は残さない。誰でも起こせて、記録が溢れるだけなので。新しい 200 件を残して古いものを消す。
- 画面: ユーザーメニューから「セキュリティ」を開き、セッションの一覧（このセッションを示す、個別に終了、ほかをすべて終了）と、ログイン履歴を見る。

## 11. 実装フェーズ（Claude Code 向け）

> この章は初版の計画。独自ドメイン・ログ・バックアップを含む最新のマイルストーンと受け入れシナリオは、リポジトリ直下の `CLAUDE.md` を正とする。

デプロイエンジンを先に固め、UI は最後に載せる。各フェーズは単体で動作確認できる状態で終える。

| フェーズ | 内容 | 完了条件 |
| --- | --- | --- |
| M0 骨格 | リポジトリ構成、設定読込、SQLite マイグレーション、Ember の空アプリを embed 配信、CI | `koroku` が起動し `/api/system` が応答する |
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

**名前の変更（2026-11-19）**

- コントロールプレーンの名前を `paasd` から `koroku` に変えた。バイナリ、systemd の unit、設定とデータのディレクトリ（`/etc/koroku`、`/var/lib/koroku`）、Compose プロジェクト名とイメージ名の接頭辞（`koroku-<slug>`）、Cookie、環境変数（`KOROKU_*`）まで、この名前にそろえる。理由は 3 つある。
  - リポジトリと Go モジュールは先に `koroku` になっていて、同じものに名前が 2 つあった。
  - 旧名は「PaaS のデーモン」という種類の説明で、固有の名前ではなかった。
  - まだどこにもデプロイしていないので、今ならパス、Compose プロジェクト名、イメージタグの移行が要らない。旧名のパスや Cookie を読む互換処理も入れない。

**契約時の注意**: さくらの VPS は Linux カーネル脆弱性への対応で一部の受付を止めている。2026-11-13 追記時点で、大阪リージョンの Linux 通常容量プランの新規申し込みと、スケールアップが停止中（[対応状況](https://vps.sakura.ad.jp/contents/info/202610-pendingapply/)）。東京か石狩で、後から上げられない前提で最初から 4GB プランを選ぶ。構築直後にカーネルを更新する。

**実機での確認が要るもの（2026-11-19）**

- GitHub App のユーザーアクセストークンで `GET /user` に `two_factor_authentication` が含まれるか（10.3）。含まれなければ `[github] require_two_factor = false` にして、この節に結果を書く。

**残り**

- [ ] Squarespace の Custom Records で Host `*.dev` が登録できるか（M2 で確認。不可なら個別 A レコード）
- [x] `--env-file` 指定時にリポジトリ内の `.env` がどう扱われるか（M1 で実機確認済み、下記）
- [ ] バックアップとログの保持期間（既定案: バックアップは日次 7・週次 4・月次 6、ログは R2 に 90 日）
- [ ] 起動失敗時の自動ロールバックを既定で有効にするか（既定案: 有効）
- [ ] push 時の自動デプロイを将来入れるか（入れるなら Webhook の受け口を M3 で用意する）

**実機確認の結果**

- `--env-file` とリポジトリ内の `.env`（2026-11-18、Docker 29.8.0 / Compose v5.8.0、ローカル環境）: `--env-file` を指定すると、プロジェクトディレクトリの `.env` は `${VAR}` の補間に使われない（`.env` にしかない変数は未定義扱いになり、`${VAR:-default}` は default になる）。一方、サービスが自分で `env_file: .env` と書いている場合、その内容はコンテナの環境変数に入る。override で足した `koroku` の env ファイルはリストの後ろに付くので、同じ名前の変数は `koroku` 側の値が勝つ。つまり、補間は `koroku` の環境変数だけで決まり、リポジトリの `.env` は「サービスが明示的に読む」ときだけ効く。`testdata/apps/build-app` と結合テストで固定している。

- 設定再投入時の接続（2026-11-18、Caddy 2.11.9 の公式 Docker イメージ、ローカル環境。**VPS 上では未確認**）: SSE の `curl -N` と、`Upgrade: websocket` で 101 を受けたあと 1 秒ごとに 1 行流す接続を保ったまま `POST /load` した。
  - 同一内容の投入は Caddy が `config is unchanged` として何もしない。
  - 内容が変わる投入（アプリを 1 つ追加）と強制再読込では、サーバーが作り直されるが、SSE は 24 秒間の観測で 1 行も欠けなかった（平文、および TLS + HTTP/2 のどちらでも）。新しいルートは投入の 1 秒後には応答した。
  - アップグレード済み接続は、`stream_close_delay` なし（Caddy の既定）だと再読込の瞬間に切れた。`stream_close_delay: 10s` では再読込の 10.0 秒後に切れた。つまりこの指定は切断をなくすのではなく先送りする。生成する設定では 5 分にしているので、アプリの作成・ドメインやポートの変更・起動や停止のたびに、他アプリの WebSocket は最長 5 分後に 1 回切れる（クライアントの再接続に任せる）。5 分ちょうどで切れるところまでは待って確認していない。
  - 実際の ACME 発行（Let's Encrypt staging を含む）は VPS と DNS が要るので未確認。

- ローカル環境のバックアップ（2026-11-19、Versity S3 Gateway v1.11.0、restic 同梱の開発用イメージ、WSL2 の snap 版 Docker）: 受け入れシナリオ 7 を手元で実施した。Rails + PostgreSQL のアプリに確認用の行を入れてバックアップ → アプリ停止 → volume を空にして起動（DB は空で初期化される）→ UI と同じ API でリストア → 行が戻り、アプリも応答した。退避した中身は所有者（postgres の UID 70）を保っていた。
  - S3 互換サーバーは、MinIO のリポジトリがアーカイブされたため、Versity S3 Gateway、RustFS、Garage、SeaweedFS、MinIO のコミュニティフォークを比較した。koroku の `TestIntegrationS3`（13 項目）と restic（init・backup・`--stdin`・`forget --prune`・`check --read-data`・restore）を各サーバーで実行し、Garage 以外は全項目通過した（Garage は期限切れの presigned URL に 400、署名の誤りに `AccessDenied` を返す。`koroku` 本体はこのエラーコードに依存しない）。ローカルには、イメージ 82MB・待機時のメモリ約 29MiB と最も軽く、環境変数だけで設定でき、中身が volume にファイルとして並ぶ Versity S3 Gateway を選んだ。本番は R2 のまま。
  - `koroku` のコンテナは利用者の UID で動くため、volume のファイル（root や postgres の所有）を読み書きできるよう、`chown`・`dac_override`・`fowner`・`fsetid` の 4 つだけを ambient capability で持たせた（`DEV_FILE_CAPS=1`。どれも Docker が既定でコンテナに与える範囲）。Docker の volume ディレクトリはホストと同じパスでマウントする。Docker Desktop は volume が VM の中にあるので、この方法では届かない。

- ローカル環境のログ退避（2026-11-19、同じ環境）: 受け入れシナリオ 8 を手元で実施した。開発用の設定は退避の間隔を最小の 1 時間にしている。`koroku` を再起動すると、起動時の取りこぼし処理で直前の時間枠までのアプリのログが `logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz` に置かれ、`GET /api/apps/{slug}/logs/archives` に出た。presigned URL（`http://127.0.0.1:9000/...`、有効 15 分）からのダウンロードは、WSL 内と Windows 側の `curl.exe` のどちらも 200 で、展開すると収集したログの行と一致した。任意の時点で退避を走らせる手段は足さなかった。キーは時間枠の開始時刻で決まるので、時間枠の途中で送ると、後で同じキーを上書きしてしまうため。

- ローカル環境の Caddy（2026-11-19、Caddy 2.11.9 の公式 Docker イメージ、WSL2）: `listen` を `127.0.0.1:80` / `127.0.0.1:443` にしても、HTTP から HTTPS への自動リダイレクトは挿入された（`http://<slug>.dev.localhost/` が 308）。内蔵 CA で `*.dev.localhost` の証明書が発行され、ルート証明書を信頼すれば検証も通る。WSL2 の localhost 転送で、Windows 側の `curl.exe` からも `https://<slug>.dev.localhost/` に届いた。

- ワンショットのサービスと `up --wait`（2026-11-19、Docker 29.8.0 / Compose v5.8.0、ローカル環境）: `restart: "no"` で `true` を実行して終わるサービスを置くと、どのサービスからも待たれていない場合は `up -d --wait` が `container ... exited (0)` で失敗した（終了コード 1）。公開サービスが `depends_on` の `condition: service_completed_successfully` で待つ場合は成功した。デプロイの手順 6 は `up --wait` なので、マイグレーションなどのワンショットのサービスは、必ず別のサービスから `service_completed_successfully` で待たれる形にする必要がある。基盤側では対処せず、利用者向けの Skill（`plugins/koroku/skills/prepare-app`）に書いた。

- `docker compose logs --follow` の二重出力（2026-11-18、Docker 29.8.0 / Compose v5.8.0、ローカル環境）: コンテナがまだないプロジェクトに対して `logs --follow --timestamps` を繰り返し起動しながら `up -d` すると、起動直後のコンテナの行が、時刻まで同じ内容で 2 回ずつ出る流れに当たることがあった（1 行だけ出すコンテナで 8 回中 5 回、0.5 秒ごとに出し続けるコンテナでは 6 回中 6 回で、以後の行もすべて 2 回ずつ）。`koroku` のログコレクタはこの状況で実際に同じ行を 2 回保存していた（開発用の Compose 環境でサンプルアプリをデプロイして発見）。対処は 10.1 章。

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
