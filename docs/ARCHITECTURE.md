# koroku アーキテクチャ

koroku の現在の設計を書く。過去の経緯や変更の履歴は書かない（それは git の履歴と PR にある）。利用者向けの説明は [README](../README.md)、HTTP API の形は [API.md](API.md)、構築と運用の手順は [SETUP.md](SETUP.md) と [OPERATIONS.md](OPERATIONS.md)。

## 1. 目的とスコープ

koroku は、3〜5 個の Docker Compose アプリを、VPS 1 台の上で GitHub リポジトリから UI 操作だけでデプロイする個人用のコントロールプレーンである。以下、本体デーモンを `koroku`、ベースドメイン（`config.toml` の `base_domain`）を例として `dev.example.com` と書く。特定のホスティング事業者、DNS サービス、オブジェクトストレージには依存しない。それぞれに求める条件は [SETUP.md](SETUP.md) にまとめる。

**提供するもの**

- GitHub リポジトリ連携（private 含む）と、UI からのブランチ／コミット指定デプロイ
- `compose.yaml` のデプロイ。`build:` があればホストでビルド、`image:` のみなら pull
- アプリごとの `<slug>.dev.example.com` 自動割当と TLS、アプリごとの独自ドメイン追加（本番用）
- 環境変数の管理、デプロイログと実行ログの閲覧
- 起動・停止・再起動・削除、過去コミットへの再デプロイ（ロールバック）
- バックアップとリストア、実行ログの保管

**提供しないもの**

- `git push` デプロイ、Addons、buildpack
- マルチノード、マルチユーザー、オートスケール
- 無停止デプロイ（再作成中の数秒の断は許容）
- push 時の自動デプロイ（Webhook を受けない）
- ホストの冗長化（ホスト障害時は全アプリが止まる）
- Loki などのログ基盤、外部への障害通知

## 2. 設計の要点

| 項目 | 方式 | 理由 |
| --- | --- | --- |
| 実行基盤 | VPS 1 台（Ubuntu 24.04 か 26.04、x86_64）。Docker Engine + Compose v2.30 以上 | override で使う `!override` タグ（2.24.4 以上）と、ポリシー検査で使う `config --no-env-resolution`（2.30 以上、6 章）の要件（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)） |
| コントロールプレーン | Go 製シングルバイナリ `koroku`。systemd でホストに直置き、SQLite | 常駐メモリが小さく、配布と更新がファイル 1 つ。詳細は 4 章 |
| フロントエンド | Ember SPA。ビルド成果物を `koroku` に埋め込んで配信 | 配信用の別プロセスが不要 |
| リバースプロキシ | Caddy。ホストに直置きし、admin API で設定を投入 | ホスト名を渡すだけで証明書の取得と更新まで自動（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)） |
| アプリへの経路 | 公開サービスを `127.0.0.1:<割当ポート>` に publish し、Caddy がそこへ転送 | 共有 Docker ネットワークが不要で、アプリ同士が互いに見えない。例外は、利用者が画面で作った接続だけ（6.1） |
| TLS | ホスト名ごとに個別証明書（HTTP-01）。ワイルドカード「証明書」は見送り | ワイルドカード発行には DNS-01 が要り、DNS 事業者の API 連携が増える（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)） |
| GitHub 連携 | 自分専用の GitHub App。ログインと clone の両方に使う | clone 用トークンが 1 時間で失効し、長寿命の秘密を置かずに済む（5 章） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p koroku-<slug>`） | プロジェクト名でリソースが分離される（[How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)） |
| ビルド | `build:` があればホストで build、なければ pull | リポジトリの形に合わせて両方に対応する |
| ジョブ実行 | `koroku` プロセス内キュー、全体で同時実行 1 | ビルドの負荷を直列化。Redis 等の外部依存が不要 |
| UI 認証 | GitHub ログイン + 許可ユーザー ID 1 件の照合 | 個人用途。パスワード管理を持たない |

## 3. 全体アーキテクチャ

常駐プロセスは Caddy と `koroku` の 2 つだけで、どちらもホストの systemd サービスとして動かす。アプリは Compose プロジェクトとして並び、外部への公開は Caddy 経由の 80/443 に限る。

```text
                      Internet
                         | 80 / 443
+------------------------v--------------------------------- VPS --+
|  Caddy (systemd)                                                |
|    console.dev.example.com -> 127.0.0.1:8080  (koroku)        |
|    app-a.dev.example.com   -> 127.0.0.1:20001 (app-a / web)   |
|    app-b.dev.example.com   -> 127.0.0.1:20002 (app-b / web)   |
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
| `/etc/koroku/config.toml` | ベースドメイン、公開 IP、GitHub App、オブジェクトストレージなどの設定（0600） |
| `/etc/koroku/github-app.pem`、`/etc/koroku/master.key`、`/etc/koroku/restic.key` | GitHub App 秘密鍵、環境変数暗号化のマスターキー、restic のパスワード（0600） |
| `/var/lib/koroku/koroku.db` | SQLite |
| `/var/lib/koroku/apps/<slug>/releases/<deploy_id>/` | チェックアウトしたソースと生成した override |
| `/var/lib/koroku/apps/<slug>/env` | 展開済み環境変数ファイル（0600） |
| `/var/lib/koroku/apps/<slug>/release-state/<deploy_id>/` | そのリリースの環境変数スナップショット（0600）と、使った Compose ファイル名・ビルドしたイメージ名。ロールバック用（7 章） |
| `/var/lib/koroku/apps/<slug>/deploys/<deploy_id>.log` | デプロイログ |
| `/var/lib/koroku/logs/<slug>/<service>/` | 収集した実行ログ（10.1） |
| `/var/log/caddy/apps/<slug>.log` | Caddy のアクセスログ（アプリ別） |

## 4. 技術スタック選定

**Go（API）+ Ember（SPA）+ SQLite** で作る。このシステムの中心は CRUD ではなく「外部プロセスの実行・ログのストリーム配信・状態機械」で、小さな VPS でアプリとメモリを分け合うため、常駐コストと配布の軽さを優先している。

- Go は常駐メモリが数十 MB で、配布と更新がバイナリ 1 つの差し替えで済む。プロセス実行と SSE は標準ライブラリで足りる。
- Docker は `docker compose` CLI を呼んで操作する。Docker Engine の Go SDK は使わない。
- SQLite ドライバは cgo 不要の [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) を使い、クロスコンパイルと単一バイナリを保つ。

**構成の詳細**

- Go: 標準 `net/http`、`database/sql`、SQL マイグレーションは埋め込みファイルを起動時に適用。フレームワークは使わない。
- Ember: TypeScript。API は JSON の REST、ログは SSE（`EventSource`）で受ける。
- リポジトリ: `cmd/koroku`、`internal/`（`api`、`deploy`、`compose`、`caddy`、`routing`、`github`、`domains`、`logs`、`backup`、`store`、`runner`、`preflight` など）、`web/`（Ember）、`deploy/`（プロビジョニング）。

## 5. GitHub 連携

自分のアカウントに非公開の GitHub App を 1 つ登録し、UI ログインとリポジトリ取得の両方に使う。権限は Contents: Read-only と Metadata: Read-only のみ。

| 用途 | 方式 |
| --- | --- |
| UI ログイン | Web application flow でユーザーアクセストークンを得て `GET /user` の ID を許可 ID と照合し、自前のセッション Cookie を発行する。トークンは保存しない（[ユーザーアクセストークンの生成](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)） |
| リポジトリ・ブランチ・コミット一覧 | installation access token で REST API を呼ぶ |
| clone | デプロイのたびに installation access token を発行し、HTTP パスワードとして使う。形式は `https://x-access-token:TOKEN@github.com/owner/repo.git`、有効期限は 1 時間（[installation としての認証](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)） |

- トークンは `.git/config` やデプロイログに残さない。`GIT_ASKPASS` か `http.extraheader` で渡し、fetch 後に破棄する。
- Webhook は受けない。デプロイは常に UI か API から手動で始める。

**ログインと保護の細則**

- セッションは 7 日で失効し、操作しても延長しない。Cookie の値は乱数 32 バイトで、DB には SHA-256 のハッシュだけを置く（DB が漏れてもセッションを再利用できないようにするため）。`config.toml` の許可ユーザー ID を変えると、既存のセッションは次のリクエストから無効になる。
- 変更系リクエスト（`POST` / `PUT` / `PATCH` / `DELETE`）は、`Origin` ヘッダーが管理 UI のオリジン（`console_url`、既定は `https://<console_host>`）と完全一致するものだけを通す。ヘッダーがないリクエストも拒否するので、`curl` で叩くときは `-H "Origin: https://console.<base>"` を付ける。
- GitHub App が未設定（`[github]` セクションなし）のときは、誰もログインできず API は 401 を返す。「未設定なら素通し」にはしない。ローカル開発では `koroku serve --insecure-no-auth` で認証と Origin 検証を外せる。設定ファイルではなく起動フラグにしているのは、本番の unit ファイルに紛れ込みにくくするため。
- `[github]` は全項目（`app_id`、`client_id`、`client_secret`、`private_key_file`、`allowed_user_id`）が揃っていなければ起動時にエラーにする。秘密鍵ファイルはグループや他人が読める権限なら拒否する。
- リポジトリの指定が `repo_full_name`（`owner/name`）のアプリは installation token を使って取得する。GitHub App がそのリポジトリにインストールされていなければ、public リポジトリの可能性があるので認証なしで取得を試みる。`repo_url` を明示したアプリには token を使わない。
- アプリ作成時の Compose ファイル検出（`GET /api/repos/{owner}/{repo}/compose`）は、サービス名とポートを画面に下書きするためのもので、YAML パーサーの依存を増やさず行ベースの簡易読み取りで済ませる（アンカーやフロースタイルは読めない。読めなければ手入力）。デプロイ時の判定は `docker compose config` の結果で行う。
- fine-grained PAT は使わない。長寿命の秘密をサーバーに置くことになり、ログイン手段も別に要るため。

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
- `--env-file` を指定するので、リポジトリ直下の `.env` は `${VAR}` の補間に使われない（`.env` にしかない変数は未定義になり、`${VAR:-default}` は default になる）。サービスが自分で `env_file: .env` と書いていれば、その内容はコンテナに入る。override が足す koroku の env ファイルはリストの後ろに付くので、同じ名前の変数は koroku 側の値が勝つ。つまり補間は koroku の環境変数だけで決まり、リポジトリの `.env` は「サービスが明示的に読む」ときだけ効く（`testdata/apps/build-app` と結合テストで固定）。
- named volume はプロジェクト名 `koroku-<slug>` が接頭辞になるため、リリースディレクトリが変わっても引き継がれる。

**override の細則**

- `restart: unless-stopped` は、リポジトリ側が `restart` を指定していないサービスにだけ付ける。全サービスに付けると、マイグレーション用のワンショットサービス（`restart: "no"`）が終了後に再起動し続けてしまうため。
- `build:` を持つサービスには、リリースごとのイメージ名 `koroku-<slug>-<service>:r<deploy_id>` を override の `image:` で与える。Compose の既定では毎回 `<project>-<service>:latest` に上書きされ、起動に失敗した新版のイメージで旧版のタグが置き換わり、旧リリースのディレクトリで `up` し直しても壊れたイメージが起動してしまう。リリース固有のタグにしておくと、ロールバックは旧版のイメージそのものを起動できる。イメージはリリースディレクトリと一緒に削除する。
- メモリ上限の決まり方は、優先度の高い順に「アプリ設定のサービス別の値」「アプリ設定の全体値」「リポジトリの Compose が自分で指定した値（`config.toml` の既定値以下のときだけ）」「`config.toml` の既定値（512m）」。既定値は、リポジトリが自分で名乗れる上限でもある。それより大きくしたいときはアプリ設定で指定する（Compose に `mem_limit: 64g` と書くだけで上限を外せてしまうのを防ぐため）。リポジトリ側が `deploy.resources.limits.memory` で書いている場合は override も同じ形式で書く。`mem_limit` と値が食い違うと Compose が `can't set distinct values` で設定全体を拒否するため。
- override に書くサービスは、リポジトリの Compose を正規化した結果（`config --format json`）に存在するものだけにする。存在しないサービス名を書くと Compose がエラーにするため、正規化 → override 生成 → もう一度正規化してポリシー検査、の 2 回 `config` を実行する。
- 正規化した JSON には `env_file` の中身が展開されて入るので、ログには出さない。
- env ファイルの値は Compose に補間されない形で書く。英数字と一部の記号だけの値はそのまま、それ以外はシングルクォート、シングルクォートか改行を含む値はダブルクォートで `\ \" \$ \n` をエスケープする。`$`・空白・`#`・引用符を含む値がコンテナにそのまま届くことを結合テストで確認している。

**ビルドと pull の判定**

1. `docker compose -p koroku-<slug> -f compose.yaml -f koroku.override.yaml config --format json` で正規化する。
2. `build` を持つサービスがあれば `docker compose build`、`image` のみのサービスは `docker compose pull`。
3. private レジストリ（GHCR など）は、ホストで `docker login` しておく（`/root/.docker/config.json`）。

**ポリシー検査（正規化した JSON に対して実施、違反はデプロイ拒否）**

- `privileged: true`、`network_mode: host`、`pid: host`
- `/var/run/docker.sock` のマウント
- リリースディレクトリ外への bind mount
- 公開サービス名が Compose に存在しない

個人用途なので、アプリ設定の「検査を緩める」フラグで個別に許可できるようにする。

**そのほかの検査項目**

Compose には上と同じ結果になる書き方がほかにもあるので、次も拒否する（いずれも「検査を緩める」フラグで許可できる）。

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

### 6.1 アプリ間の接続

アプリ同士は互いに見えないまま、一部のサービスだけを互いに呼べるようにする（例: 2 つのアプリから同じ認証サービスを使う）。利用する側のアプリは自分の Compose の中で決めた名前（例: `auth`）で相手を呼び、それがどのアプリのどのサービスを指すかを koroku の画面で決める。koroku は接続ごとに専用のネットワークを作り、相手のサービスをその名前でつなぐ。どのアプリがどこへ届くかを koroku が管理するので、`foreign_network` の検査を緩めて外部ネットワークを名前で指す必要はない。

**言葉**

| 言葉 | 意味 |
| --- | --- |
| グループ | 接続してよい範囲。利用者が作り、同じ `environment` のアプリだけが入れる |
| 公開するサービス | グループのほかのアプリが接続先に選べる、自分のサービス |
| 接続 | 「利用側アプリの、この名前は、相手アプリのこの公開サービスを指す」という 1 件。利用側のどのサービスがその名前を使うかも決める |

**方式**

| 項目 | 方式 | 理由 |
| --- | --- | --- |
| 構成を決める場所 | グループ、所属、公開するサービス、接続は UI で決め、DB にだけ持つ。リポジトリの Compose からは決められない | DB が唯一の正。リポジトリの変更で他アプリへの経路が増えないようにする |
| 所属 | 1 つのアプリが入れるグループは 1 つまで。接続は同じグループのアプリの間だけ | グループを、接続してよい範囲の枠にする |
| 環境 | グループのメンバーは `environment` が同じアプリだけ | staging から本番の DB に届く構成を作れないようにする（8.2） |
| ネットワーク | 接続 1 件ごとに `docker network create --internal` で作る。名前は `koroku_link_<設置 ID>_<接続 ID>`、ラベルは `io.koroku.installation=<設置 ID>` と `io.koroku.link=<接続 ID>` | 名前の意味が利用側ごとに閉じ、届く範囲が接続した 2 つのアプリの間に絞られる。`--internal` で外への経路にならない（各コンテナは自分の default ネットワーク経由で外へ出られる）。slug に `_` は使えないので、どのアプリのネットワーク（`koroku-<slug>_*`）とも重ならない |
| つなぎ方 | Compose の設定には書かず、koroku が `docker network connect` でつなぐ。相手のサービスは `--alias <名前>`、利用側のサービスは別名なし | 下記 |
| 設置 ID | 初回起動時に乱数 8 桁（16 進）を作って DB に置く | 同じ Docker を使う別の koroku（開発用のスタック、結合テスト）と、ネットワークの名前も照合の範囲も重ならないようにする。DB ごと復旧すれば ID も戻る |
| 名前 | DNS のラベルの形（`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`）。利用側で重複不可。利用側自身のサービス名・コンテナ名と同じ名前は不可 | 自分のサービスと同じ名前にすると、そのサービスが止まっている間に相手へ解決される（下記） |
| つなげないサービス | `container_name` か `network_mode` を指定したサービスは、公開も、接続の利用側にもできない | `container_name` は任意の名前をネットワークに載せられる。`network_mode` のサービスは自分のネットワークを持たない |

**Compose でつながない理由**: Compose の `networks` で共用ネットワークにつなぐと、サービス名そのもの（`db` など）がそのネットワークの DNS 名に載る。`aliases` を明示してもサービス名は外れない。すると、利用側の `db` が止まっている間（再起動中やデプロイ中）だけ、同じ名前が相手の `db` に解決され、利用側のアプリが別アプリの DB へ接続しうる。`docker network connect --alias` でつなぐと、DNS 名はコンテナ名、指定した別名、コンテナ ID だけになる。接続ごとにネットワークを分けるので、利用側 2 つがそれぞれ別の相手を同じ `auth` で引け、利用側同士や接続していない相手には名前で届かない。

**起動の手順**

Compose が管理していないネットワークへの接続は、設定が変わらない `up` では残り、コンテナが作り直されると消える。そこで、接続に関わるアプリ（利用側でも相手側でも）の `up`（デプロイの手順 6、起動、失敗時の自動ロールバック、保持しているリリースへのロールバック（7.1））は次の 3 段階にする。関わらないアプリは通常の `up` だけ。

1. `docker compose up -d --no-start --remove-orphans` で、コンテナを作る（設定が変わったものは作り直す）。
2. DB の接続から、各コンテナがつながるべきネットワークを決め、足りない接続をつなぎ、余分な接続（この設置の `koroku_link_*` のうち、DB で許されていないもの）を切る。ネットワークがなければ作る。
3. `docker compose up -d --remove-orphans --wait`。手順 1 で作ったコンテナを起動して待つ。設定は変わっていないので作り直されず、2 の接続は残る。

起動する前につなぐのは、起動時に相手へ接続するサービスのヘルスチェックを通すため。つなぐ先は毎回 DB から決めるので、接続を消したあとに古いリリースへロールバックしても、消した接続は戻らない。

**ポリシー検査**

- リポジトリの Compose が `koroku_link_*` のネットワークを指すことは、「検査を緩める」フラグを立てても許可しない（`link_network`）。接続のネットワークにつなぐのは koroku だけにする。ポートの公開（6 章）と同じ扱い。
- 公開するサービスと、接続の利用側のサービスがリリースにあり、つなげる形であること、接続の名前が自分のサービス名・コンテナ名と重ならないことを、デプロイの手順 4 と、起動・ロールバックの検査で確かめる（`link_service`）。満たさなければ起動しない。
- volume の共有の禁止（`foreign_volume`）と、外部へのポート公開の禁止は変わらない。共有するのはネットワークだけ。

**作成と削除**

- 接続を作ると、相手側のコンテナ（作られていれば）をすぐにつなぐ。利用側は次の `up`（デプロイ、または起動）からつながる。
- 届く範囲を狭める変更は、次のデプロイを待たずにすぐ切る。接続の削除、グループからの離脱、公開するサービスの解除、アプリの削除が該当する。離脱と公開の解除では、それに依存する接続も一緒に消す。消した接続のネットワークは、つながっているコンテナを切ってから消す。
- グループを消せるのはメンバーがいないときだけ。ネットワークの作成と削除は koroku だけが行う。
- `koroku` の起動時に、この設置のラベルが付いたネットワークだけを DB と照合して直す。DB にあるのに存在しないネットワークは作り、DB にないものは消し、DB で許されていないコンテナは切る。ほかの設置のネットワークには触れない。`koroku restore-system` でホストを作り直したときも、この照合でネットワークが戻る。
- `docker compose restart` と Docker の再起動ポリシーによる再起動では、接続は残る。

**UI と API**

- システム画面にグループの一覧と作成・名前の変更・削除を置く。メンバーごとに公開するサービスと接続を表示する。
- アプリの設定タブで、所属するグループ、公開するサービス、接続（名前、相手のアプリとサービス、利用側のサービス）を設定する。
- production のアプリのグループと接続の変更には、確認ダイアログと再ログインを求める（10.3）。
- 公開したサービスは、接続した利用側のサービスと双方向に全ポートで届くことを画面に書く。DB と思われるサービスを公開するときは警告を出す。
- API の形は `docs/API.md`。

**制約**

- 接続した 2 つのサービスの間は双方向で、全ポートに届く。Docker のネットワークでは向きもポートも絞れない。絞るには、利用側に中継用のコンテナを置いて決めたポートだけを転送する方式が要る（中継用のイメージを koroku が配布・更新する負担があるので、持たない）。
- 通信は暗号化されない。接続先でも、DB などは認証を必須にしたまま使う。
- `docker compose up` を koroku の外から手で実行してコンテナが作り直されると、接続が外れる。koroku の起動か再デプロイでつなぎ直る。

## 7. デプロイフロー

デプロイは `queued → running → succeeded | failed | canceled` の状態を持つ 1 ジョブで、次の順に進む。

1. **受付**: UI でブランチかコミットを選ぶ。API がコミット SHA を確定し、`deployments` に `queued` で記録する。
2. **取得**: installation token を発行し、`releases/<deploy_id>/` に `git fetch --depth 1 origin <sha>` でチェックアウトする。
3. **生成**: env ファイル（0600）と `koroku.override.yaml` を書き出す。
4. **検証**: `docker compose config --format json` を実行し、6 章のポリシー検査をかける。
5. **ビルド／pull**: 6 章の判定に従う。
6. **起動**: `docker compose up -d --remove-orphans --wait --wait-timeout 300`。`--wait` はサービスが running または healthy になるまで待つ（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。
7. **経路**: Caddy 設定を再生成し、内容が変わっていれば投入する（8 章）。
8. **確認**: `http://127.0.0.1:<port><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒リトライする。
9. **完了**: `succeeded` にして `current_deployment_id` を更新。リリースディレクトリは直近 3 件を残し、`docker image prune -f` を実行する。

**失敗時**

- 手順 5 までの失敗は稼働中のコンテナに触れないので、`failed` を記録するだけ。
- 手順 6 以降の失敗は、直前の成功リリースのディレクトリで `up` し直して旧版に戻す。
- `koroku` 再起動時に `running` のまま残ったジョブは `failed (interrupted)` にする。
- 直前の成功リリースがない（初回デプロイの）場合は戻す先がないので、`docker compose down` で起動しかけたコンテナを消す（volume は残す）。
- 手順 6 の `up` か手順 8 の確認で失敗したら、戻す前に `docker compose logs --tail 30` の出力をデプロイログのその手順に書く（見出しは `Last output of the containers:`）。起動に失敗した理由はたいていコンテナの出力にあるが、ロールバックでコンテナが置き換わると、アプリのログ画面からしか追えなくなるため。この出力もデプロイログのマスクを通る。読み出しに失敗してもデプロイの結果は変えない。キャンセルや `koroku` の停止による失敗では読まない。
- ロールバックでは環境変数も戻す。env ファイルはリリースごとにスナップショットを取り（`release-state/<deploy_id>/env`）、稼働中の `apps/<slug>/env` を書き換えるのは手順 6 の直前にする。こうすると、手順 5 までの失敗では稼働中のリリースの env に触れず、手順 6 以降の失敗では旧版を「当時の環境変数」で起動し直せる。環境変数の変更が原因で起動に失敗した場合にも旧版に戻れるようにするため。
- `koroku` の停止（SIGTERM）と重なったデプロイは `failed (interrupted)` にし、停止処理の中ではロールバックを走らせない。コンテナが中途半端な状態で残りうるので、`koroku` の更新はデプロイ中を避ける。
- `docker compose up --wait` は、ヘルスチェックを持たないコンテナが起動直後に落ちて再起動を繰り返していても成功を返す。起動失敗の検出は手順 8 の HTTP 確認が担う。

**その他の規則**

- 同時実行は全体で 1。同じアプリに後続のデプロイが積まれたら、古い `queued` は `canceled` にする。
- 各手順の stdout/stderr を行単位でログファイルに追記し、SSE で配信する。環境変数の値とトークンはマスクする。
- 手動のロールバックは、保持しているリリースを再ビルドせずに起動し直す（7.1）。保持していない古い版へは「過去の SHA を同じフローで再デプロイ」する。どちらも DB マイグレーションの巻き戻しは行わない。
- マイグレーション等のリリース処理は Compose 側で表現する（起動コマンド、またはワンショットのサービスと `depends_on`）。基盤側に専用機能は持たない。
- ワンショットのサービス（`restart: "no"` で終わるもの）は、別のサービスから `depends_on` の `condition: service_completed_successfully` で待たれていなければならない。待たれていないと、手順 6 の `up -d --wait` が `container ... exited (0)` で失敗する。基盤では対処せず、利用者向けの Skill（`plugins/koroku/skills/prepare-app`）で案内する。

**デプロイ時の断の範囲**: 断はデプロイ対象のアプリだけに起きる。`docker compose up` は `-p koroku-<slug>` で指定したプロジェクトのコンテナしか操作せず、その中でも設定かイメージが変わったサービスだけを再作成する（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。他アプリへの間接的な影響は 2 つある。

- ビルド中の CPU・メモリ負荷で他アプリの応答が遅くなりうる（停止はしない）。8.2 のメモリ上限とビルド直列化で抑える。
- Caddy 設定は内容が変わったとき（アプリの作成、ドメイン・ポートの変更、起動・停止など）だけ投入し、通常の再デプロイでは投入しない。投入しても他アプリの SSE と通常のリクエストは切れない。WebSocket などのアップグレード済み接続は 8 章の `stream_close_delay` の猶予のあとに 1 回切れる。

### 7.1 保持しているリリースへの即時ロールバック（2026-11-21）

手動で前の版に戻すには「過去の SHA を再デプロイ」するしかなく、取得・ビルド・pull を全部やり直していた。4GB の VPS ではビルドに数分かかり、障害の最中に待つことになる。一方、koroku は直近のリリースを、ディレクトリ（チェックアウトと override）、リリースごとのイメージ（`koroku-<slug>-<service>:r<id>`）、その時の環境変数のスナップショットごと残している（7 章の失敗時の自動ロールバックのため）。これを画面から使えるようにする。Heroku の `releases:rollback` に相当する。

- **ロールバックもデプロイの 1 件として記録する**。`deployments` に `source_deployment_id`（戻す先のデプロイ）を足す。コミットとメッセージは戻す先のものを写し、`ref` は `rollback` にする。キュー、同時実行 1、キャンセル、ログ、SSE、本番の再ログインは通常のデプロイと同じ。
- **手順**は `prepare`（戻す先のリリースのファイルと環境変数のスナップショットが残っているか確かめる）→ `validate`（7 章の起動前の再検査）→ `start`（スナップショットの環境変数を稼働中の env ファイルに書き、戻す先のリリースのファイルで `up`）→ `route` → `check`。取得、ビルド、pull、ディスク使用率の検査は行わない。`start` 以降で失敗したら、通常のデプロイと同じく直前の成功リリースへ自動で戻す。
- **環境変数も戻す**。リリースは「コードとその時の設定の組」なので、戻す先のリリースが動いていた時の値で起動する（失敗時の自動ロールバックと同じ考え方）。今 DB に保存している値とは違いうるので、環境変数の画面には「未反映の変更」として差が出る。指紋（9.2）は戻す先のものを写す。
- **成功したら、アプリの現在のリリース（`current_deployment_id`）は戻す先のデプロイを指す**。ロールバックの記録自体はリリースのファイルを持たないため。起動・停止、次回の失敗時の自動ロールバック、環境変数の差の計算は、そのまま戻す先のリリースを使う。
- **リリースの保持**: 成功後の整理は、成功したデプロイを新しい順にたどり、異なるリリースを N 個（既定 3）残す。ロールバックの記録は、それが指すリリースとして数える。現在のリリースも必ず残す。成功したデプロイ N 件で数えると、ロールバック直後の整理でいま動いているリリースが消え、ロールバックを 2 回続けると戻し先の新しい版も消えてしまう。
- **戻せる版**: 成功していて、ファイルとスナップショットが残っているもの。ロールバックの記録を指定したときは、その戻す先に読み替える。デプロイの JSON に `rollback_available`（真偽値）と `source_deployment_id` を足し、UI は戻せる版にだけ「この版に戻す」を出す。保持していない古い版は、従来どおり SHA の再デプロイで戻す。
- **DB のマイグレーションは戻さない**。新しい版で DB を変えていれば、古い版が動かないことがある。UI の確認ダイアログでそのことを伝える。

## 8. ルーティングと TLS

DNS にワイルドカードの A レコードを 1 本置き、証明書は Caddy がホスト名ごとに自動取得する。

- **DNS**: `*.dev.example.com` を VPS の IP に向ける。アプリは `<slug>.dev.example.com`、管理 UI は `console.dev.example.com`。
- **証明書**: Caddy は設定にホスト名があると自動 HTTPS を有効にし、証明書の取得・更新と HTTP→HTTPS リダイレクトを行う（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)）。
- **発行上限**: Let's Encrypt は登録ドメインあたり 7 日間で 50 枚まで（[Rate Limits](https://letsencrypt.org/docs/rate-limits/)）。3〜5 アプリでは問題ないが、開発中は staging 環境を使う。
- **設定投入**: `koroku` が DB から Caddy の JSON 設定全体を生成し、admin API の `POST /load` で置き換える（[API Tutorial](https://caddyserver.com/docs/api-tutorial)）。差分更新はしない。`koroku` 起動時にも必ず実行し、DB を唯一の正とする。
- **停止中アプリ**: ルートは残し、503 の固定ページを返す。

**Caddy 設定の細則**

- サーバーは `:80` 用と `:443` 用の 2 つに分ける。1 つのサーバーに両方のポートを持たせると、Caddy が自動リダイレクトを挿入する位置がホスト別ルートの後ろになり、`http://<slug>.<base>` へのアクセスが平文のままアプリへ転送されてしまう。この構成では HTTP は 308 で HTTPS へ、未知のホスト名は `:80` で 404、`:443` では TLS ハンドシェイク失敗になる。
- 一度もデプロイしていないアプリも、停止中アプリと同じ 503 の固定ページにする（転送先がまだないため）。
- 設定の投入は、DB から生成した内容が前回投入したものと同じなら省く。これで通常の再デプロイでは Caddy に触れない。`koroku` 起動時だけは必ず投入する（Caddy 側は同一内容なら `config is unchanged` として何もしない）。投入に失敗したら次回必ず投入し直し、5 分ごとの定期処理でも再試行するので、Caddy が後から起動しても追いつく。
- アプリへの `reverse_proxy` には `stream_close_delay: 5m` を付ける。Caddy は設定を入れ替えるとサーバーを作り直し、既定ではその瞬間に、他アプリを含む WebSocket などのアップグレード済み接続を切る。この指定は切断をなくすのではなく先送りするもので、アプリの作成・ドメインやポートの変更・起動や停止のたびに、他アプリの WebSocket は最長 5 分後に 1 回切れる（クライアントの再接続に任せる）。SSE と通常のリクエストは、平文でも TLS + HTTP/2 でも、入れ替えの間に欠けない。この指定には Caddy 2.7 以上が要るので、Ubuntu 標準の apt パッケージではなく Caddy 公式リポジトリのものを入れる（プロビジョニングスクリプトで対応）。
- アクセスログはアプリごとに `<access_log_dir>/<slug>.log`（既定 `/var/log/caddy/apps`）へ出す。`koroku` のデータディレクトリは root 専用（0700）で Caddy のユーザーが書けないため、置き場所を分けた。ロガー名は `http.log.access.<slug>.app` とする。Caddy はロガー名を前方一致で選ぶので、slug そのままだと `shop-staging` のログが `shop.log` にも入ってしまう。管理 UI と未知のホスト名のアクセスログは出さない。
- Caddy の admin API は認証がなく設定全体を書き換えられるので、`caddy.admin_url` はループバックアドレスしか受け付けない。
- `koroku` がデプロイの成功を記録したあとにも、もう一度設定を同期する。ルートの更新（手順 7）はデプロイが「現在のリリース」になる前に走るので、そこだけでは初回デプロイ（と停止中からのデプロイ）のアプリが、5 分ごとの定期処理まで 503 の固定ページのまま残ってしまう。

**ローカル環境の Caddy**

手元でも本番と同じ経路（Caddy → アプリ）で `https://<slug>.dev.localhost` を開けるように、開発用の `compose.yaml` にも Caddy がある。`koroku` 側の生成ロジックは本番と共通で、`config.toml` の 2 項目だけで切り替える。

| 項目 | 内容 |
| --- | --- |
| `[caddy] listen_ip` | `:80` と `:443` を、このアドレスだけで待ち受ける（ローカルは `127.0.0.1`）。空なら全インターフェース（本番） |
| `[caddy] internal_ca` | ACME の代わりに Caddy 内蔵の CA で証明書を出す。`*.localhost` には公的な証明書が出ないため。CA のルート証明書は利用者がブラウザ側の OS に手で入れる（コンテナの中の Caddy にはホストの信頼ストアを変えられないので、`install_trust` は切る） |

- `--insecure-no-auth` は、Caddy が有効でも `listen_ip` がループバックなら起動を許す。このフラグを Caddy と併用させない理由は「Caddy 経由で管理 UI がネットワークに出る」ことで、ループバックだけで待ち受けるならその心配がない。
- 認証なしモードの Host 検査は `*.localhost` を受け付ける（`console.dev.localhost` で開くため）。`.localhost` 配下の名前は常にループバックに解決される（RFC 6761）ので、DNS リバインディングの対策は崩れない。変更系の `Origin` は `http://<Host>` に加えて `https://<Host>` も許す。同じ `dev.localhost` 配下のアプリからの要求は、本番と同じく `Sec-Fetch-Site: same-site` と `Origin` の不一致で拒否する。
- アクセスログは出さない（`access_log_dir = ""`）。Caddy はコンテナ内で root として動き、ログファイルを `koroku`（利用者の UID）が読めないため。

**ワイルドカードのレコードを登録できない DNS サービス**: `console.<base>` とアプリごとの `<slug>.<base>` を個別に登録する。証明書はホスト名ごとに取る設計なので、どちらでも `koroku` 側の実装は変わらない。

ワイルドカード証明書は DNS-01 チャレンジでのみ発行できる（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)）。個別証明書ではサブドメイン名が証明書の公開ログ（Certificate Transparency）に載る。ワイルドカード証明書に切り替えるには、DNS 事業者の API 連携が要る。

### 8.1 独自ドメイン

アプリごとに任意個の独自ドメインを UI から追加できる。自動割当の `<slug>.dev.example.com` は常に残り、独自ドメインはそこに足される別名として扱う。

1. **登録**: UI でホスト名（例 `example.com`、`www.example.com`）を入力する。状態は `pending`。
2. **DNS 設定（利用者が実施）**: サブドメインは `<slug>.dev.example.com` への CNAME、apex は VPS の IP への A レコード。UI に設定すべき値を表示する。
3. **検証**: `koroku` がホスト名を名前解決し、`config.toml` の `public_ip` と一致したら `active` にする。「再検証」ボタンと 5 分間隔の自動再試行を持つ。
4. **反映**: `active` のホスト名だけを Caddy 設定に含めて `POST /load` する。証明書は自動割当ドメインと同じく Caddy がホスト名ごとに取得する。

- 検証前のホスト名を Caddy に渡さないのは、DNS が向いていない状態で証明書発行を試み続けるのを避けるため。
- 検証は「名前解決で得た全アドレス（A・AAAA とも）が `public_ip` と一致すること」を条件にする。正しい A レコードに加えて別ホストを指す AAAA が残っていると、一部の通信や証明書の検証がそちらへ向かうため。
- 5 分間隔の自動再試行は `pending` のものだけを対象にする。`active` になったドメインは、後で DNS が変わっても自動では `pending` に戻さない（一時的な名前解決の失敗で本番ドメインを止めないため）。「再検証」ボタンを押したときは結果どおりに更新する。
- プライマリにできるのは `active` のドメインだけ。プライマリのドメインが `pending` に戻ったら、プライマリ指定も外れる（リダイレクト先が応答しなくなるのを避けるため）。
- 追加した直後に 1 回検証するので、DNS を先に設定してあれば待たずに `active` になる。
- CDN を前段に置くなど IP が一致しない構成のために、検証を飛ばす「強制有効化」を用意する。
- アプリごとに「プライマリドメイン」を 1 つ選べる。選んだ場合、他のホスト名へのアクセスはプライマリへ 308 リダイレクトする（`www` ありなしの統一用）。
- ホスト名は全アプリで一意。`*.dev.example.com` 配下は独自ドメインとして登録できない。

**データと API**

| 種別 | 内容 |
| --- | --- |
| テーブル `app_domains` | `id`, `app_id`, `hostname`（一意）, `status`（pending / active）, `is_primary`, `force_active`, `last_checked_at`, `last_error` |
| `GET / POST /api/apps/{slug}/domains` | 一覧、追加 |
| `POST /api/apps/{slug}/domains/{id}/verify` | 再検証 |
| `PATCH / DELETE /api/apps/{slug}/domains/{id}` | プライマリ指定・強制有効化、削除 |
| UI | アプリ詳細に「ドメイン」タブ。状態、設定すべき DNS レコード、直近のエラーを表示 |

### 8.2 staging と本番の分け方

staging と本番は同じ VPS に置き、「環境ごとに別アプリ」として分ける。環境という概念を基盤に足さず、同じリポジトリを複数のアプリに紐づけられるようにするだけで済む。

|  | staging アプリ | 本番アプリ |
| --- | --- | --- |
| slug の例 | `myapp-staging` | `myapp` |
| URL | `myapp-staging.dev.example.com` のみ | 独自ドメイン（プライマリ）+ `myapp.dev.example.com` |
| デプロイ元 | 任意のブランチ | `main` のコミット |
| 環境変数・volume | アプリ単位で独立 | アプリ単位で独立 |

- `apps` の `environment`（production / staging）は表示用で、一覧ではラベルになる。本番アプリのデプロイと削除には確認ダイアログを出す。
- 同居の弱点は、staging のビルド負荷やディスク枯渇が本番に波及すること。ビルドの同時実行 1、サービスごとのメモリ上限（override で既定値を付与、アプリ設定で変更可）、ディスク使用率 85% 超でデプロイ拒否、の 3 つで抑える。
- より強い分離が要るようになったら、同じプロビジョニングスクリプトで 2 台目を立てて `koroku` をもう 1 組動かす。マルチノード対応はしない。

**本番利用で残る制約**: デプロイ時にコンテナ再作成による数秒の断がある。VPS 1 台なのでホスト障害時は全アプリが止まる。デプロイ時の断はデプロイ対象のアプリに限られる（7 章）。ホスト障害時の全停止は許容する。

## 9. データモデル・API・UI

データは SQLite の 1 ファイル（`koroku.db`）にある。マイグレーションは埋め込みの SQL を起動時に適用する。サービス名を含む値（サービスごとのメモリ上限、公開するサービス、接続の利用側のサービス）は JSON の列に持つ。行と一緒に読み書きするだけで、サービス名で検索することがないため。

| テーブル | 内容 |
| --- | --- |
| `apps` | slug、リポジトリ（`repo_id`、`repo_full_name` か `repo_url`）、Compose ファイルのパス、公開サービスとポート、割当ホストポート、ヘルスチェックパス、`environment`、`desired_state`、`relax_policy`、メモリ上限、稼働中のデプロイ、バックアップの設定（除外 volume、ダンプフック、デプロイ前取得）、必要な変数の読み方（`env_source`、`env_example_path`、9.4）、所属するグループと公開するサービス（6.1） |
| `env_vars` | 環境変数。値は暗号化して保存（`app_id` + `key` で一意） |
| `deployments` | ref、コミット、状態、エラー、時刻、環境変数の指紋（9.2） |
| `app_domains` | 独自ドメイン（8.1） |
| `backup_runs` | バックアップの実行記録（10.2） |
| `sessions` | セッション（Cookie のハッシュ、表示用の ID、最終利用、IP、User-Agent。10.3） |
| `login_events` | ログイン履歴（10.3） |
| `app_groups` | アプリ間の接続のグループ（名前、`environment`。6.1） |
| `app_links` | アプリ間の接続（利用側アプリ、名前、相手のアプリとサービス、利用側のサービス名の JSON 配列。6.1） |
| `settings` | ホスト単位の値（接続のネットワーク名に使う設置 ID。6.1） |

API はすべて JSON の REST で、ログとデプロイの進行は SSE で配信する。`/auth/*` を除き、セッション Cookie が要る。エンドポイントの一覧と形は [API.md](API.md) が正。

### 9.1 管理 UI の構成と API

管理 UI は「上部バー + アプリ一覧（ホストの状態を常時表示）+ アプリ詳細（ヘッダーと横タブ: 概要 / デプロイ / ログ / 環境変数 / ドメイン / バックアップ / 設定）+ システム + セキュリティ」で構成する。画面の要素と、それを支える API の値は次のとおり。

| 画面の要素 | 必要な情報 | API |
| --- | --- | --- |
| アプリ一覧のホスト状態行、警告バナー | ログ退避の最終成功時刻、ディスク警告のしきい値、Caddy への設定投入の状態 | `GET /api/system` の `disk_limit_percent`、`log_archive`（`configured`、`last_success_at`、`last_error`、`next_run_at`）、`caddy`（`enabled`、`last_loaded_at`、`last_error`）、`swap` |
| アプリ一覧の行、詳細ヘッダー | プライマリ URL、検証待ちドメインの有無 | アプリの JSON の `primary_url`（プライマリドメインがあればその URL、なければ自動割当の URL）と `pending_domains`（検証待ちの数）。一覧のためにアプリごとにドメイン API を呼ばずに済む |
| 概要タブのサービス一覧 | メモリ使用量と上限 | `GET /api/apps/{slug}/services` の各サービスの `memory_bytes` と `memory_limit_bytes`（`docker stats --no-stream` の値。取れなければ `null`） |
| デプロイタブの手順表示 | 手順の区切りと、手順ごとの所要時間 | デプロイログの手順の見出し行（`==> <RFC 3339 の時刻> <手順のキー> <表題>`）。UI はこの行でログを手順に分け、次の見出し（または終了）までを所要時間とする。手順のキーは `fetch` `prepare` `validate` `build` `pull` `backup` `start` `route` `check` `done` `cleanup` と、失敗時の `failed` `rollback`。DB に手順の表は持たない（ログファイルが唯一の記録で、SSE の再接続とも整合する）。自動ロールバックの結果も同じ理由でログの行から読み取り、デプロイの JSON にフィールドは持たない（UI が解釈する行は `docs/API.md`） |
| バックアップタブ | 次回の実行予定、スナップショットのサイズ | `GET /api/backups` の `next_run_at`、スナップショットの `size_bytes`（restic が記録していれば。なければ `null`）、`GET /api/apps/{slug}/volumes`（下記） |
| システム画面 | Docker のディスク内訳 | `GET /api/system/docker`（`docker system df` の結果: イメージ、コンテナ、volume、ビルドキャッシュそれぞれの件数と合計と、「不要なデータを削除」で空く量）。数秒かかりうるので `/api/system` とは分ける |

- `swap`（`total_bytes`、`used_bytes`。swap がない、または読めなければ `null`）は `/proc/meminfo` の `SwapTotal` と `SwapFree` から求める。ビルド時のピークに備えて swap を 2GB 置くので（10 章）、メモリと並べて使われ具合を見せる。
- `log_archive.next_run_at` は次にログ退避が走る時刻（未設定なら `null`）。退避の間隔は設定値で UI からは分からないので、バックアップの `next_run_at` と対にしてシステム画面の「次回」に出す。
- `GET /api/apps/{slug}/volumes` は、そのアプリの named volume の一覧（名前、バックアップから外しているか、Docker に実在するか）。バックアップタブで、一度もスナップショットを取っていない volume（DB のデータディレクトリなど、最初のバックアップより前に外したいもの）も選べるようにするため。
  - 「アプリの volume」の定義はバックアップと同じにする（10.2 の細則）。バックアップが使う列挙の処理（`internal/backup`）をそのまま呼び、別の定義を作らない。
  - 列挙は `docker volume ls` と `docker volume inspect` だけで、restic もオブジェクトストレージも使わない。バックアップが未設定のホストでも応答する。
  - 一度もデプロイしていないアプリは volume がまだないので、空の一覧を返す（エラーにしない）。
  - 除外リストにあるが Docker に実在しない名前（先に外しておいた、あるいは volume を消した）も `exists: false` として一覧に含める。UI に出ないと対象へ戻せないため。
  - volume の大きさは返さない。求めるにはディレクトリを歩く（`docker system df -v` も同じ）必要があり、タブを開くたびに走らせるには重い。スナップショットの `size_bytes` が目安になる。

バックアップの API（一覧、実行、リストア、ダンプのダウンロード）は 10.2 の内容で、形は `docs/API.md` にある。環境変数の画面の「再デプロイ」はデプロイの `POST` を呼ぶだけで、専用の API はない（保存とデプロイは分ける。9.2）。状態の自動更新はポーリング（一覧と詳細は数秒間隔、デプロイ中のログは SSE）で行い、状態変化を配信する専用の SSE は作らない（アプリ 3〜5 個の規模では不要）。

### 9.2 環境変数を 1 件ずつ確定する

環境変数は行ごとに編集して確定（保存）する。全件を一括置換する形だと、1 件だけ直したいときにも画面全体の「未保存の変更」を抱え、別のタブで開いた古い一覧で保存すると他の行の変更を消してしまうため。

- API は `PUT /api/apps/{slug}/env/{key}`（追加・変更、本文の `key` で名前の変更）と `DELETE /api/apps/{slug}/env/{key}`。名前の変更は 1 つのトランザクションで行い、「削除して追加」の途中の状態を作らない。一括置換の `PUT /api/apps/{slug}/env` は「.env 形式で編集」に使う。
- 保存はデプロイを伴わない。行ごとに「保存して再デプロイ」を付けると、3 件直せば 3 回デプロイが走るため。代わりに、DB の値と稼働中のリリースの値の差を「未反映の変更」として画面に出し、再デプロイのボタンはそこに 1 つだけ置く。
- 差を知るため、デプロイの手順 3 で env ファイルを作るときに、各変数の指紋（キーと値の HMAC-SHA256 を 16 バイトに切ったもの）を `deployments.env_fingerprints` に JSON で記録する。`GET /api/apps/{slug}/env` は、`current_deployment_id` のデプロイの指紋と今の DB を比べて、変数ごとの `pending`（`added` / `changed`）と、削除したがまだ稼働中の `removed` を返す。
  - 値そのものや素のハッシュは記録しない。短い値は素のハッシュから総当たりで戻せるため。HMAC の鍵はマスターキーから導出する（`HMAC(master_key, "koroku env fingerprint v1")`）。マスターキーを差し替えると比べられなくなるが、それは全件「変更」と出るだけで、再デプロイすれば揃う。
  - 稼働中のリリースがない（未デプロイ）か、指紋を記録していないデプロイが稼働中のときは比べられないので、`compared: false` を返し、UI は差を出さない。
  - 指紋は値を含まないので、再ログインから 10 分を過ぎて値を隠しているときも差は返す。
- 失うもの: 関連する変数（接続先とパスワードなど）を同時に変えられない。1 件目を保存した時点で、半端な組み合わせが DB に残る。デプロイは手動でしか始まらないので、その間に誰かがデプロイしない限り稼働中のアプリには影響しない。まとめて変えたいときは「.env 形式で編集」（一括置換）を使う。

### 9.3 コンテナのリソースの内訳

システム画面は、ホスト全体の使用量に加えて、稼働中のコンテナのメモリと CPU をアプリごとにまとめて出す。アプリ 3〜5 個を 4GB のホストに詰める構成では、どのアプリが使っているかが一番知りたいため。

- API は `GET /api/system/containers`。`docker stats --no-stream` は CPU 使用率を 2 回の計測の差から出すので、数秒かかる。`/api/system` に混ぜると一覧画面のホスト状態行まで遅くなるため、`/api/system/docker` と同じく分ける。
- 取り方は 3 つのコマンド。`docker stats --no-stream --no-trunc --format json`（稼働中の全コンテナ）と `docker info`（CPU のコア数と、Docker から見たメモリの総量）を並行に走らせ、終わってから stats に出たコンテナを `docker container inspect` で引く。inspect からは Compose のラベル（`com.docker.compose.project`、`com.docker.compose.service`）とメモリ上限（`HostConfig.Memory`）を取る。stats の出力にはラベルがなく、上限は「なし」でもホストのメモリ量が出て区別できないため。
- アプリへの振り分けは、プロジェクト名が DB にあるアプリの `koroku-<slug>` と一致するかで決める。それ以外（koroku の外で動かしているコンテナ、Compose 以外のコンテナ、手元の開発環境の `koroku-dev` のように `koroku-` で始まっても DB にないもの）は 1 つの「その他」にまとめる。合計が Docker 全体の使用量と一致するように、捨てずに出す。
- 稼働中のコンテナだけを出す（停止中のコンテナは stats に出ず、何も使っていない）。アプリのコンテナが 1 つも動いていなければ、そのアプリの行は出さない。
- **CPU は `docker stats` と同じく 1 コアを 100% とする値で返す**（`CPUPerc` そのもの）。2 コアのホストでは 200% まで振れる。`docker stats` や `top` で見慣れた読み方と同じ数字が出るほうが、手元で確かめた値と突き合わせやすいため。コア数は応答に含め、UI は「このホストは N コア、最大 N×100%」と書き添え、メーターは N×100% を満杯として描く（メモリやディスクと同じく 0〜100% の長さで比べられるように）。値は stats の 2 回の計測の間（約 1 秒）の瞬間値で、平均ではない。
- メモリの使用量は stats の値（cgroup の使用量からページキャッシュの非アクティブ分を除いたもの。`docker stats` の表示と同じ）。割合の分母は `docker info` のメモリ総量（VPS ではホストの `MemTotal` と同じ）。上限は `HostConfig.Memory` で、0（上限なし）は `null`。アプリの上限の合計は、そのアプリの稼働中のコンテナすべてに上限があるときだけ出す（koroku は override で全サービスに付けるので、普通は出る）。
- 競合: stats のあとで inspect する間にコンテナが消えると、inspect は `No such container` で失敗するが、残りのコンテナの出力は出す。その場合は出力を使い、消えたコンテナは除く（消えたものは何も使っていない）。stats と inspect が Docker デーモンのエラーで失敗したときは、`docker system df` と同じく数回やり直す。
- 履歴は持たない。画面を開いている間、ホストの状態と同じ 30 秒間隔で取り直す。

### 9.4 リポジトリが必要とする環境変数との照合

リポジトリの側に「必要な変数の一覧」を書いてもらい、環境変数の画面とデプロイの前に、設定済みの変数と照らし合わせて見せる。アプリのコードが新しい環境変数を使い始めたときの設定漏れに、デプロイする前に気づけるようにするため。

**一覧の取り方（アプリ設定で選ぶ）**

| `env_source` | 読むもの | 必須と任意 | 説明 |
| --- | --- | --- | --- |
| `compose`（既定） | Compose ファイル中の `${VAR}` / `$VAR` の参照（`docker compose config --variables` 相当） | `${VAR:?msg}`・`${VAR?msg}` と、既定値のない `${VAR}`・`$VAR` は必須。`${VAR:-x}`・`${VAR-x}`・`${VAR:+x}`・`${VAR+x}` は任意 | `:?` / `?` のメッセージ |
| `dotenv` | リポジトリの `.env.example`（パスは `env_example_path` で変えられる） | 区別しない（すべて必須） | キーの直前に続くコメント行 |
| `off` | 読まない | — | — |

- **既定が `compose` である理由**: `koroku` は `--env-file` で補間に使う変数を渡しており（6 章）、`${VAR}` の参照はリポジトリに必ずある Compose ファイルに書ける。`.env.example` を置いていないリポジトリで「ファイルがない」と出続けることもない。`${VAR}` を 1 つも書いていなければ「必要な変数はない」と出るだけで害がない。利用者向けの Skill（`plugins/koroku/skills/prepare-app`）も、必須の変数を `${VAR:?説明}`、任意の変数を `${VAR:-既定値}` で Compose に書く形を勧める。
- **`compose` の判定は `docker compose config --variables` と少し変えている**: Compose の `Required` は `?` の形だけで、`${VAR}` は任意と扱う。しかし未設定の `${VAR}` は空文字に置き換わって起動するので、ほぼ確実に設定漏れである。そこで既定値のない参照も必須として出し、空でよい変数は `${VAR:-}` と書けば任意になる。既定値の中に入れ子になった参照（`${A:-${B}}` の `B`）は、外側が未設定のときだけ評価されるので任意とする。
- **`dotenv` に必須と任意の区別がない理由**: `.env.example` には区別を書く決まった方法がない。コメントの書き方で推し量ると誤判定するので、すべて必須として扱い、区別が欲しければ `compose` を使うよう画面と Skill で案内する。

**Compose の参照の読み取り**

- `docker compose config --variables` を呼ばず、Go で Compose ファイルの文字列を走査する。呼ぶにはファイルを一時ディレクトリに書き出す必要があり、`include` や `extends` の参照先が揃わないとエラーで何も得られない。走査なら Docker のない開発環境とテストでも同じに動き、`:?` のメッセージ（Compose の出力には含まれない）を説明として出せる。YAML パーサーの依存も増やさない（5 章の Compose ファイル検出と同じ方針）。
- 扱う構文は Compose の補間と同じ: `$$` はエスケープ、`${VAR}`、`$VAR`、`${VAR:-x}` `${VAR-x}` `${VAR:?x}` `${VAR?x}` `${VAR:+x}` `${VAR+x}`、既定値の中の入れ子。YAML のコメント（行頭か空白の後の `#`、引用符の外）の中は読まない。ブロックスカラー（`|`、`>`）の中の `#` はコメントではないので読む。同じ変数が何度も出たら、どこか 1 か所でも必須なら必須。
- 結合テストで、`testdata` の Compose ファイルについて変数名・`?` の有無・既定値が `docker compose config --variables --format json` と一致することを確かめる。
- **読まないもの**（画面にも「Compose ファイルの `${VAR}` だけを見る」と書く）:
  - `include:`、`extends: { file: }` で参照する別ファイル。読むのは `compose_path`（未指定なら自動検出した 1 ファイル）だけ。
  - `environment: [VAR]` のような値なしの受け渡し、リポジトリ内の `env_file:`、アプリのコードが直接読む変数。`koroku` は env ファイルで全変数を全サービスに注入するので、コードだけが読む変数は Compose に現れない。必要なら Compose に `${VAR:?...}` として書いてもらう。
  - 補間の中の不正な構文は、その参照を読み飛ばす（デプロイ時に `config` が失敗して分かる）。

**照合の結果**

- 照合するのは DB に保存済みの変数（稼働中のリリースではない）。「次にデプロイしたら足りるか」を知りたいため。値は使わず、キーの有無だけを見る。
- 「設定してあるが一覧にない変数」も返す。`dotenv` では「リポジトリから消えた変数」として行に印を付ける。`compose` では、アプリのコードが直接読む変数（Compose に現れない）が正常に多数あるので、画面には出さない。
- 環境変数の画面は、アプリの `default_branch` の最新コミットと照合する。デプロイの画面は、選んだブランチかコミットと照合し、足りない必須の変数があれば選択欄の下と、本番アプリの確認ダイアログに警告を出す。**デプロイは止めない**。`${VAR:?}` の形なら手順 4 の `config` が失敗して旧版が動き続けるし、意図して未設定のまま出すこともあるため。
- 環境変数の画面には、足りない変数の行に「追加」ボタンを置き、押すとキーを入れた新しい行の編集を開く（9.2 の 1 件ずつの保存と同じ流れ）。

**取得**

- GitHub の contents API（installation token）で、ref のコミットのファイルを読む。clone はしない。ブランチ名はまずデプロイと同じ方法でコミット SHA に解決し、Compose ファイルのパスの自動検出（ルートの一覧）と本体の読み取りを SHA で行う。
- 結果は（リポジトリ、SHA、方式、パス）をキーに、プロセス内に 64 件までキャッシュする。SHA の中身は変わらないので期限は付けない。環境変数を 1 件保存するたびに画面が照合し直しても、GitHub への問い合わせは SHA の解決 1 回で済む。キャッシュするのはキーと説明だけで、値は持たない。「ファイルがない」はそのコミットでは変わらないのでキャッシュし、ほかの失敗はキャッシュしない。
- GitHub への問い合わせは全体で 15 秒で打ち切る。
- GitHub App で読めないリポジトリは、認証なしの contents API で読む。対象は、App がインストールされていないリポジトリ、App そのものが未設定のホスト、`repo_url` が `https://github.com/<owner>/<repo>`（`.git` 付きも可）のアプリ。
  - 制限は送信元 IP ごとに 1 時間 60 回で、koroku の画面からの照合だけがこれを使う。1 コミットにつき 2 回（ルートの一覧とファイル本体）で、結果はコミットごとにキャッシュするので、新しいコミットを 1 時間に 30 個まで照合できる。ブランチからコミットへの解決は `git ls-remote`（7 章のデプロイと同じ）で、この回数に入らない。
  - 超えたら `rate_limited` を返し、画面は照合なしで使える。デプロイは git で取得するので影響しない。
  - 認証なしで 404 なら、private か存在しないリポジトリなので `repo_unavailable` にする。
- 読めなかったときもエラーにせず、200 で `status: "unavailable"` と理由を返し、画面は照合なしで通常どおり使えるようにする。理由は `file_not_found`（ファイルがない）、`repo_unavailable`（GitHub App からも認証なしでも読めない。private リポジトリに App がインストールされていないなど）、`unsupported_repo`（`repo_url` が github.com 以外。GitHub の API では読めない）、`ref_not_found`、`too_large`、`rate_limited`、`github_error`。`not_configured` は返さない（型にだけ残っている）。
- デプロイの手順 2 で取得したチェックアウトからは読まない。デプロイ前に知りたいという目的に合わないため。

## 10. セキュリティと運用

`koroku` は Docker ソケットを操作できるためホストの root 相当であり、管理 UI の認証がこのシステムの唯一の関門になる。

**セキュリティ**

- **信頼モデル**: デプロイするのは自分のリポジトリだけ。他人のコードを動かす前提の隔離（gVisor 等）は持たない。アプリ同士は互いに見えないが、画面で作った接続の両端のサービス同士は、双方向に全ポートで届く（6.1）。
- **認証**: GitHub ログイン後、ユーザー ID が `config.toml` の許可 ID と一致した場合のみセッションを発行する。Cookie は `HttpOnly`、`Secure`、`SameSite=Lax`。変更系 API は `Origin` ヘッダーを検証する。GitHub の 2 段階認証の必須化、危険な操作の前の再ログイン、セッションの一覧とログイン履歴は 10.3。
- **待受**: `koroku` と Caddy admin API は 127.0.0.1 のみ。外部公開は 22、80、443。
- **前提と限界**: ポリシー検査は「自分のリポジトリを誤って危険な構成でデプロイしない」ための検査で、悪意あるリポジトリを安全に動かす隔離ではない。「検査を緩める」フラグを立てたアプリには、上の保護の多くが効かない（フラグは 1 つで、項目ごとには選べない）。Caddy の admin API は認証なしの TCP（ループバック）なので、ホストのネットワークに入れたコンテナからは設定を書き換えられる。ログのマスクは環境変数の値そのものとの一致だけで、3 文字未満の値や、base64 などに変換された値は隠せない。
- **ポート公開の抑止**: Docker が publish したポートの通信は ufw の設定を通らずに届く（[Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)）。ufw に頼らず、6 章の override で全サービスの `ports` を消すか 127.0.0.1 に限定する。
- **シークレット**: 環境変数の値は AES-256-GCM で暗号化して SQLite に保存する。マスターキーは `/etc/koroku/master.key`。UI は既定でマスク表示。

**管理 UI の保護の細則**

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
- **ディスク**: デプロイ成功後に `docker image prune -f`、週 1 回 `docker builder prune --all --filter until=168h`。使用率はシステム画面に出す。
- **メモリ**: ビルド時のピークに備えて swap を 2GB 置く。VPS は 2 vCPU、4GB RAM、40GB ディスクを出発点にする（未計測の目安）。
- **バックアップ**: 基盤の設定とアプリのデータを日次でオブジェクトストレージに退避する（10.2）。
- **koroku の更新**: バイナリを差し替えて `systemctl restart koroku`。アプリのコンテナは影響を受けない。
- **起動制御と削除**
  - 停止は `docker compose stop`、起動は稼働中リリースのファイルで `up -d --wait`（設定を確実に揃えるため。リリースのファイルが残っていなければ `docker compose start`）、再起動は `docker compose restart`。停止・起動のたびに Caddy 設定を再投入し、停止中は 503 の固定ページになる。
  - デプロイと手動操作はアプリ単位で直列化する。デプロイが `queued` か `running` のアプリへの操作は 409（`busy`）で断り、操作の実行中に始まったデプロイは操作の完了を待つ。同じアプリに対して `docker compose` を同時に走らせないため。
  - 削除は `docker compose down`（`?volumes=true` なら `--volumes` に加え、プロジェクトのラベルが付いた volume を明示的に消す）→ リリース用イメージの削除 → DB の行とアプリのディレクトリの削除 → Caddy 設定の再投入、の順。`down` に失敗したら DB には触れない。volume を残して削除したあとに同じ slug でアプリを作り直すと、プロジェクト名が同じなので残した volume がそのまま使われる。
  - `docker builder prune --all --filter until=168h` は毎日実行する。対象は「1 週間使われていないビルドキャッシュ」なので効果は週 1 回と同じで、`koroku` を再起動しても実行の機会を逃さない。
  - `--all` を付ける。付けないと、Docker に組み込みの BuildKit は、層がイメージと共有されているキャッシュの記録を消さない。キャッシュは手前の記録から順にしか消えないので、ビルドの連なりの末端がたいていイメージの層である以上、その手前の古いキャッシュも残る。`--all` でもイメージが使う層は消えない（キャッシュの記録が外れるだけ）。
  - システム画面の「削除で空く」量は、`docker system df` の回収可能（Reclaimable）ではなく、koroku の削除が実際に消す分を koroku が見積もる。Docker の値は、タグ付きでも使われていないイメージ（ロールバック用に残した直近のリリース、停止中のアプリのイメージ）と、まだ 1 週間たっていないキャッシュを含み、ボタンを押しても減らない量になるため。`docker system df -v` の明細から、どのコンテナにも使われていないタグのないイメージの固有サイズと、自身もその先も 1 週間使われておらず使用中でもない、イメージと共有していないキャッシュの合計を求める。volume とコンテナは消さないので 0 とする。
  - Compose がビルドしたイメージ（`com.docker.compose.project` のラベルが付く）は、タグがなくても見込みに入れない。層がビルドキャッシュにも残っているので、イメージを消しても空かない。どの層がどのキャッシュと重なるかは `docker system df` から分からないので、見込みは少なめに倒す。押したあとは、Docker が空けたと報告した量（`Total reclaimed space` と `Total`）の合計を画面に出し、実際の結果を示す。
  - Caddy 設定は 5 分ごとに、内容が変わっていなくても投入し直す。Caddy が単独で再起動すると投入済みの設定を忘れるため。同一内容の投入は Caddy が `config is unchanged` として何もしないので負荷にならない。
- **プロビジョニング**: 新規 VPS を 1 本のスクリプト（`deploy/provision.sh`）で構築する。Docker、Caddy、restic、`koroku` の unit、swap、Docker のログ設定、ufw、SSH の鍵認証化（10.4）。手順は `docs/OPERATIONS.md`。

### 10.1 ログ収集

ログは `koroku` 内蔵のコレクタがファイルに蓄積し、4 時間ごとにオブジェクトストレージに退避する。Loki などのログ基盤は、常駐メモリが増えるので 3〜5 アプリの規模では採用しない。

| 項目 | 仕様 |
| --- | --- |
| 対象 | 全サービスの stdout / stderr、Caddy のアクセスログ（アプリ別）、デプロイログ |
| 収集 | アプリごとに `docker compose logs --follow --timestamps --no-color` を読み続け、`/var/lib/koroku/logs/<slug>/<service>/YYYY-MM-DD.log` に追記 |
| 再開 | `koroku` 再起動時は最後に書いた時刻を `--since` に渡す。直近分は Docker 側に残っているので短い停止は埋まる |
| ローカル保持 | 14 日。日付が変わったファイルは gzip 圧縮 |
| オブジェクトストレージへの退避 | 4 時間ごと（00:10 JST 起点）に、前回以降の差分を gzip して `logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz` へアップロード |
| オブジェクトストレージ側の保持 | プレフィックス `logs/` に 90 日で削除するライフサイクルルールを設定（[S3: Lifecycle configuration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html) と同等の機能。koroku からは設定しない） |
| 閲覧 | ライブ tail（SSE）。過去 14 日は日付・サービス・キーワードで絞り込み。それ以前は presigned URL でダウンロード（[S3: Presigned URLs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html)） |

- API: `GET /api/apps/{slug}/logs?service=&from=&to=&q=`、`GET /api/apps/{slug}/logs/archives`、`GET /api/apps/{slug}/logs/archives/url?key=`。

**ログ収集の細則**

- ローカルのファイルは 1 行が `<UTC の時刻（ナノ秒まで）> <本文>`。日付の区切りと退避の時間枠は `config.toml` の `timezone`（既定 `Asia/Tokyo`）で決める。
- サービス名は `docker compose logs` の行頭（コンテナ名からプロジェクト名を除いたもの）から末尾の `-<番号>` を落として求める。`container_name` を独自に指定したサービスはその名前で記録される（Compose がそう出力するため）。
- 再開時は、サービスごとに最後に書いた時刻の 30 秒前から `--since` で読み直し、「サービス・時刻・本文」が一致する行を数えて重複を落とす。同じ時刻の別の行は残す。Docker 側に行が残っている限り欠落しない。
- 1 回の `logs --follow` の中で、同じコンテナの「時刻・本文」が一致する行がもう一度出たら捨てる（コンテナごとに直近 4096 行を覚えておく）。`docker compose logs --follow` は、コンテナの起動と重なって開始されると同じコンテナに二重に接続し、以後その行をすべて時刻まで同じ内容で 2 回ずつ出すことがある。停止中のアプリを待っているフォロワーはデプロイのたびにこの状況になるので、対処しないと保存されるログとオブジェクトストレージへの退避が倍になる。Docker は行ごとにナノ秒の時刻を付けるので、同じコンテナで時刻も本文も同じ行はアプリが 2 回出力したものではない。別レプリカ（`web-1` と `web-2`）の同じ行は残す。
- 1 行は 16 KiB で切り、不正な UTF-8 と制御文字は置き換える。ログの本文は `koroku` 自身のログやエラーメッセージに出さない（アプリの出力に何が入るか分からないため）。
- Caddy のアクセスログは、同じアプリの擬似サービス `_access` として取り込む。`koroku` が止まっている間にローテートされて消えた分は読めない。
- 退避は「日ごとのファイルのどこまで送ったか」をバイト位置で記録し、送る範囲を保存してからアップロードする。失敗や再起動のあとは同じ範囲を同じキーへ送り直すので、重複も欠落も起きない。失敗した時間枠は次回（および 10 分後の再試行）にその時間枠のキーで送る。空の時間枠は何も送らない。
- デプロイログは、30 分間更新がなければ完了とみなして `logs/<slug>/_deploy/<id>.log.gz` に 1 回だけ退避する。
- オブジェクトストレージが未設定でもログの収集・検索・ライブ表示は動く。退避と、退避済みログの一覧・ダウンロードだけが無効になる。
- 過去ログの検索は `/logs/history` に分けた（`docs/API.md`）。SSE と JSON を同じパスで出し分けると扱いにくいため。
- 制約: 直近の区間は VPS 上にしかない。ホストを失うと最大 4 時間分のログが消える。間隔は設定値（既定 4 時間、最短 1 時間）。1 回の処理はサービスごとに差分テキストの圧縮と 1 オブジェクトのアップロードだけで、常駐負荷は増えない。

### 10.2 バックアップ

バックアップは restic で S3 互換のオブジェクトストレージに取る。restic は S3 互換ストレージをリポジトリにできる（[restic documentation](https://restic.readthedocs.io/)）。重複排除・暗号化・世代管理を自前で書かずに済む。

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
- **ストレージの選択**: S3 互換 API（署名 V4、パス形式の URL）だけを使い、事業者固有の API は使わない。事業者の切り替えは `config.toml` の endpoint・region・バケット名・アクセスキーを変えるだけで済む。

**リストア**

1. volume: UI でスナップショットを選ぶ → アプリ停止 → 現在の中身を `/var/lib/koroku/restore-trash/` に退避（7 日保持）→ `restic restore` → アプリ起動。
2. DB ダンプ: UI からダウンロードするところまでを提供する。流し込みは手動（手順を画面に表示）。
3. ホスト全損: 新しい VPS をプロビジョニング → `koroku restore-system` で `koroku.db` と `/etc/koroku` を戻す → 各アプリを再デプロイして volume をリストア → DNS を新 IP に向ける。

**事前準備と秘密情報**

- オブジェクトストレージにバケットを作り、そのバケットに限定したアクセスキー（Access Key ID / Secret Access Key）を発行して `/etc/koroku/config.toml` に置く。
- restic のリポジトリパスワードは `/etc/koroku/restic.key`。これとオブジェクトストレージの認証情報は VPS の外（パスワードマネージャなど）にも必ず控える。失うとホスト全損時に復元できない。

**バックアップの細則**

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
- 基盤のバックアップで設定ディレクトリをコピーするとき、その下にあるデータディレクトリ（`data_dir`）と、コピー先の作業ディレクトリ自身は除外する。ローカル環境では両方を `data/dev` の下に置くので、除外しないと作業ディレクトリを何重にもコピーし続けてしまう。本番は `/etc/koroku` と `/var/lib/koroku` が別なので関係しない。
- volume のリストアで中身を退避する移動は `rename` で行い、`EXDEV`（マウントをまたぐ移動）で失敗したときだけ、所有者・パーミッション・時刻を保ったコピーと元の削除に切り替える。事前に「同じファイルシステムか」は確かめるが、同じファイルシステムでも別の bind mount を経由すると Linux は `rename` を拒むため（ローカル環境は、`koroku` のコンテナにデータディレクトリと Docker の volume ディレクトリを別々にマウントしている）。コピーではハードリンクと拡張属性は保たれないが、退避は手作業での救出用なので許容する。ソケット・デバイス・名前付きパイプはコピーせず、リストアを失敗させて元に戻す。本番は同じマウントなので常に `rename` になる。

### 10.3 ログインの強化

許可ユーザー以外はログインできない（5 章）。そのうえで、「許可ユーザーの GitHub アカウントか、koroku のセッション Cookie が盗まれた場合」の被害を小さくし、盗まれたことに気づけるように、次の 3 つを重ねる。管理 UI をネットワークの段階で閉じる仕組み（Tailscale、クライアント証明書など）は持たない。

**GitHub の 2 段階認証を必須にする**

- ログイン時の `GET /user` の応答の `two_factor_authentication` が `true` のときだけセッションを発行する。`false` なら `two_factor_required` で拒否する。
- この項目は GitHub のドキュメントでは「private user の応答」にだけあり、GitHub App のユーザーアクセストークンで返るかは書かれていない。項目がない応答は `two_factor_unknown` で拒否する（確かめられないものを通さない）。返らない環境で締め出されないよう、`[github] require_two_factor = false` で検査を外せる。既定は `true`。外したときは起動時に警告をログに出す。
- 拒否したログインも、GitHub のユーザー ID が分かっているのでログイン履歴に残す（下記）。

**危険な操作の前に再ログインを求める（step-up）**

- セッションの発行（ログイン）から 10 分以内でなければ、次の操作を `403 reauth_required` で断る。UI は再ログインの案内を出し、GitHub のログインを経て元の画面に戻す。
  - アプリの削除
  - 本番アプリ（`environment = production`）のデプロイ
  - 環境変数の保存（一括置換も 1 件ずつの追加・変更・削除も）と、値の表示（下記）
  - volume のリストアと、DB ダンプのダウンロード
  - アプリ設定のうち、`environment`、`relax_policy`、リポジトリ（`repo_full_name`、`repo_url`、`repo_id`）の変更。本番の確認ダイアログを外す、ポリシー検査を外す、別のコードを動かす、のいずれかにつながるため
- 環境変数の `GET` は、10 分を過ぎていても断らず、値を `null` にして `values_hidden: true` を返す。キーの一覧は見られるようにして、タブそのものは使えるようにするため。
- 再ログインは通常のログインと同じ流れで、新しいセッションを発行する。同じブラウザーの古いセッションはそのとき消す。戻り先は `GET /auth/github/login?return_to=<パス>` で渡し、state と同じく短命の Cookie に入れる。`/` で始まり `//` や `\` を含まないパスだけを受け付ける（外部サイトへのリダイレクトに使われないように）。
- 限界: GitHub にログインしたままのブラウザーでは、再ログインはパスワードを聞かれずに終わる。防げるのは「Cookie だけが盗まれて別の場所で使われる」場合で、本人のブラウザーそのものを使われる場合は防げない。GitHub の OAuth には OpenID Connect の `max_age` に当たるものがない。
- 再ログインを求めない操作: 作成、staging のデプロイ、起動制御、ドメイン、バックアップの実行、prune。どれも取り消せるか、データを外へ出さないため。

**セッションの一覧と失効、ログイン履歴**

- `sessions` は `id`（画面と API で使う識別子。乱数 8 バイトの 16 進。Cookie のハッシュは外に出さない）、`last_seen_at`、`ip`、`user_agent` を持つ。`last_seen_at` は 1 分以上たっていれば更新する（ポーリングのたびに書き込まないため）。
- クライアントの IP: `koroku` はループバックで待ち受け、Caddy を経由して届く。接続元がループバックのときだけ `X-Forwarded-For` の最後の値を使う。Caddy は既定でクライアントから来た `X-Forwarded-For` を信用せず、接続元の IP で置き換えるので、偽装できない。User-Agent は 512 バイトで切る。
- `login_events` テーブル: 時刻、結果（`succeeded`、`not_allowed`、`two_factor_required`、`two_factor_unknown`）、GitHub のユーザー ID と login、IP、User-Agent。GitHub のユーザーが分かったものだけを残し、state の不一致や GitHub の失敗は残さない。誰でも起こせて、記録が溢れるだけなので。新しい 200 件を残して古いものを消す。
- 画面: ユーザーメニューから「セキュリティ」を開き、セッションの一覧（このセッションを示す、個別に終了、ほかをすべて終了）と、ログイン履歴を見る。

### 10.4 設定の点検と SSH

外部サービスの設定の誤りを早く見つけるための `koroku check` と、SSH の入口を狭めるプロビジョニングの手順。利用者向けの手順は `docs/SETUP.md`。

**`koroku check`**

- 外部サービスの設定は手作業で、誤りは「ログインできない」「バックアップが夜に失敗する」のように、使ってみるまで表に出ない。`koroku check` は `config.toml` を読み、次を 1 つずつ確かめて、直し方と `docs/SETUP.md` の節を示す。失敗が 1 つでもあれば終了コード 1。
  - 例の値（`example.com`、`203.0.113.x`）が残っていないこと。`public_ip` がホストのインターフェースのアドレスにあること（ないときは警告。NAT の下ならありうる）。
  - DNS: `console_host` と、登録していない名前（`koroku-check-<乱数>.<base_domain>`）が `public_ip` だけに解決されること。後者が解決されなければワイルドカードがないとして警告する（アプリごとの登録でも運用できるため）。判定は独自ドメインの検証（8 章）と同じ関数。
  - Caddy の admin API が応答すること。`console_url` に HTTPS で接続でき、証明書が信頼できること。`acme_ca` が Let's Encrypt の staging なら、信頼できない証明書は警告にとどめ、本番への切り替えを促す。
  - `master.key` があり 0600 であること（ないときは警告。初回起動で作られる）。
  - GitHub App: App の JWT で `GET /app` が通り、`app_id`・`client_id` が一致し、権限が Contents と Metadata の read だけであること（足りなければ失敗、余分なら警告）。`GET /app/installations` で 1 つ以上インストールされていること。`allowed_user_id` が実在するアカウントであること（`GET /user/{id}`、認証なし）。callback URL は API で読めないので、正しい値を表示するだけにする。
  - オブジェクトストレージ: `koroku-check/<乱数>` に書き、読み、一覧し、消す。`logs/` と `restic/` の外なので、ライフサイクルルールにも restic にも触れない。トークンの権限不足をここで見つける。
  - restic: 基盤のスナップショットを一覧できること（パスワードの誤りはここで失敗する）。まだ 1 つもなければ警告（リポジトリは最初のバックアップで作られる）。最新が 26 時間より古ければ警告。
- 外部への書き込みはストレージの確認用オブジェクト 1 つだけで、終わったら消す。DNS、GitHub、Caddy は読むだけ。秘密は表示しない。
- `serve` の起動時には行わない。起動時に外部サービスを待つと、GitHub やオブジェクトストレージの障害で管理 UI まで止まるため。

**SSH を鍵だけにする**

- VPS の OS イメージは、パスワードでの SSH ログインを受け付けるものが多い。`koroku` を動かすホストの root はすべてのアプリと秘密を握るので、`provision.sh` はパスワードと keyboard-interactive の認証を止め、root のログインを禁じる（スクリプトを root で直接実行した場合は鍵での root ログインだけ残す）。設定は `/etc/ssh/sshd_config.d/01-koroku.conf` に置く。sshd は最初に読んだ値を使うので、`50-cloud-init.conf` より先に読まれる名前にする。
- 締め出しを避けるため、`sudo` を実行したユーザー（`SUDO_USER`、なければ root）の `~/.ssh/authorized_keys` に鍵が 1 つ以上あるときだけ行う。ないときは何もせず、最後のまとめで知らせる。書いたあとに `sshd -t` で検査し、通らなければファイルを消して止まる。反映は reload で、開いているセッションは切れない。
- `--keep-ssh-password` で、この手順だけを飛ばせる。

## 11. テスト方針

- ユニット: override 生成、ポリシー検査、Caddy 設定生成、状態遷移、ログのマスク。入力と出力がファイルで決まる部分は golden file で固定する。
- 結合（`-tags integration`）: 実 Docker と restic を使い、サンプル Compose をデプロイして HTTP 応答まで確認する。S3 互換 API はローカルの S3 互換サーバーで確かめる。
- GitHub、Caddy、オブジェクトストレージ、時計はインターフェースにしてフェイクに差し替える。外部コマンド呼び出しは 1 つの `Runner` インターフェースに集約し、引数をテストで検証する。
- プロビジョニングスクリプトは、対応する Ubuntu の版ごとに、使い捨てのコンテナで通し実行して確かめる（`deploy/test/dry-run.sh`）。

## 出典

- [Compose: Merge](https://docs.docker.com/reference/compose-file/merge/)（`!reset`、`!override`）
- [Compose: How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)（プロジェクト名）
- [docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)（`--wait`）
- [Docker: Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)
- [Caddy: Automatic HTTPS](https://caddyserver.com/docs/automatic-https)
- [Caddy: API Tutorial](https://caddyserver.com/docs/api-tutorial)
- [Let's Encrypt: Rate Limits](https://letsencrypt.org/docs/rate-limits/)
- [Let's Encrypt: Challenge Types](https://letsencrypt.org/docs/challenge-types/)
- [GitHub Docs: Authenticating as a GitHub App installation](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)
- [GitHub Docs: Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)

* [Amazon S3: Lifecycle configuration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html)
* [Amazon S3: Presigned URLs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html)
* [restic documentation](https://restic.readthedocs.io/)
