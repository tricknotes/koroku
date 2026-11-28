# koroku アーキテクチャ

koroku の現在の設計を書く。過去の経緯や変更の履歴は書かない（それは git の履歴と PR にある）。利用者向けの説明は [README](../README.ja.md) と [USAGE.md](USAGE.md)、HTTP API の形は [API.md](API.md)、構築と運用の手順は [SETUP.md](SETUP.md) と [OPERATIONS.md](OPERATIONS.md)。

## 1. 目的とスコープ

koroku は、3〜5 個の Docker Compose アプリを、VPS 1 台の上で GitHub リポジトリから UI 操作だけでデプロイする個人用のコントロールプレーンである。運用するのはオーナー 1 人で、オーナーは特定の GitHub ユーザーをメンバーとして加え、アプリごとにロールを与えられる（10.7）。以下、本体デーモンを `koroku`、ベースドメイン（`config.toml` の `base_domain`）を例として `dev.example.com` と書く。特定のホスティング事業者、DNS サービス、オブジェクトストレージには依存しない。それぞれに求める条件は [SETUP.md](SETUP.md) にまとめる。

**提供するもの**

- GitHub リポジトリ連携（private 含む）と、UI からのブランチ／コミット指定デプロイ
- `compose.yaml` のデプロイ。`build:` があればホストでビルド、`image:` のみなら pull。Compose ファイルを持たないリポジトリは、koroku に書いた Compose で動かす（6.3）
- アプリごとの `<slug>.dev.example.com` 自動割当と TLS、アプリごとの独自ドメイン追加（本番用）
- 環境変数の管理、デプロイログと実行ログの閲覧
- private なレジストリのイメージの取得（アプリごとのコンテナレジストリの認証。6.1）
- 起動・停止・再起動・削除、過去コミットへの再デプロイ（ロールバック）
- 公開サービスを blue/green で入れ替える無停止デプロイ（条件を満たすアプリが選ぶ。7.2）
- ブランチへの push での自動デプロイ（アプリごとに選ぶ。5.2）
- pull request ごとのレビューアプリ（staging を親にして手で作る。9.9）
- バックアップとリストア（DB は選んだダンプへ、PostgreSQL は指定した時刻へ戻す。10.8、10.9）、実行ログの保管

**提供しないもの**

- `git push` デプロイ、Addons、buildpack
- マルチノード、オートスケール
- マルチテナント（利用者ごとに隔離した基盤）。メンバーはオーナーのホストを共有し、アプリ単位のロールで分けるだけ（10.7）
- 公開サービス以外の無停止の入れ替え。blue/green を選ばないアプリは、デプロイ中に数秒の断がある（7 章。その間のページの読み込みは Caddy で待たせて遅い応答にする。8 章「再起動中のリクエストの保留」）。選んだアプリでも、ワーカーはデプロイのたびに、DB はアプリの環境変数を変えたデプロイで作り直し、長い接続は切れる（7.2）
- 公開サービスを常に複数のコンテナで動かす負荷分散
- ホストの冗長化（ホスト障害時は全アプリが止まる）
- Loki などのログ基盤、外部への障害通知

## 2. 設計の要点

| 項目 | 方式 | 理由 |
| --- | --- | --- |
| 実行基盤 | VPS 1 台（Ubuntu 24.04 か 26.04、x86_64）。Docker Engine + Compose v2.30 以上 | override で使う `!override` タグ（2.24.4 以上）と、ポリシー検査で使う `config --no-env-resolution`（2.30 以上、6 章）の要件（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)） |
| コントロールプレーン | Go 製シングルバイナリ `koroku`。systemd でホストに直置き、SQLite | 常駐メモリが小さく、配布と更新がファイル 1 つ。詳細は 4 章 |
| フロントエンド | Ember SPA。ビルド成果物を `koroku` に埋め込んで配信 | 配信用の別プロセスが不要 |
| リバースプロキシ | Caddy。ホストに直置きし、admin API で設定を投入 | ホスト名を渡すだけで証明書の取得と更新まで自動（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)） |
| アプリへの経路 | 公開サービスを `127.0.0.1:<割当ポート>` に publish し、Caddy がそこへ転送 | 共有 Docker ネットワークが不要で、アプリ同士が互いに見えない |
| TLS | ホスト名ごとに個別証明書（HTTP-01）。ワイルドカード「証明書」は見送り | ワイルドカード発行には DNS-01 が要り、DNS 事業者の API 連携が増える（[Challenge Types](https://letsencrypt.org/docs/challenge-types/)） |
| GitHub 連携 | 自分専用の GitHub App。ログインと clone の両方に使う | clone 用トークンが 1 時間で失効し、長寿命の秘密を置かずに済む（5 章） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p koroku-<slug>`） | プロジェクト名でリソースが分離される（[How Compose works](https://docs.docker.com/compose/intro/compose-application-model/)） |
| ビルド | `build:` があればホストで build、なければ pull | リポジトリの形に合わせて両方に対応する |
| ジョブ実行 | `koroku` プロセス内キュー、全体で同時実行 1 | ビルドの負荷を直列化。Redis 等の外部依存が不要 |
| UI 認証 | GitHub ログイン + オーナー（`allowed_user_id`）かロールを持つメンバーの ID の照合。権限はアプリごとのロール（10.7） | パスワード管理を持たない。ID は GitHub が変えないので、login の変更や取り直しで別人が入れない |

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
| `/var/lib/koroku/apps/<slug>/releases/<deploy_id>/` | チェックアウトしたソースと生成した override。Compose を koroku に書いたアプリでは `koroku.compose.yaml` も（6.3） |
| `/var/lib/koroku/apps/<slug>/env` | 展開済み環境変数ファイル（0600） |
| `/var/lib/koroku/apps/<slug>/env.release` | デプロイのたびに変わる `KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT`（0600）。デプロイのたびに作り直すサービスにだけ渡す（6 章「コミットの変数を渡すサービス」） |
| `/var/lib/koroku/apps/<slug>/docker/config.json` | アプリのコンテナレジストリの認証を書いた Docker の設定（ディレクトリ 0700、ファイル 0600。設定したアプリだけ。6.1） |
| `/var/lib/koroku/apps/<slug>/release-state/<deploy_id>/` | そのリリースの環境変数スナップショット（`env` と `env.release`、0600）と、使った Compose ファイル名・ビルドしたイメージ名。ロールバック用（7 章） |
| `/var/lib/koroku/apps/<slug>/deploys/<deploy_id>.log` | デプロイログ |
| `/var/lib/koroku/logs/<slug>/<service>/` | 収集した実行ログ（10.1） |
| `/var/log/caddy/apps/<slug>.log` | Caddy のアクセスログ（アプリ別） |
| `/var/lib/koroku/apps/<slug>/tasks/<run_id>.log` | ワンオフのコマンドとタスクの実行ログ（9.6） |
| `/var/lib/koroku/apps/<slug>/wal/<service>/` | PostgreSQL がアーカイブした WAL の、退避前の分（0700。時点指定の復旧を有効にしたアプリだけ。10.9） |
| `/var/lib/koroku/apps/<slug>/pitr-restores/<id>.log` | 時点指定の復旧のログ（10.9） |

## 4. 技術スタック選定

**Go（API）+ Ember（SPA）+ SQLite** で作る。このシステムの中心は CRUD ではなく「外部プロセスの実行・ログのストリーム配信・状態機械」で、小さな VPS でアプリとメモリを分け合うため、常駐コストと配布の軽さを優先している。

- Go は常駐メモリが数十 MB で、配布と更新がバイナリ 1 つの差し替えで済む。プロセス実行と SSE は標準ライブラリで足りる。
- Docker は `docker compose` CLI を呼んで操作する。Docker Engine の Go SDK は使わない。
- SQLite ドライバは cgo 不要の [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) を使い、クロスコンパイルと単一バイナリを保つ。
- YAML の読み取りには [go.yaml.in/yaml/v3](https://pkg.go.dev/go.yaml.in/yaml/v3) を使う。リポジトリの Compose ファイルの `include` と `extends.file` を、Compose に読ませる前に調べるため（6 章「Compose が読み込むファイル」）。標準ライブラリに YAML はなく、文字列の検索では、引用符やエスケープ、アンカーで書いたキーを見落とす。

**構成の詳細**

- Go: 標準 `net/http`、`database/sql`、SQL マイグレーションは埋め込みファイルを起動時に適用。フレームワークは使わない。
- Ember: TypeScript。API は JSON の REST、ログは SSE（`EventSource`）で受ける。
- リポジトリ: `cmd/koroku`、`internal/`（`api`、`deploy`、`compose`、`caddy`、`routing`、`github`、`domains`、`logs`、`backup`、`idle`、`review`、`store`、`runner`、`preflight` など）、`web/`（Ember）、`deploy/`（プロビジョニング）。

## 5. GitHub 連携

自分のアカウントに GitHub App を 1 つ登録し、UI ログインとリポジトリ取得の両方に使う。権限は Contents: Read-only と Metadata: Read-only に、デプロイを GitHub に写すための Deployments: Read and write（5.1）を加えたもの。ワークフローを待つ自動デプロイ（5.2）を使うなら Actions: Read-only、レビューアプリ（9.9）を使うなら Pull requests: Read-only も加える。

**デプロイできるリポジトリ**

| リポジトリ | 指定のしかた | 取得 |
| --- | --- | --- |
| App をインストールした、自分のアカウントのリポジトリ（private を含む） | 作成画面の一覧から選ぶ（`repo_full_name` と `repo_id`） | installation token |
| App をインストールした、自分が管理する別の org のリポジトリ（private を含む） | 同上 | installation token |
| App をインストールしていない public リポジトリ（OSS など） | 作成画面で URL を入れる（`repo_url`） | 認証なし |

- 別の org に App をインストールするには、App の「Where can this GitHub App be installed?」を「Any account」にする。そうすると誰でも App を自分のアカウントにインストールできるので、koroku は使うインストール先を、オーナー自身のアカウント（`allowed_user_id` と一致するアカウント ID）と、`[github] installation_account_ids` に書いたアカウント（数値のアカウント ID）に限る。それ以外のインストール先のリポジトリは一覧に出さず、token も発行しない。そのリポジトリを指定しても App が入っていないものとして扱う（public なら認証なしで読む）。
- アカウントを ID で指定するのは、login ではアカウントを特定できないため。org の名前を変えると元の名前は空き、その名前を取った別のアカウントが App をインストールできる（メンバーを ID で照合するのと同じ理由。10.7）。`[github] installation_accounts`（login の一覧、大文字小文字を区別しない）でも指定でき、どちらかの一覧にあるアカウントを使う（両方を書くと、ID の一覧と login の一覧の和になる）。login での指定は、その名前を取った別のアカウントのインストールも使ってしまう弱い形で、login で指定した既存のホストがインストール先を失わないために受け付ける。書いてあれば `koroku serve` は起動時に、ID を `installation_account_ids` に書くよう警告を出す。
- `koroku check` は、インストール先ごとにアカウントの login と ID を表示し、使わないインストール先があれば警告する。login だけで一致しているアカウントがあれば、代わりに書く `installation_account_ids` の行を示す（10.4）。システム画面の「GitHub App」は、インストール先ごとにアカウントの ID（コピーできる）と使うかどうかを示し、login だけで一致しているインストール先にはその旨を出す。別の org を加える手順（App の設定画面とインストール画面へのリンク、`config.toml` に書く `installation_account_ids` の行）も出す。作成画面のリポジトリ欄からも、そこへ移れる。
- 作成画面の Compose ファイルの検出（`GET /api/repos/{owner}/{repo}/compose`）は、App で読めないリポジトリ（未インストール、または使わないインストール先）を認証なしで読み直す。github.com 以外の URL は読めないので、サービスとポートを手で入れる。
- Compose ファイルを持たないリポジトリや、持っていても koroku では動かせない形のリポジトリは、Compose を koroku に書いて動かす（6.3）。

| 用途 | 方式 |
| --- | --- |
| UI ログイン | Web application flow でユーザーアクセストークンを得て `GET /user` の ID をオーナーとメンバーの ID と照合し（10.7）、自前のセッション Cookie を発行する。トークンは保存しない（[ユーザーアクセストークンの生成](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)） |
| リポジトリ・ブランチ・コミット・pull request の一覧 | installation access token で REST API を呼ぶ |
| デプロイの反映 | installation access token で Deployments API を呼ぶ（5.1） |
| clone | デプロイのたびに installation access token を発行し、HTTP パスワードとして使う。形式は `https://x-access-token:TOKEN@github.com/owner/repo.git`、有効期限は 1 時間（[installation としての認証](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)） |

- トークンは `.git/config` やデプロイログに残さない。`GIT_ASKPASS` か `http.extraheader` で渡し、fetch 後に破棄する。
- Webhook は自動デプロイ（5.2）のために push と workflow_run のイベントだけを使う。自動デプロイを有効にしたアプリがなければ、デプロイは常に UI か API から手動で始まる。

**リポジトリは ID で確かめる**

GitHub のリポジトリの名前（`owner/name`）は、同じリポジトリを指し続けるとは限らない。リポジトリを消せば同じ名前で作り直せ、アカウントが名前を手放せば別の人がその名前を取って同じ名前のリポジトリを作れる。名前だけで取得すると、アプリを設定したときとは別のリポジトリのコードが、そのアプリの環境変数・volume・DB につながって動く。リポジトリの数値の ID は名前の変更や移譲では変わらず、作り直したリポジトリには別の ID が付く。そこでアプリはリポジトリの ID（`apps.repo_id`）を持ち、デプロイは取得の前に、名前がいまもその ID のリポジトリを指すことを確かめる（7 章の手順 2）。

- **対象**: GitHub App で取得するアプリ（`repo_full_name` を持ち、`repo_url` を明示していない）の、取得を伴うデプロイすべて（手動、自動デプロイ、昇格、レビューアプリ。どれも 7 章の手順 2 を通る）。
- **確かめ方**: 取得に使う installation token で（token がないときは下の「App がリポジトリを見られないとき」）`GET /repos/{owner}/{repo}` を呼び、応答の `id` を `repo_id` と比べる。デプロイ 1 件につき 1 回の呼び出しで、30 秒で打ち切る。違えばデプロイは手順 2 で失敗し、何も取得しない（ビルドも起動もせず、動いているリリースには触れない）。失敗の理由は `fetch source: <owner/name> is not the repository this app was set up with: GitHub has repository <いまの ID> under that name, the app was set up with repository <アプリの ID>. Nothing was fetched. ...`。
- **ID の決まり方**: 作成画面で一覧から選んだリポジトリは、名前と一緒に ID を保存する（`repo_id`）。ID を持たないアプリ（API で `repo_id` を付けずに作ったものなど）は、最初に確かめたときに読めた ID を記録し、以後のデプロイはその ID と比べる。初回の読み取りは信頼する（そのときに名前が指しているリポジトリを、アプリのリポジトリとする）。記録は `repo_id` が空のときだけ埋める 1 つの SQL 文で行い、すでにある ID は置き換えない。記録したことはデプロイログとサーバーログに書く。ID を持たないアプリも、設定を変えずに、次のデプロイから守られる。
- **レビューアプリ**（9.9）は親の `repo_id` を写して作る。ID を持たないレビューアプリは、同じ名前のリポジトリの親が ID を持っていれば、自分では初回の読み取りを信頼せず、親の ID と比べる。合えばその ID を自分の `repo_id` に記録する。レビューアプリは親のリポジトリの pull request を出すものなので、親が知っているリポジトリに従う。
- **名前の変更と移譲**: リポジトリの ID は変わらず、GitHub は古い名前への API の要求を `301` で同じホストの `/repositories/{id}` へ送る。Go の HTTP クライアントは、同じホストへの転送には `Authorization` を付けたまま従い、別のホストへの転送には付けない（`internal/github` のテストで固定している）。応答は同じ ID と新しい名前になるので、デプロイは通る。デプロイログに GitHub でのいまの名前を書き、アプリの設定は書き換えない（リポジトリの指定を変えるのはオーナーだけ。10.7）。git の取得も、GitHub が古い名前を転送している間は通る。古い名前に別のリポジトリが作られると転送は止まり、ID が違うのでデプロイは失敗する。
- **App がリポジトリを見られないとき**（未インストール、または使わないインストール先）: installation token がなく、git は public リポジトリとして認証なしで取得する。名前を別のアカウントに取られたときは、まさにこの形になる（その人のリポジトリに App は入っていないか、入っていても使わないインストール先）。そこで ID を持つアプリは、このときも確かめる。`GET /repos/{owner}/{repo}` を認証なしで呼び（public リポジトリなら読める。GitHub の認証なしの上限は IP ごとに 1 時間 60 回）、同じように `id` を比べる。自分のリポジトリを public にして App のインストール先から外した、使わないインストール先のアカウントへ移譲した、という場合は ID が合うので通る。ID を持たないアプリは、このときは確かめず、ID も記録しない（App が一度も見たことのないリポジトリで、`repo_url` で指定した public リポジトリと同じ扱い）。デプロイログに、確かめずに取得することを書く。
- **確かめられないとき**: GitHub の API が失敗した（レート制限、障害など）ときは、確かめずに先へ進まず、手順 2 で失敗にする。token の発行の失敗がデプロイを失敗にするのと同じ扱い。理由は `fetch source: cannot confirm that <owner/name> is the repository this app was set up with: <GitHub の応答>. Nothing was fetched; deploy again once GitHub answers`。ID を持つアプリで、その名前のリポジトリが見つからない（404。認証なしで読むときは、public でないものを含む）ときも、同じく失敗にする。ID を持つアプリは、ID を確かめられたときにだけ取得する。ID を持たないアプリで見つからないときは、比べるものも記録するものもないので取得に進み、git の失敗が理由になる。
- **確かめないもの**:
  - `repo_url` を明示したアプリ。URL の先をそのまま取得する（`repo_id` を持っていても比べない）。
  - ID を持たないアプリで、App がそのリポジトリを見られないとき（上記）。
  - 保持しているリリースへのロールバック（7.1）。取得をしない。
  - 管理 UI のための読み取り（ブランチとコミットの一覧、Compose ファイルの検出、必要な環境変数の照合、pull request の一覧）。名前で読むので、名前が別のリポジトリを指していれば、そのリポジトリの内容が画面に出る。読むだけで何も動かさず、そこから始めたデプロイは手順 2 で止まる。
- **画面**: ID が違って失敗したデプロイは、履歴の行と詳細に理由を日本語で出し（名前、いまの ID とアプリの ID、何も取得していないこと、直し方）、koroku が書いた英語の理由を添える。確かめられなかった失敗は、koroku が書いた英語の理由をそのまま出す。
- **直し方**: 名前の先のリポジトリを意図して入れ替えた（消して作り直した、など）ときは、オーナーが `PATCH /api/apps/{slug}` で `repo_id` を新しい ID にする（失敗の理由に、いまの ID が出る）。リポジトリの指定の変更なので、オーナーだけができ、再ログインが要る（10.3、10.7）。`repo_full_name` か `repo_url` を変えて `repo_id` を送らなければ、古い ID は消え、次のデプロイが新しい名前の ID を記録する。`repo_id` の変更は操作の記録（10.7）に残る。

**ログインと保護の細則**

- セッションは 7 日で失効し、操作しても延長しない。Cookie の値は乱数 32 バイトで、DB には SHA-256 のハッシュだけを置く（DB が漏れてもセッションを再利用できないようにするため）。`config.toml` のオーナー（`allowed_user_id`）を変えると、前のオーナーの既存のセッションは次のリクエストから無効になる（前のオーナーがメンバーとしてロールを持っていなければ）。メンバーのセッションも、ロールをすべて失えば同じく無効になる（10.7）。
- 変更系リクエスト（`POST` / `PUT` / `PATCH` / `DELETE`）は、`Origin` ヘッダーが管理 UI のオリジン（`console_url`、既定は `https://<console_host>`）と完全一致するものだけを通す。ヘッダーがないリクエストも拒否するので、`curl` で叩くときは `-H "Origin: https://console.<base>"` を付ける。
- GitHub App が未設定（`[github]` セクションなし）のときは、誰もログインできず API は 401 を返す。「未設定なら素通し」にはしない。ローカル開発では `koroku serve --insecure-no-auth` で認証と Origin 検証を外せる。設定ファイルではなく起動フラグにしているのは、本番の unit ファイルに紛れ込みにくくするため。
- `[github]` は全項目（`app_id`、`client_id`、`client_secret`、`private_key_file`、`allowed_user_id`）が揃っていなければ起動時にエラーにする。秘密鍵ファイルはグループや他人が読める権限なら拒否する。
- リポジトリの指定が `repo_full_name`（`owner/name`）のアプリは installation token を使って取得する。取得の前に、名前がアプリのリポジトリをいまも指すことを ID で確かめる（上の「リポジトリは ID で確かめる」）。GitHub App がそのリポジトリにインストールされていなければ、public リポジトリの可能性があるので認証なしで取得を試みる（ID を持つアプリは、その前に認証なしで ID を確かめる）。`repo_url` を明示したアプリには token を使わない。
- アプリ作成時の Compose ファイル検出（`GET /api/repos/{owner}/{repo}/compose`）は、サービス名とポートを画面に下書きするためのもので、作成画面の「既定のブランチ」の欄にあるブランチ（空ならリポジトリの既定のブランチ）を読む。欄を変えて離れると読み直す。読むのはルートの既定の名前（`compose.yaml` など）の候補で、候補から選ぶか、ルートからのパスを入力できる（`deploy/compose.production.yml` など。入力したファイルも読んでサービスとポートを下書きし、作成時の `compose_path` にする）。YAML パーサーの依存を増やさず行ベースの簡易読み取りで済ませる（アンカーやフロースタイルは読めない。読めなければ手入力）。デプロイ時の判定は `docker compose config` の結果で行う。作成画面は同じ読み取りで、環境変数の初期値（9.8）も下書きする（同じブランチを読み、欄を変えたときに一緒に読み直す）。
- fine-grained PAT は使わない。長寿命の秘密をサーバーに置くことになり、ログイン手段も別に要るため。

### 5.1 GitHub の Deployments への反映

koroku のデプロイを、リポジトリの GitHub Deployments（`POST /repos/{owner}/{repo}/deployments` と `.../deployments/{id}/statuses`）に写す。リポジトリのページの Deployments 欄とコミットの画面に、環境ごとに「いま何が出ているか」と最新の結果が出るようにするため。GitHub 側の記録は表示のためのもので、デプロイの判断には使わない（DB が唯一の正）。

**対象**

- GitHub App で取得するアプリ（`repo_full_name` を持ち、`repo_url` を明示していない）だけ。`repo_url` で指定したアプリと、App がインストールされていないリポジトリ（認証なしで取得する public リポジトリ）は対象外で、何も送らない。
- 必要な権限は Repository permissions の **Deployments: Read and write**。Contents と Metadata だけの App でも koroku は動き、反映だけが止まる（`koroku check` が警告する。10.4）。

**写し方**

- koroku のデプロイ 1 件を、GitHub のデプロイ 1 件にする。`ref` は koroku がデプロイするコミットの完全な SHA、`environment` はアプリの slug（`blog`、`blog-staging` など。staging と本番は別アプリなので別の環境になる。8.2）、`production_environment` はアプリの `environment` が `production` のとき真、`transient_environment` はレビューアプリ（9.9）のとき真で、それ以外は偽。`auto_merge` は偽、`required_contexts` は空にする（GitHub にブランチのマージやコミットのステータスの確認をさせない。koroku はすでに出すものを決めている）。ロールバック（7.1）と昇格（9.7）も 1 件のデプロイとして写る。
- 状態は koroku の状態機械に合わせる。

| koroku | GitHub の state | 説明文（`description`） |
| --- | --- | --- |
| `queued` | `queued` | `Deployment queued` |
| `running` | `in_progress` | `Deployment in progress` |
| `succeeded` | `success` | `Deployment succeeded` |
| `failed` | `failure` | `Deployment failed`。自動ロールバックで前の版に戻ったときは `; the previous release was restored` を足す |
| `failed (interrupted)` | `error` | `Deployment interrupted: koroku restarted` |
| `canceled` | `error` | `Deployment canceled`。新しいデプロイに置き換わったときは `Deployment superseded by a newer one` |

- 説明文の `Deployment` は、ロールバックなら `Rollback to koroku deployment #<id>`、昇格なら `Promotion from staging` になる。英語にするのは、GitHub の画面とデプロイログに合わせるため。
- 説明文にエラーの本文は入れない。ホストのパスやコマンドが含まれうるうえ、public リポジトリでは誰でも読めるため。詳しい理由は管理 UI で見る。
- 各状態の `log_url` は管理 UI のデプロイ画面（`<console_url>/apps/<slug>/deploy?deployment=<id>`。そのデプロイを開いた状態で表示する）。`success` には `environment_url` としてアプリのプライマリ URL（9.1 の `primary_url` と同じ規則）を付ける。
- 置き換えられたデプロイは、koroku が `inactive` にする。デプロイの `success` が GitHub に届いたら、その環境で最新の状態が `success` のデプロイ（GitHub が Active と表示するもの）を問い合わせ、koroku がそのアプリで最後に `success` を届けたデプロイより古いものすべてに `inactive`（説明文は `Replaced by a newer deployment`）を送る。環境に Active が 1 件だけ残り、「いま何が出ているか」が読めるようにするため。
  - GitHub の `auto_inactive` には任せない。GitHub は本番の環境（`production_environment`）と消える環境（`transient_environment`）の古いデプロイを `inactive` にしないと定めていて、それ以外の環境でも、App が作ったデプロイは `success` を重ねても Active のまま残る（実機で確かめた振る舞い）。
  - 環境の名前が同じなら、koroku が作っていないデプロイ（GitHub Actions のワークフローが作ったものなど）も `inactive` にする。同じ名前の環境に出ているものは 1 つ、という `auto_inactive` と同じ見方をとる。koroku の最後の `success` より新しいものには触れない。
  - 問い合わせは GraphQL（`POST /graphql`）で、環境の新しい 100 件の最新の状態を 1 回で読む。REST の一覧には状態が含まれず、デプロイごとに問い合わせることになるため。
  - 失敗したデプロイは何も `inactive` にしない。自動ロールバックで旧版が動き続けている間は、GitHub でも旧版が動いているように見える。
  - レビューアプリ（9.9）のデプロイは `inactive` にしない。GitHub は消える環境の `inactive` を「破棄された」と表示するが、レビューアプリは次のコミットで動き続けているため。
  - `inactive` の送信が失敗して残ったものは、そのアプリの次の `success` で片付く。
- `queued` のまま取り消されたデプロイで、まだ GitHub に作っていないものは作らない（置き換えのたびに GitHub に `error` が並ばないように）。

**デプロイを止めないこと**

- GitHub への呼び出しはデプロイの経路に置かない。デプロイのワーカーは状態を DB に書いたあとで変化を知らせるだけで、送信は別の 1 つの goroutine が順に行う。知らせを溜める器（256 件）が溢れたら捨てて記録する。
- 送信のたびに DB からデプロイの今の状態を読み直して送る。GitHub の状態は `queued` → `in_progress` → 終わりの状態、の順にしか進めないので、知らせの処理が遅れても逆戻りや二重送信は起きない。
- 1 回の呼び出しは 10 秒で打ち切る。ネットワークのエラー、5xx、レート制限は 2 秒と 10 秒をおいて 2 回まで繰り返す。それ以外の 4xx は繰り返さずに記録する。ログに出すエラーは `internal/github` がトークンを取り除いたもの。
- 403（App に Deployments の権限がない）は、アプリごとに 1 回だけ警告を出し、そのアプリを 1 時間送信の対象から外す。App がそのリポジトリを見られない（404）ときは警告しない（public リポジトリの通常の姿）。

**再起動をまたぐ**

- GitHub のデプロイ ID と、GitHub が最後に受け付けた状態を `github_deployments` に置く（koroku のデプロイ 1 件に 1 行。デプロイの削除で消える）。後続の状態は同じ GitHub のデプロイに送る。
- 起動時と 10 分ごとに、24 時間以内に終わったデプロイのうち、GitHub に終わりの状態が届いていないもの（送信前に `koroku` が止まった、GitHub が落ちていた、など）を送り直す。`koroku` の停止と重なって `failed (interrupted)` になったデプロイも、次の起動でここから `error` になる。
- GitHub でデプロイが消されていて状態の送信が 404 になったものは、以後送らない。

### 5.2 push での自動デプロイ

アプリごとに「自動デプロイ」を有効にすると、決めたブランチに push されたとき、そのコミットを自動でデプロイする。staging を「main に入れたら出る」状態にしておくため。既定は無効で、無効のアプリの振る舞いは変わらない。

**受け取り方**

- GitHub App の Webhook で push のイベントを受ける。URL は `https://<console_host>/webhooks/github`（`console_url` + `/webhooks/github`）。App の設定で Webhook を Active にし、URL と secret を入れ、Subscribe to events で Push を選ぶ（[SETUP.md](SETUP.md) 4）。Push のイベントには追加の権限は要らない（Contents の read で足りる）。
- 自前のポーリングや、リポジトリごとの Webhook は使わない。App の Webhook なら、App を入れたリポジトリすべての push が 1 か所に届き、リポジトリごとの設定が要らないため。
- secret は `config.toml` の `[github] webhook_secret`（20 文字以上）。`client_secret` と同じく `config.toml` に直接書き、ファイルは所有者だけが読める権限にする（10 章）。未設定なら、エンドポイントはすべての配送を 403 で断る（署名を確かめようがないため。「未設定なら素通し」にはしない）。

**エンドポイントの検査**（`POST /webhooks/github`）

- セッション、`Origin`、Content-Type の規則（10 章）の対象から、このパスの `POST` だけを明示して外す。送ってくるのはブラウザーではなく GitHub なので。代わりに次で守る。
- 署名: `X-Hub-Signature-256`（本文の HMAC-SHA256、`sha256=<16 進>`）を secret で計算した値と定数時間で比べる。ヘッダーがない、形が違う、一致しない配送は 401。署名と本文はログに出さない。
- 大きさと頻度: 本文は 5 MiB まで（超えたら 413）。受け付けは全体で 1 分 60 件まで（超えたら 429）。署名の検査より前に数えるので、偽の配送をいくら送っても本文を読む以上の負荷はかからない。そのかわり、溢れている間は本物の push も断られる（GitHub の画面から再配送できる）。
- Content-Type は `application/json` だけ（App の Webhook は常に JSON）。
- イベント: `X-GitHub-Event` が `ping` なら 200 で応え、`push` と `workflow_run` 以外は 200 で何もしない。`push` でも、タグ、ブランチの削除（`deleted: true` か `after` がゼロの SHA）、どのアプリも追っていないブランチは何もしない。`workflow_run` は下の「ワークフローを待つ」。
- 重複: 同じ配送を 2 回デプロイしない。`X-GitHub-Delivery` と本文の SHA-256 を、アプリごとの鍵としてメモリに 24 時間（最大 4096 件）覚える。GitHub の再配送は同じ配送 ID と本文で来るので、デプロイ済みのアプリには何もしない。本文を見て弾くのは、署名付きの本文が配送 ID だけ変えて送り直された場合のため。覚えた内容は `koroku` の再起動で消える。署名には時刻が入らないので、盗まれた配送を再起動のあとに送り直されると古いコミットがデプロイされうる。配送は TLS の中を通り、GitHub の配送の記録は App の持ち主にしか見えないので、この残りは受け入れる。

**どのアプリが動くか**

- 自動デプロイが有効で、GitHub App で取得する（`repo_full_name` を持ち、`repo_url` を明示していない）アプリ。`repo_url` のアプリでは有効にできない（API が 422 で断る）。App を通さない取得には push が届かないため。
- リポジトリの照合は、アプリと配送の両方にリポジトリ ID があれば ID で（名前の変更に強い）、なければ `full_name` を大文字小文字を区別せずに比べる。
- ブランチはアプリの `auto_deploy_branch`、空なら既定のブランチ（`default_branch`）。完全一致で比べる。
- 停止中（`desired_state = stopped`）のアプリはデプロイしない。止めたアプリが push で勝手に起動しないように。
- 1 つの push に複数のアプリが応じてよい（同じリポジトリの staging と本番が同じブランチを追う、など）。

**デプロイのしかた**

- 応じたアプリごとに、push の `after`（ブランチの新しい先頭）のコミットを、手動のデプロイと同じ経路（7 章）でキューに積む。ref はブランチ名（コミットがブランチからたどれるかの確かめも、このブランチで受ける。7 章「デプロイできるコミット」）、コミットメッセージは `head_commit` のもの。ディスクの検査、同時実行 1、「後から積まれたデプロイが待っている古いデプロイを置き換える」規則、本番の自動ロールバック、GitHub Deployments への反映（5.1）は、手動のデプロイと同じ。続けて push すると、待っている古い push のデプロイは新しいものに置き換わる。
- `deployments` に、何が始めたか（`trigger_kind`: `manual` か `push`）と push した人の login（`pusher`。配送の `sender.login`、なければ `pusher.name`）を残す。デプロイの JSON の `trigger` と `pusher` で返し、UI は履歴に「push で自動デプロイ」と出す。デプロイログの最初の行にも `pushed to <branch> by <login>` と書く。
- そのアプリでコマンドやタスク（9.6。定期実行を含む）が実行中なら、配送は受け付けて、デプロイを実行が終わるまで預かる（下の「実行中のコマンドの後に回す」）。そのアプリの結果は `deferred`。
- キューに積めなかったアプリ（ディスク使用率の超過、DB の失敗）があれば 503 で応え、そのアプリの重複の記録を消す。GitHub の配送の記録に失敗として残り、原因を取り除いてから再配送すると、積めなかったアプリだけがデプロイされる。応答の本文はアプリごとの結果（`queued`、`deferred`、`duplicate`、`stopped`、`failed` と理由）。
- push で始まるデプロイには確認ダイアログも再ログインもない（誰も画面の前にいない）。その代わり、本番アプリで自動デプロイを有効にすること（と、追うブランチや待つワークフローを変えること）を、確認ダイアログと再ログイン（10.3）の対象にする。そのブランチに push できる人が、本番を更新できるようになるため。

**実行中のコマンドの後に回す**

コマンドとタスクの実行中のアプリには、デプロイを積まない（9.6「同時実行」。実行は最長 30 分続き、全体で 1 つのデプロイのワーカーを塞ぐため）。手動のデプロイは `409 busy` で断れば画面の前の人が知るが、push の配送を 503 で断ると、GitHub は自動では配送し直さないので、その push のデプロイは誰にも気づかれずに失われる。定期実行（9.6）を使うアプリでは、深夜の集計が走っている間の push で日常的に起きる。そこで自動デプロイは、断らずに預かって、実行が終わってから積む。

- **預かる場所**: `pending_auto_deploys`（アプリごとに 1 行。アプリの削除で消える）。ブランチ、コミット、コミットメッセージ、push した人、待ったワークフロー（あれば）、受け取った時刻。DB に置くので `koroku` を再起動しても残る。
- **新しい push が勝つ**: 預かっている間に同じアプリへ次の配送が来たら、行を置き換える（受け取った順で後のもの）。積まれたデプロイで、後から積まれたものが古いものを置き換えるのと同じ規則。預かったものは積まれていないので、置き換わった古いほうは記録に残らない（デプロイは始まっていない）。
- **判定**: 「実行中か」の判定と預ける書き込みは、デプロイの受付と実行の開始が使う 1 つのロック（9.6）の中で行う。実行の終わりは、実行の記録を外したあとで同じロックを取って預かったものを読むので、終わる直前の配送も取りこぼさない。
- **積むとき**: 実行が終わったとき（成功、失敗、キャンセル、タイムアウトのどれでも）と、`koroku` の起動時（中断された実行を `failed` にしたあと）に、預かったものを手動のデプロイと同じ経路で積む。ディスク使用率の検査、同時実行 1、置き換えの規則、GitHub Deployments への反映（5.1）は、ふつうの push と同じで、`trigger_kind` は `push`、`pusher` も預かったもの。デプロイログの最初の行は push と同じで、続けて `Deferred while a command was running on the app; queued when it ended.` と書く。
- **積む前に見直す**: アプリを読み直し、自動デプロイが無効になった、停止中になった、追うブランチや待つワークフローが変わった、のどれかなら積まずに捨てる（預けたあとの設定の変更を優先する）。
- **積めなかったとき**: 見直しで捨てたとき、ディスク使用率の超過で積めなかったときは、行を消し、操作の記録（10.7）に koroku 自身の操作として `auto_deploy.dropped`（コミット、ブランチ、push した人、理由）を残し、サーバーログにも書く。配送はもう 2xx で応えているので、GitHub から再配送はされない。記録を見て手動でデプロイする。DB の失敗では行を残し、次に実行が終わったときか次の起動で積み直す。
- **ほかのデプロイが先に積まれたとき**: 預かっている間に、そのアプリへ新しいデプロイが積まれたら（`koroku` の再起動の直後で実行が終わっていない場合など）、預かったものは捨てる。新しいデプロイのほうが後なので、置き換えの規則と同じ向きにそろえる。記録は残さない。
- **重複**: 預けたアプリの重複の記録（配送 ID と本文）は消さない。GitHub が再配送しても `duplicate` になり、同じ push を二重に預けない。`koroku` を再起動すると記録は消えるが、同じ push を預け直すだけで害はない。
- **見せ方**: `GET /api/apps/{slug}/deployments` の `pending_auto_deploy`（なければ `null`）。デプロイタブの履歴の上に「コマンドの実行が終わったら自動デプロイします」とコミット・ブランチ・push した人・受け取った時刻を出し、タスクタブへのリンクを置く。キャンセルの操作は持たない。実行が終わって積まれたあとは、ふつうのデプロイとして取り消せる。
- **手動のデプロイは `409 busy` のまま**: 依頼した人が画面の前にいて、断られたことが分かる。預かると「押したのに何も起きない」ように見え、実行が終わる時刻に予期せずデプロイが始まる。ロールバック（7.1）と昇格（9.7）も同じ。
- 実行中のアプリへの push だけを預かる。DB のコピー中（10.6）やほかの手動操作の最中は、これまでどおり積んで、デプロイが操作の完了を待つ（10 章「起動制御と削除」）。

**ワークフローを待つ**

Compose が `image:` だけで、イメージを GitHub Actions がコミットごとにビルドする（`ghcr.io/<owner>/<repo>:sha-${KOROKU_COMMIT_SHA}` など。6 章）アプリでは、push の直後にデプロイするとイメージがまだなく、pull が `not found` で失敗する。そこでアプリごとに「先に待つワークフロー」（`auto_deploy_workflow`）を選べるようにし、設定したアプリは push ではなくそのワークフローの成功でデプロイする。

- 値はワークフローのファイルのパス（`.github/workflows/image.yml` など）。`.github/workflows/` の直下の `.yml` か `.yaml` だけを受け付ける（GitHub がワークフローとして読むのはそこだけ）。空なら待たない（push の直後にデプロイする）。
- 設定したアプリには、push の配送では何もしない（応答の本文のそのアプリの結果は `waiting_workflow`）。
- GitHub App の Webhook で `workflow_run` のイベントを受ける。このイベントの購読には Repository permissions の **Actions: Read-only** が要る（[workflow_run](https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_run)）。koroku は Actions の API を呼ばない。
- 次がすべて成り立つ配送で、そのアプリをデプロイする。
  - `action` が `completed` で、`workflow_run.conclusion` が `success`。
  - 走ったワークフローのパスが設定と完全一致する。パスは配送の `workflow.path`（なければ `workflow_run.path`）で、`@<ref>` が付いていれば外し、`<owner>/<repo>/` が前に付いていれば外してから比べる（再利用のワークフローなどでは参照の形で書かれうるため）。
  - 配送の `repository` がアプリのリポジトリ（照合は push と同じ）で、`workflow_run.head_repository` も同じリポジトリ。fork からの pull request で走ったものはデプロイしない（fork の同名のブランチを追うブランチと取り違えないため）。
  - `workflow_run.head_branch` がアプリの追うブランチ（push と同じ規則）。
  - `workflow_run.head_sha` が、いまのブランチの先頭である。
- 先頭かどうかは、配送を受けたときに GitHub にブランチの先頭を問い合わせて決める（手動のデプロイでブランチ名を SHA にするのと同じ経路。installation token の REST API で、読めなければ `git ls-remote`）。koroku が覚えている最後の push とは比べない。push の配送を取り逃したとき、`koroku` を再起動して記憶が消えたとき、ワークフローの順序が入れ替わったときにも、GitHub の今の状態で決まるため。先頭でなければ何もしない（結果は `stale`）。後の push のワークフローが成功したときにその版が出る。後の push でワークフローが走らない（`paths` の絞り込みなど）と、そのブランチは次にワークフローが走るまで自動では出ない。問い合わせに失敗したら、積めなかったときと同じく 503 で応え、重複の記録を消す（再配送でやり直せる）。
- 連鎖したワークフロー（CI が終わってから `on: workflow_run` で動くイメージのワークフロー）もそのまま使える。後ろのワークフローの `workflow_run` の配送では、`head_branch` と `head_sha` は元の push のものになるので、待つワークフローには最後にイメージを置くもの（後ろのもの）を書く。
- デプロイは push と同じ経路で積む。`trigger_kind` は `push`（push が始まりで、ワークフローを待っただけなので）、`pusher` は `workflow_run.actor.login`（push した人。なければ `sender.login`）、コミットメッセージはブランチの先頭を問い合わせたときのもの。デプロイログの最初の行は `pushed to <branch> by <login>, after workflow <path> succeeded` になる。GitHub Deployments への反映（5.1）も push と同じで、積んだときから始まる。
- `conclusion` が `success` 以外（`failure`、`cancelled`、`timed_out` など）なら、デプロイしない。応じたアプリ（パス・リポジトリ・ブランチが合い、停止中でないもの）ごとに、操作の記録（10.7）に koroku 自身の操作として `auto_deploy.skipped` を残す（ワークフロー、結論、コミット、ブランチ、実行の URL）。デプロイは始まっていないので GitHub Deployments には何も送らない（失敗したワークフローは GitHub の Actions の画面に出ている）。先頭でない（`stale`）ものは記録しない。ふつうに起きることで、記録が溢れるため。サーバーログには残す。
- `completed` 以外の `action`（`requested`、`in_progress`）、どのアプリも待っていないワークフローは何もしない。
- 重複の記録は push と同じ（配送 ID と本文、アプリごと）。ワークフローを再実行して成功したときは別の配送になるので、同じコミットをもう一度デプロイする。

**点検**

- `koroku check`（10.4）は、自動デプロイが有効なアプリがあるのに `webhook_secret` がないとき、App が push のイベントを購読していないとき（`GET /app` の `events`）、App の Webhook の URL が違うとき（`GET /app/hook/config`）に警告する。ワークフローを待つアプリがあるのに、App に Actions の read の権限がないとき、App が workflow_run のイベントを購読していないときも警告する。どのアプリが有効かは DB を読み取り専用で開いて読む（マイグレーションを当てない）。
- 管理 UI のデプロイタブの「デプロイの設定」（9.1）は、自動デプロイを有効にしている間、Webhook の URL（コピーできる）と App 側に要る設定を示し、secret が未設定なら警告を出す（`GET /api/system` の `webhook`）。

## 6. Compose の扱い

リポジトリの `compose.yaml` は書き換えず、`koroku` が生成する override ファイルを `-f` で重ねて実行する。Compose ファイルをリポジトリから読まず、koroku に書いたものを使うこともできる（6.3）。

**アプリごとの設定（UI で入力）**

- Compose の置き場所（リポジトリのファイルか、koroku に書いた本文。6.3）と、リポジトリのファイルのパス（既定 `compose.yaml`、なければ `docker-compose.yml`）
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
      - /var/lib/koroku/apps/app-a/env.release
    restart: unless-stopped
    mem_limit: "512m"
    oom_score_adj: -400
    mem_reservation: "256m"
  db:
    ports: !reset []
    env_file:
      - /var/lib/koroku/apps/app-a/env
    restart: unless-stopped
    mem_limit: "512m"
    oom_score_adj: -400
    mem_reservation: "256m"
```

この例は本番アプリ（`environment = production`）のもの。staging アプリでは `oom_score_adj: 500` になり、`mem_reservation` は付かない（6.2）。

- 公開サービスの `ports` は `!override` で割当ポートに置き換え、他サービスの `ports` は `!reset []` で消す。通常のマージでは `ports` が追記されてしまうため、この 2 つのタグが必要（[Compose merge](https://docs.docker.com/reference/compose-file/merge/)）。
- ホストポートは 20000 番台からアプリ作成時に 1 つ割り当て、DB で一意に保つ。公開サービスを blue/green で入れ替えるアプリ（7.2）は、2 つ目のポートを持ち、override は公開サービスを色の付いた 2 つのサービスに写す。
- アプリの環境変数は全サービスに `env_file` で注入する（Heroku の config vars と同じ感覚）。koroku が足すコミットの変数（下記）だけは別のファイル `env.release` に書き、デプロイのたびに作り直すサービスにだけ渡す（下の「コミットの変数を渡すサービス」）。例では公開サービスの `web` が受け取り、`db` は受け取らない。`${VAR}` 補間用に、`--env-file` で両方のファイルを渡す。
- koroku 自身が `KOROKU_COMMIT_SHA`（デプロイしたコミットの 40 桁）と `KOROKU_COMMIT_SHORT`（先頭 7 桁）を `env.release` に書く。チェックアウトから `.git` は消えるので、アプリはほかの方法でコミットを知れない。CI がコミットごとにビルドしたイメージを `image: ...:sha-${KOROKU_COMMIT_SHORT}` で指せば、小さい VPS でビルドせずに pull だけで済む。エラー報告のリリース名などにも使える。あわせて env ファイルに `KOROKU_APP_URL`（デプロイした時点のプライマリ URL。9.1 の `primary_url` と同じ規則）と `KOROKU_APP_HOST`（そのホスト名）を足し、レビューアプリ（9.9）には `KOROKU_PR_NUMBER`（pull request の番号）も足す。ホスト名がリポジトリに書けないレビューアプリでも、アプリが自分の URL を組み立てられるようにするため（独自ドメインやプライマリを変えたら、次のデプロイで反映される）。名前が `KOROKU_` で始まる変数は koroku が書くものとして予約し、画面からは保存できない。環境変数の照合（9.4）は、これらを設定済みとして扱う。指紋（9.2）は取らない（アプリの変数ではないため）。ロールバックはスナップショットごと戻すので、戻した版のコミットと、その時点の URL を指す。
- `--env-file` を指定するので、リポジトリ直下の `.env` は `${VAR}` の補間に使われない（`.env` にしかない変数は未定義になり、`${VAR:-default}` は default になる）。サービスが自分で `env_file: .env` と書いていれば、その内容はコンテナに入る。override が足す koroku の env ファイルはリストの後ろに付くので、同じ名前の変数は koroku 側の値が勝つ。つまり補間は koroku の環境変数だけで決まり、リポジトリの `.env` は「サービスが明示的に読む」ときだけ効く（`testdata/apps/build-app` と結合テストで固定）。
- named volume はプロジェクト名 `koroku-<slug>` が接頭辞になるため、リリースディレクトリが変わっても引き継がれる。

**コミットの変数を渡すサービス**

`KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` はデプロイのたびに変わる。全サービスの env ファイルに入れると、コードだけを変えたデプロイでも全サービスの設定が変わり、DB も毎回作り直される（作り直しの数秒は、DB を使うリクエストが失敗する）。そこでこの 2 つは `apps/<slug>/env.release`（0600。リリースごとのスナップショットは `release-state/<id>/env.release`）に書き、次のサービスにだけ `env_file` で渡す。どれも、ほかの理由でデプロイのたびに作り直されるものである。

- 公開サービス（blue/green のアプリでは、色の付いたサービスとプロファイルで外した元のサービスも。7.2）
- ワンショットのサービス（ほかのサービスが `condition: service_completed_successfully` で待つもの。6.2 と同じ判定）
- `build:` を持つサービス（イメージ名がリリースごとに変わる）
- リポジトリの Compose の定義がコミットの変数で変わるサービス。`image: ...:sha-${KOROKU_COMMIT_SHORT}` や `environment: { KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA} }` のように、補間でコミットの変数を参照するもの

最後の判定は、手順 3 でコミットの変数だけを別の値（すべて 0 のコミット）にしてリポジトリの Compose をもう一度正規化し、サービスの定義（正規化した JSON）が変わるかで行う。デプロイログには `Services that follow the commit: <サービス>, ...` と書く。「稼働中のリリースと定義が違うか」で判定すると、DB のイメージを上げたデプロイで DB に変数を渡し、次のデプロイで渡さなくなるので、イメージを上げたあとのデプロイでもう一度 DB が作り直されてしまうため。どのサービスに渡したかはリリースの記録（`release.json` の `release_env_services`）に残す。

- 規則は blue/green かどうかによらず同じ。blue/green を有効にしても無効にしても、公開サービス以外のサービスに渡る env は変わらないので、切り替えの最初のデプロイで DB は作り直されない（7.2）。
- DB やキャッシュのように上のどれにも当たらないサービスは、コミットの変数を受け取らず、アプリの環境変数もそのサービスの定義も変えていないデプロイでは作り直されない。リリースのディレクトリ（Compose ファイルのパス）が変わっても作り直されない（結合テストで、コードだけを変えたデプロイの前後で DB のコンテナの作成時刻が変わらないことを確かめている）。
- アプリの環境変数を変えたデプロイでは、こうしたサービスも作り直す（どのサービスがどの変数を読むかを koroku は知らないため）。そのときは、稼働中のリリースにもあるサービスごとに、デプロイログの起動の手順に `Recreating <サービス> because the app's env file changed (<変数の名前>, ...); requests may fail while it restarts.` と書く。稼働中のリリースの `env` のスナップショットと新しい `env` を比べ、足した・消した・値の変わった変数の名前だけを並べる（値は書かない）。`KOROKU_APP_URL` など、koroku が `env` に足す変数の変化も含む。スナップショットが読めないときは書かない。
- スナップショットに `env.release` がないリリースは、コミットの変数を `env` に持ち、override も `env.release` を読まない。そうしたリリースが稼働中のアプリの次のデプロイでは、DB などの `env` からコミットの変数が消えるので、そのサービスは 1 回だけ作り直される（ログの変数の名前に `KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` が出る）。そうしたリリースへのロールバックは `env` だけを書き戻し、`env.release` には触れない。
- コミットの変数を読みたいサービス（エラー報告のリリース名を付けたい、ビルドしないサイドカーなど）は、`environment` で補間して参照する（`KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA}`）。値は補間でコンテナに届き、上の判定にも当たるので `env.release` も渡る。そのサービスはデプロイのたびに作り直される。

**override の細則**

- `restart: unless-stopped` は、リポジトリ側が `restart` を指定していないサービスにだけ付ける。全サービスに付けると、マイグレーション用のワンショットサービス（`restart: "no"`）が終了後に再起動し続けてしまうため。
- `build:` を持つサービスには、リリースごとのイメージ名 `koroku-<slug>-<service>:r<deploy_id>` を override の `image:` で与える。Compose の既定では毎回 `<project>-<service>:latest` に上書きされ、起動に失敗した新版のイメージで旧版のタグが置き換わり、旧リリースのディレクトリで `up` し直しても壊れたイメージが起動してしまう。リリース固有のタグにしておくと、ロールバックは旧版のイメージそのものを起動できる。イメージはリリースディレクトリと一緒に削除する。
- `build:` を持たないサービスのイメージ（補間した後の `image:`。CI がコミットごとに作る `ghcr.io/<owner>/<repo>:sha-<commit>` など）は、検証の手順で正規化した結果から、リリースの記録（`release-state/<id>/release.json` の `pulled_images`）に残す。pull の前に書くので、失敗したデプロイの分も残る。リリースを消すとき（成功後の整理、失敗したデプロイの後始末、アプリの削除）に、残っているどのリリース（ほかのアプリも含む）も記録しておらず、ホストのどのコンテナ（止まっているものも含む。`docker ps --all` の `Image`）も使っていないものだけを `docker image rm` する。`docker image prune -f` はタグのないイメージしか消さないので、これがないとコミットごとのタグがデプロイのたびに溜まる。`postgres:17` のように複数のアプリが使うイメージは、どれかのリリースが記録している限り残る。コンテナの一覧が取れないとき、読めないリリースの記録があるときは、何も消さない。`-f` は付けないので、使っているコンテナがあれば Docker も削除を断る。
- メモリ上限の決まり方は、優先度の高い順に「アプリ設定のサービス別の値」「アプリ設定の全体値」「リポジトリの Compose が自分で指定した値（`config.toml` の既定値以下のときだけ）」「`config.toml` の既定値（512m）」。既定値は、リポジトリが自分で名乗れる上限でもある。それより大きくしたいときはアプリ設定で指定する（Compose に `mem_limit: 64g` と書くだけで上限を外せてしまうのを防ぐため）。リポジトリ側が `deploy.resources.limits.memory` で書いている場合は override も同じ形式で書く。`mem_limit` と値が食い違うと Compose が `can't set distinct values` で設定全体を拒否するため。
- リポジトリが `memswap_limit` を書いていて、その値が override で付けるメモリ上限より小さくなる場合は、`memswap_limit` も同じ値に上げる。swap を禁じるために `memswap_limit` を `mem_limit` と同じにしておく書き方はよくあり、そのままアプリ設定で上限を上げると、Docker が「memory+swap が memory より小さい」として起動を拒否するため。上限より大きい値（swap を許す書き方）はそのまま残す。
- `oom_score_adj` と、メモリの予約（`mem_reservation`、または `deploy.resources.reservations.memory`）は koroku が決めて全サービスに書く。アプリの `environment` で値が変わる（6.2）。
- override に書くサービスは、リポジトリの Compose を正規化した結果（`config --format json`）に存在するものだけにする。存在しないサービス名を書くと Compose がエラーにするため、正規化 → override 生成 → もう一度正規化してポリシー検査、の 2 回 `config` を実行する。
- 正規化した JSON には `env_file` の中身が展開されて入るので、ログには出さない。
- env ファイルの値は Compose に補間されない形で書く。英数字と一部の記号だけの値はそのまま、それ以外はシングルクォート、シングルクォートか改行を含む値はダブルクォートで `\ \" \$ \n` をエスケープする。`$`・空白・`#`・引用符を含む値がコンテナにそのまま届くことを結合テストで確認している。

**ビルドと pull の判定**

1. `docker compose -p koroku-<slug> -f compose.yaml -f koroku.override.yaml config --format json` で正規化する。
2. `build` を持つサービスがあれば `docker compose build`、`image` のみのサービスは `docker compose pull`。起動の `up` は、ホストにないイメージだけを取得する（7 章の手順 6）。
3. private レジストリ（GHCR など）のイメージは、アプリのコンテナレジストリの認証で取得する（6.1）。設定していないアプリは、ホストの root の Docker の設定（`sudo docker login` したもの）を使う。

**ポリシー検査（正規化した JSON に対して実施、違反はデプロイ拒否）**

- `privileged: true`、`network_mode: host`、`pid: host`
- `/var/run/docker.sock` のマウント
- リリースディレクトリ外への bind mount
- 公開サービス名が Compose に存在しない

個人用途なので、アプリ設定の「検査を緩める」フラグ（デプロイタブの「デプロイの安全装置」。9.1）で個別に許可できるようにする。

**そのほかの検査項目**

Compose には上と同じ結果になる書き方がほかにもあるので、次も拒否する（いずれも「検査を緩める」フラグで許可できる）。

| ルール | 拒否する内容 | 理由 |
| --- | --- | --- |
| `volume_host_path` | `driver_opts` の `device` にホストのパスを指定した named volume。`type` が `nfs`・`nfs4`・`tmpfs` 以外の `driver_opts` | bind mount の検査を volume の形で回避できる。local ドライバは `driver_opts` をそのまま mount に渡すので、`type: overlay` の `o: lowerdir=...` のように `device` 以外にもホストのディレクトリを書ける（実際の Docker で、ホストのディレクトリがコンテナから読めることを確かめている） |
| `foreign_volume`、`foreign_network` | `external: true`、または名前が `koroku-<slug>_` で始まらない volume / ネットワーク。`network_mode` に書いた、`none` と同じアプリの `service:<名前>` 以外の値 | 他アプリの volume やネットワークを名前で指して使える（アプリ同士を分離するという前提が崩れる）。`network_mode` は、`host` などの決まった値でなければホスト上のネットワークの名前として扱われ、他アプリのネットワークに入れる（実際の Docker で、他アプリのサービスに届くことを確かめている） |
| `foreign_namespace` | `network_mode` / `pid` / `ipc` の `container:<名前>`、`volumes_from` の `container:<名前>`。`pid` / `ipc` / `uts` / `userns_mode` / `cgroup` の、アプリの中で完結する値（`ipc` の `none`・`private`・`shareable`、`cgroup` の `private`、`pid` / `ipc` の同じアプリの `service:<名前>`）以外 | 他アプリのコンテナの名前空間や volume に入れる。同じアプリ内の `service:<名前>` は許可 |
| `host_namespace` | `ipc` / `uts` / `userns_mode` / `cgroup` の `host` | `pid: host` や `network_mode: host` と同類 |
| `capability` | Docker の既定の権限と `IPC_LOCK`・`SYS_NICE` 以外の `cap_add` | `SYS_ADMIN` などは `privileged` に近い |
| `device` | `devices`、`device_cgroup_rules` | ホストのデバイスに触れる |
| `security_opt` | `no-new-privileges` 以外の `security_opt` | `seccomp` / `apparmor` の `unconfined` やラベルの無効化はコンテナの隔離を弱める。プロファイルをリポジトリ内のファイルで指定する形（`seccomp=./profile.json`）も、中身しだいで何でも許可できる |
| `file_outside_release` | リリースディレクトリ外の `env_file`、`secrets` / `configs` の `file`、ビルドコンテキスト、Dockerfile（コンテキストからの相対パスで `..` をたどるものを含む）、`include` と `extends.file` が読むファイル | ホストのファイル（他アプリの env ファイルなど）をコンテナやイメージに取り込める。基盤が注入する env ファイルだけは例外。Dockerfile は、ビルドが読めなかった行をそのままエラーに出すので、ホストのファイルを指すとその中身がデプロイログに出る |
| `foreign_image` | `build` のないサービスの `image` が、koroku がビルドしたイメージの名前（`koroku-` で始まる）で、同じアプリのサービスのものでない | ホストの Docker のイメージは全アプリで共通なので、名前を書くだけで、ほかのアプリのビルド結果（private リポジトリのコードを含む）を動かせる |
| `unsupported_setting` | 許可する設定の一覧にない設定と、一覧にある設定の、ほかのルールが扱わない値（下の「許可する設定の一覧」） | Compose の設定は多く、版ごとに増える。知らない設定を通すと、ホストやほかのアプリに届く設定が増えるたびに穴になる |
| `bind_writable` | 読み取り専用（`:ro`）でない bind mount | 下記 |
| `host_network`、`privileged`（ビルド） | `build.network: host`、`build.privileged: true` | ビルド中のコマンドが、ホストのループバックだけで待ち受けているもの（Caddy の admin API など）に届く |
| `runtime` | `runc` 以外の `runtime` | 隔離の弱いランタイムを選べる |
| `memory_guard` | `oom_kill_disable: true`、`memswap_limit: -1` | メモリ上限を実質的に無効にできる |

**許可する設定の一覧（ルール `unsupported_setting`）**

上の表は「危険だと分かっている設定」を値で判定する。それだけでは、表にない設定がそのまま通る。Compose には、表の設定と同じ結果になる設定がほかにもあり、版ごとに増える。実際の Docker と Compose で確かめた例:

| 設定 | 起きること |
| --- | --- |
| サービスの `provider` | `up` のときに、`type` に書いたプログラム（リポジトリ内のスクリプトでもよい）が、コンテナの中ではなく `docker compose` を起動した側（`koroku`、つまりホストの root）で実行される |
| `use_api_socket: true` | コンテナに Docker ソケットがマウントされる。正規化した結果の `volumes` には現れない |
| `build.ssh` | ホスト上の鍵ファイルが ssh agent としてビルドに渡る |
| `build.entitlements: [network.host]` | ビルドの `RUN --network=host` がホストのネットワークで動く（`build.network: host` と同じ） |
| `build.tags` | ビルドしたイメージに、ほかのアプリのイメージの名前を付けられる |
| `post_start` / `pre_stop` の `privileged: true` | フックが、サービス自身の設定によらず全ケーパビリティで実行される |

そこで、正規化した JSON の全体を「koroku が許可する設定の一覧」と突き合わせ、一覧にない設定があればデプロイを拒否する。

- 一覧は `internal/compose/schema.go` にあり、設定の名前は `docker compose config --format json` が出力するとおりに書く。対象は、トップレベル（`services`、`networks`、`volumes`、`secrets`、`configs`）、サービス、`build`、`deploy`、`logging`、フック、`volumes` の各項目、ネットワーク・volume・secret・config の定義。その先（`healthcheck` や `environment` の中身など）は見ない。
- 一覧に入れるのは、何をする設定かを確かめて、ホストにもほかのアプリにも届かないと分かったもの。値によっては届くもの（`network_mode`、`security_opt`、`driver_opts` など）は一覧に入れ、上の表のルールが値を判定する。
- 一覧に入れていない主な設定: サービスの `provider`、`use_api_socket`、`gpus`、`models`、`cgroup_parent`、`external_links`、`label_file`、`credential_spec`、`blkio_config`、`isolation`。`build` の `tags`、`ssh`、`entitlements`。フックの `privileged`。`deploy.resources.reservations` の `devices`。bind mount の `propagation`。ネットワークの `driver_opts`。secret / config の `driver`。トップレベルの `models`。
- 同じルールで拒否する値: `volumes` の `type` が `bind`・`volume`・`tmpfs` 以外（`image` はホストにある任意のイメージを読める）、volume の `driver` が `local` 以外、ネットワークの `driver` が `bridge` 以外（`macvlan` などはコンテナをホストのネットワークに直接つなぐ）、`logging.driver` が `json-file`・`local`・`none` 以外（出力をホストの journal や外部へ送る。`koroku` のログ収集は Docker から読む）、`build.cache_from` / `build.cache_to` の `type=local`（ホストのディレクトリを root で読み書きする）、`build.network` の `default`・`none` 以外。
- 値が空（`null`、`false`、`0`、空文字列、空の配列やマップ）の設定は、一覧になくても拒否しない。Compose は書いていない設定を空の値で出力することがあり、空の値は何もしない。`x-` で始まる拡張の項目は、Compose が無視するので拒否しない。
- ほかのルールと同じく、「検査を緩める」フラグで許可できる。
- 一覧にない設定を使うリポジトリは、害のない設定であっても、一覧に足すまでデプロイできない。この不便より、知らない設定を通さないことを取る。エラーには設定の名前を出す（`service "web": build.tags is not a setting koroku allows`）。
- Compose の新しい版が、普通の設定を別の名前で出力したり、全サービスに新しい項目を足したりすると、どのアプリも検査を通らなくなる。普通の設定を集めた Compose ファイル（`testdata/compose/allowed.compose.yaml`）を、結合テストで手元の Compose に正規化させ、拒否される設定がないことを確かめる。

**Compose が読み込むファイル（`include`、`extends.file`）**

`include` はほかの Compose ファイルを読み込み、`extends.file` は別のファイルのサービスを写す。どちらも Compose がファイルを読む時点で済んでしまい、正規化した結果には残らない。そのため、`docker compose config` を実行する前に、`koroku` がファイルそのものを読んで調べる。

- `include` の `env_file` は、読み込むファイルの `${VAR}` の補間に使う env ファイルを指す。ほかのアプリの env ファイルを指せば、その値がこのアプリのコンテナのコマンドや環境変数に入る（実際の Compose で確かめている）。`project_directory` と合わせて拒否する（`unsupported_setting`）。読み込まれるファイルは、読み込む側と同じくアプリの環境変数で補間される。
- `include` と `extends.file` のパスは、チェックアウトの中のファイルだけを許可する（外は `file_outside_release`）。シンボリックリンクは解決してから判定する。ディレクトリ、`${VAR}` を含むパス（補間されるので、文字列からは読むファイルが分からない）、Git や OCI のアドレス（`https://`、`ssh://`、`git@host:`、`oci://` など）は拒否する（`unsupported_setting`）。外から取得するファイルは、検査の `config` とそのあとの `build`・`up` で別々に取得されるので、検査した内容と起動する内容が同じとは限らない。
- 読み込まれるファイルも、同じ規則で順にたどる。
- YAML として読めないファイル、4 MiB を超えるファイル、100 個を超える読み込みは拒否する。`koroku` が読めないものは、中に何があるか確かめられないため。
- 検査は、デプロイの手順 3（`config` の前）と、リリースのファイルを再利用する前の再検査で行う。違反は「検査を緩める」フラグで許可できる。

リポジトリが書いた `oom_score_adj` とメモリの予約は拒否しない。override が必ず上書きするので（6.2）、効かないだけで害がない。拒否すると、ほかの基盤向けにこれらを書いたリポジトリが koroku に載らなくなる。

**書き込み可能な bind mount を禁止する理由**: チェックアウト（リリースディレクトリ）に書き込めるコンテナは、別の bind mount の元になっているパスをホストの任意の場所へのシンボリックリンクに置き換えられる。Docker はコンテナの次回起動時（`restart: unless-stopped` による自動再起動を含む）にリンクをたどるので、アプリのコードを実行できるだけの攻撃者がホストのファイルシステムに届いてしまう。また `koroku` が起動・ロールバックで再利用する Compose ファイルや override も書き換えられる。設定ファイルを渡すための bind は `:ro` を付け、データは named volume に置く（バックアップの対象も named volume だけ）。

- `env_file` のパスを検査するため、`docker compose config` には `--no-env-resolution` を付ける（付けないと env ファイルの中身が `environment` に展開され、パスが消える）。この指定で、正規化した JSON にアプリの環境変数の値が入らなくなる利点もある。Compose 2.30 以上が要る。
- リリースのファイルを再利用する `up`（アプリの起動、ロールバック）の直前にも、`config` とポリシー検査をやり直す。デプロイ時の検査のあとでファイルが書き換えられていたら、起動せずに拒否する（起動は 422 `policy_violation`、ロールバックは「戻せない」とログに書く）。
- 再検査の `config` は、そのあとの `up` と同じ条件で実行する。Compose は `${VAR}` を、env ファイルと自分の環境変数で補間するので、条件が違うと、検査したものと起動するものが別の内容になりうる（`${DOCKER_CONFIG:+...}` のように、変数があるときだけ別のパスになる書き方ができる）。そのため、補間にはそのリリースが起動するときの環境変数（リリースごとのスナップショット）を使い、`DOCKER_CONFIG` もあとの `up` と同じにする（アプリのレジストリの認証があればそのディレクトリ、コマンドの実行のように認証なしで動かすものは指定なし）。
- Compose を起動するときは、`koroku` 自身の環境変数のうち、動かすのに要るもの（`PATH`、`HOME`、ロケール、プロキシ、`DOCKER_` / `COMPOSE_` / `BUILDKIT_` / `BUILDX_` で始まるもの）だけを渡す。リポジトリの Compose ファイルが `${VAR}` で読めるのは、アプリの環境変数とこれらだけで、`koroku` のプロセスにあるほかの変数は読めない。
- bind mount の元のパスがまだ存在しない場合（Docker が作る）も、存在する最も深い親までシンボリックリンクを解決してから内外を判定する。
- override ファイルは一時ファイルに書いてから rename で置く。チェックアウトの中身はリポジトリ由来で、同名のシンボリックリンクが置かれていると、リンク先（ホスト上の任意のファイル）を root 権限で上書きしてしまうため。Compose ファイルのパスも、途中のディレクトリがシンボリックリンクでリリース外へ出ていないことを確かめる。

ただし「公開サービス以外のポート公開」と「公開サービスの `127.0.0.1:<割当ポート>` 以外への公開」は、フラグを立てても許可しない。Docker が publish したポートは ufw を通らずに外へ届くので（10 章）、override が効いていることの最終確認として常に検査する。bind mount のパスはシンボリックリンクを解決してから判定する（リポジトリ内のリンクでホストのディレクトリを持ち込めないようにするため）。

**停止の猶予（`stop_grace_period`）の上限**

アプリの全サービスの `stop_grace_period` の合計が 10 分を超える Compose は拒否する（ルール `stop_grace_period`）。「検査を緩める」フラグでも許可しない。

- 猶予は、コンテナを止めるときに Docker が SIGTERM を送ってから SIGKILL するまで待つ時間で、書かなければ 10 秒。デプロイ（7 章の手順 6）は、公開サービス、ワンショット、ビルドするサービスなど、デプロイのたびに作り直すサービス（上の「コミットの変数を渡すサービス」）のコンテナを止め、アプリの環境変数を変えたデプロイではほかのサービスのコンテナも止める。ロールバック、停止、再起動、休止（8.4）でも止める。cron のコンテナやジョブキューのワーカーが、走っている処理を終えてから止まれるように、リポジトリが猶予を延ばすのは正当な使い方である。
- それでも上限を置くのは、デプロイのワーカーが全体で 1 つで直列だから。あるアプリのデプロイが猶予を待つ間、後ろに積まれたほかのアプリのデプロイも待つ。起動に失敗して自動ロールバックすると、新しい版のコンテナを止める分の猶予がもう一度かかる。猶予の長さが koroku のタイムアウト（下記）に収まらないと、停止やロールバックの途中で `docker compose` を打ち切ってしまう。
- 1 つのサービスではなく合計で数えるのは、Compose が依存の順にサービスを止めるため。`docker compose stop` は依存される側を後に、`up` の作り直しは依存の順に 1 サービスずつ止めて作り直すので、`depends_on` でつながったサービスの猶予は足し合わさる（手元の Compose で、15 秒の猶予の 2 サービスの作り直しと停止がそれぞれ 30 秒あまりかかることを確かめている。`restart` は並行に止める）。猶予を書かないサービスは数えない（既定の 10 秒は、koroku のタイムアウトの余白で吸収する）。
- 検査を緩めて許可しないのは、ポート公開の検査と同じく、そのアプリの中で完結しない影響だから。上限はほかのアプリのデプロイの待ち時間と、koroku 自身のタイムアウトを守るためのもので、アプリの隔離の検査ではない。
- 待つのは、止められるコンテナが作られたときの値である。Docker はコンテナごとに作成時の猶予を持ち、Compose は `up` の作り直しでもその値で止める（猶予を 15 秒から 2 秒に変えたデプロイは 15 秒待ち、2 秒から 15 秒に変えたデプロイは 2 秒で済むことを確かめている）。新しい猶予は、そのデプロイで作ったコンテナを次に止めるときから効く。
- 検査はデプロイの手順 4 と、リリースのファイルを再利用する `up` の前の再検査（起動、ロールバック、コマンドの実行）で行う。上限ができる前に作られて、合計が上限を超えるコンテナが動いていることはありうる。その分は次のデプロイの作り直しで一度だけ待つ。

**koroku のタイムアウトとの関係**

猶予の上限（10 分）を `MaxStopGracePeriod` とし、コンテナを止めうる操作のタイムアウトに足しておく。

| 操作 | タイムアウト | 内訳 |
| --- | --- | --- |
| デプロイの手順 6 の `up` | デプロイ全体（`JobTimeout`、既定 30 分）の中 | 作り直しの停止（猶予の合計まで）+ `--wait-timeout`（300 秒）。`--wait-timeout` はコンテナを起動したあとの待ちだけに掛かり、停止の待ちは含まない（手元の Compose で、`--wait-timeout 3` の `up` が 30 秒あまりの停止を待って成功することを確かめている） |
| 失敗時の自動ロールバック | `--wait-timeout` + 猶予の上限 + 1 分 | デプロイの残り時間とは別に持つ |
| 起動（10 章） | `--wait-timeout` + 猶予の上限 + 1 分 | 起動でも、設定の変わったコンテナは作り直す |
| 停止・再起動・削除（`down`）・休止の `stop`・定期タスクのあとの `stop` | 猶予の上限 + 3 分 | — |
| `koroku` 自身の停止（systemd の `TimeoutStopSec=90`） | 変えない | `koroku` は停止のときにコンテナの停止を待たない。実行中の `docker compose` を止め、デプロイを `failed (interrupted)` にするだけで（7 章）、作りかけのコンテナはそのまま残り、次のデプロイがそろえる |

- デプロイの手順 6 の最初に、デプロイログへ猶予の内訳を書く。動いているコンテナのうち猶予が既定より長いもの（`docker container inspect` の `Config.StopTimeout`）と、それらを止めるのに待ちうる時間の合計（`Stopping the running containers may wait up to <合計>: <サービス> <猶予>, ...`）、新しいリリースで猶予が既定より長いサービス（`This release sets a longer stop grace period: ...`）。デプロイが遅いとき、何を待っているかが分かるように。どちらもなければ書かない。コンテナの情報が取れなくてもデプロイは続ける。

### 6.1 コンテナレジストリの認証

private なイメージ（CI がビルドして `ghcr.io/<owner>/<repo>:sha-...` に置いたイメージや、Dockerfile の `FROM` に書いたベースイメージ）は、アプリごとに持つ「コンテナレジストリの認証」で取得する。ホストの root で `docker login` すると、ログインは `/root/.docker/config.json` 1 つに入り、全アプリで共有される。メンバーにアプリを任せる構成（10.7）では、あるアプリのためのトークンでほかのアプリのイメージも取れてしまい、同じレジストリに別のアカウントで入りたいアプリ同士は上書きし合うため。

- **持つもの**: アプリごとに 1 つ、レジストリのホスト（既定 `ghcr.io`、ポート付きも可）、ユーザー名、トークン。`registry_credentials` テーブルに置き、トークンは環境変数の値と同じくマスターキーで AES-256-GCM で暗号化する。関連データにアプリの ID を入れて、暗号文をほかのアプリの行へ写しても開けないようにする。アプリの削除で消える。
- **API**: `GET` / `PUT` / `DELETE /api/apps/{slug}/registry-credential`（形は [API.md](API.md)）。トークンはどの応答にも入れず、設定済みかどうか、レジストリ、ユーザー名、更新時刻だけを返す。`PUT` でトークンを空にすると、レジストリとユーザー名が保存済みのものと同じときに限り、保存済みのトークンを残す。レジストリかユーザー名を変える `PUT` にはトークンが要る（422）。Docker はトークンを認証に書かれたホストへ送るので、保存済みのトークンを残したままホストを変えられると、認証を保存できる人（管理のメンバー、盗まれたセッション）が、ほかの人の入れたトークンを自分の用意したホストへ送らせて読み取れるため。画面は、レジストリかユーザー名を書き換えるとトークンの欄を必須にする。
- **誰が**: 見るのは閲覧以上（トークンを返さないので）、設定・変更・削除は管理以上（10.7）。保存と削除は再ログインから 10 分以内に限る（10.3）。
- **使い方**: koroku が取得を伴いうる `docker compose` を走らせる直前に、DB から `/var/lib/koroku/apps/<slug>/docker/config.json`（ディレクトリ 0700、ファイル 0600）を書き直し、そのコマンドだけに環境変数 `DOCKER_CONFIG=/var/lib/koroku/apps/<slug>/docker` を渡す。ファイルにはそのレジストリの `auths` の 1 件（`ユーザー名:トークン` の base64）だけを書く。Docker Hub（`docker.io`）は Docker CLI と同じく `https://index.docker.io/v1/` の名前で書く。トークンはコマンドの引数に入れない。
  - 対象: デプロイの `config`・`build`・`pull`・`up`（昇格もこの流れ）、失敗時の自動ロールバックと手動のロールバックの `up`、起動の `up`。`config` は取得しないが、同じ呼び出しの組み立てで渡しても害がない
  - 対象外: 停止、再起動（`restart`）、`logs`、`ps`、コマンドの実行（`run`。稼働中のリリースのイメージを使う）、バックアップのダンプ（`exec`）、DB のコピー。どれもイメージを取得しない
- **設定していないアプリ**は、`DOCKER_CONFIG` を渡さず、ディレクトリも消す。koroku の既定の設定（root の `/root/.docker/config.json`）で動くので、ホストで `sudo docker login` したレジストリはこれまでどおり使える。設定したアプリは root のログインを見ない（1 アプリで 2 つの private レジストリは使えない）。
- **削除**: DB から消したら、その場でディレクトリも消す。消し損ねても、次の操作の直前の書き直しで消える。DB が唯一の正で、ファイルは毎回 DB から作る。
- **ビルド**: `docker compose build` が BuildKit に渡す認証は、Compose（クライアント側）が読む Docker の設定から来る。`DOCKER_CONFIG` を渡すと、`FROM` のベースイメージの解決にもその認証が使われる。結合テストで、ログインを求める偽のレジストリを立て、`docker compose pull` と BuildKit の `FROM` の解決の両方が、アプリの認証を送ってくることを確かめている。
- **Docker CLI のプラグイン**: `DOCKER_CONFIG` を変えると、Docker CLI はプラグインを `$DOCKER_CONFIG/cli-plugins` とシステムのディレクトリ（`/usr/libexec/docker/cli-plugins` など）から探す。プロビジョニングは Compose と Buildx を apt のパッケージで入れるので、システムのディレクトリにあり、見つかる。root のホームの `~/.docker/cli-plugins` にだけ置いたプラグインは、認証を設定したアプリでは見つからない。
- **エラー**: `build`・`pull`・`up` が失敗し、出力にレジストリの拒否（`unauthorized`、`no basic auth credentials`、`authentication required`、`denied`、`401 Unauthorized` など。`permission denied` は除く）があれば、その手順のログに「アプリのコンテナレジストリの認証（レジストリ、ユーザー名、トークンの期限と権限）を確かめる」案内を 1 行書き、デプロイのエラーも同じ趣旨にする。認証を設定していないアプリでは、設定を促す案内にする。起動の `up` での拒否は 502 `registry_denied`。
- **タグがない**: `build`・`pull`・`up` が失敗し、出力に `manifest unknown` があれば（Docker Hub は `manifest for <image> not found: manifest unknown`、GHCR は `manifest unknown` とだけ出す。手元で確かめている）、その手順のログに「レジストリにそのタグのイメージがない。CI がコミットごとにビルドするなら、まだ push されていない。ワークフローが終わってからデプロイし直す。自動デプロイなら先に待つワークフロー（5.2）を設定する」という趣旨の案内を 1 行書き、デプロイのエラーも同じ趣旨にする。認証の拒否と両方が出たときは、認証の案内を優先する（認証が通らなければ、タグがあるかどうかも分からないため）。
- **マスク**: トークンと、`auths` に書く base64 の値をデプロイログのマスクに加える。
- **トークンの勧め**: 読み取りだけができるもの。GHCR なら `read:packages` だけを付けた classic な Personal access token か、読み取り専用のボットアカウントのトークン（画面のヒントでも勧める）。
- 限界: 認証を設定している間、トークンは平文でアプリのディレクトリのファイルにある（root だけが読める。env ファイルと同じ扱い）。Docker の credential helper は使わない（デーモンとして常駐するものが増え、アプリごとに分けにくいため）。

### 6.2 本番アプリをメモリ不足から守る

アプリのメモリの使用量は、アクセスの波、夜間のバッチ、ワンオフのコマンド、デプロイ中のビルドで上下する。重なるとホスト全体でメモリが尽き、カーネルの OOM killer が「いちばん大きいプロセス」を選んで止める。そのままでは、たいていいちばん大きい本番アプリが選ばれる。そこで、アプリの `environment`（8.2）を優先度の印として使い、本番アプリのコンテナを staging より後に止めさせ、メモリが足りなくなったときの回収も staging から先に行わせる。優先度という別の概念は足さない。staging と本番は別アプリとして分けてあり（8.2）、守りたいのは本番、という区別がすでにあるため。

**OOM killer の順序（`oom_score_adj`）**

| 対象 | `oom_score_adj` | 決めているもの |
| --- | --- | --- |
| staging アプリのコンテナ | `500` | koroku の override |
| `koroku` と、そこから起動する `docker compose`・`git`・`restic` | `0` | systemd の既定 |
| systemd-journald | `-250` | Ubuntu の unit |
| 本番アプリのコンテナ | `-400` | koroku の override |
| Caddy | `-450` | プロビジョニングの drop-in（`OOMScoreAdjust`） |
| dockerd | `-500` | Docker の公式パッケージの unit |
| containerd とコンテナの shim | `-999`、`-998` | containerd の公式パッケージの unit。shim は containerd の値 + 1 |
| sshd の待受プロセス | `-1000`（止めない） | OpenSSH |

- OOM killer は、プロセスの使用量（RSS + swap + ページテーブル）を RAM と swap の合計の 1000 分率で表した値に `oom_score_adj` を足した点数が最も大きいプロセスを止める。`oom_score_adj` の 1 は RAM と swap の合計の 0.1%（RAM 4GB + swap 2GB なら約 6MB）に当たる。
- staging の `500` は約 3GB 分の上乗せで、どの staging のプロセスも、ほかのどのプロセスより先に止まる。
- 本番の `-400` は約 2.4GB 分の割り引きで、`koroku` とその子プロセスは本番より先に止まる。`koroku` が止まっても動いているアプリには影響せず、systemd が起動し直す。
- 本番を dockerd（`-500`）より上に置くのは、dockerd が止まるとすべてのコンテナが止まるため。同じ `-500` にすると、本番の小さなサービス（数十 MB の Redis など）が dockerd より小さい点数になり、dockerd が先に選ばれうる。100 の差（約 600MB 分）があれば、dockerd の常駐の大きさでは順序が入れ替わらない。
- Caddy を本番と dockerd の間に置くのは、Caddy が止まるとすべてのアプリへの経路が切れるため。本番のプロセスを 1 つ止めるほうが影響が小さく、空くメモリも多い。Caddy の常駐は数十 MB で、守っても困らない。
- 1 回の OOM で止まるのは 1 プロセスで、コンテナの中のプロセスはすべてそのコンテナの値を継ぐ。主プロセスが止まったコンテナは `restart: unless-stopped`（6 章）で起動し直す。
- 値はリポジトリの Compose に何が書いてあっても override で上書きする。リポジトリが負の値を書けば、どのアプリでも本番より前に割り込めてしまうため。メモリ上限（6 章）と同じく、基盤が決める値である。ポリシー検査では拒否しない（6 章）。
- サービスのメモリ上限に当たったときの OOM（cgroup の中だけで起きる）は、この順序と関係なくそのサービスの中で起きる。この節はホスト全体でメモリが尽きたときの話である。
- ビルドの手順（`RUN`）のプロセスは Docker の BuildKit が動かし、koroku は値を付けない。

**回収からの保護（メモリの予約）**

本番アプリの各サービスに `mem_reservation` を付ける。Docker はこれを cgroup v2 の `memory.low` にする。カーネルはメモリが足りなくなると、使用量が `memory.low` より少ない cgroup からはできるだけ回収せず、ほかの cgroup（staging のコンテナ、ホストのページキャッシュ）から先にページキャッシュを捨て、swap へ追い出す。OOM の順序が「止めるとき」の順なら、予約は「止める手前で遅くなるとき」の順である。staging アプリには付けない。

- 予約の決まり方は、優先度の高い順に「アプリ設定のサービス別の値」「アプリ設定の全体値」「既定値（そのサービスのメモリ上限の 50%、MiB 単位に切り捨て）」。メモリ上限の決まり方（6 章）と同じ形にしている。値は `512m` の形か `0`（予約しない）。
- 既定を上限の 50% にするのは、上限を使用量のピークの 1.5 倍程度に合わせたアプリ（9.5 の目安）で、ふだんの使用量がおおよそ収まる量だから。予約を使用量より大きくしても、使っていない分は誰のものにもならない（ほかの cgroup は使える）が、予約の合計が RAM を超えると、カーネルは予約を比例で割り引いて扱い、保護の意味が薄れる。そのため合計をシステム画面で見せる（下記）。
- 予約はメモリ上限を超えられない（Docker が起動を拒否する）。override は予約を、そのサービスに実際に付く上限（アプリ設定の値、リポジトリ自身の上限、既定値のいずれか）までに切り詰める。API は、保存の時点で分かる上限（サービス別の上限、なければアプリ全体の上限、なければ既定値）を超える予約を 422 で断る。リポジトリ自身の上限はデプロイまで分からないので、そこは切り詰めで扱う。Docker は 6MiB 未満の予約を拒否するので、API も 6MiB 未満（`0` を除く）を断り、既定値の計算が 6MiB 未満になるサービスには付けない。
- 書き方はメモリ上限と同じ理由で、リポジトリの形に合わせる。リポジトリが `deploy.resources.limits.memory` か `deploy.resources.reservations.memory` を書いていれば `deploy.resources.reservations.memory` に書き、`mem_reservation` を書いていればそれにも同じ値を書く。どちらもなければ `mem_reservation` に書く。同じ項目を 2 つの形で違う値にすると、Compose が設定全体を拒否するため。
- ワンショットのサービス（ほかのサービスが `depends_on` の `condition: service_completed_successfully` で待つもの。7 章のとおり、koroku で動くワンショットは必ずこの形になる）には、設定によらず予約を付けない。アプリが起動した時点で終わっていて何も使わないので、`memory.low` に意味がなく、記録に入れるとシステム画面の合計が使われない量で膨らむため。リポジトリがこのサービスに予約を書いていれば消す。判定は Compose の `depends_on` だけで行い、`restart: "no"` は見ない（待たれていないワンショットは手順 6 で失敗するので、デプロイに成功したリリースには現れない）。
- リポジトリが書いた予約は使わない。本番では koroku の値で上書きし、staging では `!reset null` で消す（staging のコンテナに予約が残ると、本番より先に回収されるという前提が崩れる）。予約を変えたいときはアプリ設定（リソースタブの「メモリの上限と予約」か、サービスの行の「変更」。9.5）で変える。

**環境を切り替えたとき**

`oom_score_adj` と予約は override に書くので、アプリの `environment` を変えると次のデプロイから反映される（メモリ上限の変更と同じ）。起動（10 章）とロールバック（7.1）は、リリースのファイル（その版の override）をそのまま使うので、その版を作ったときの環境の値になる。設定タブは、環境の欄に「次のデプロイから」と出す。リソースタブは、サービスごとに稼働中のリリースの予約と、保存した設定で次のデプロイに付く値が違えばそれを並べる（`GET /api/apps/{slug}/services`、下記。9.5）。

**予約の記録と表示**

- デプロイの手順 3 で override を作るときに、サービスごとの予約（バイト）を `deployments.memory_reservations` に JSON で記録する。staging のデプロイは空の記録になる。ロールバックの記録は持たない（アプリの現在のリリースは戻す先のデプロイを指すので、7.1）。override をあとから読み直さずに、稼働中のリリースの予約が分かるようにするため。
- 同じ手順で、サービスごとの「形」（リポジトリ自身のメモリ上限と、ワンショットかどうか）を `deployments.memory_shapes` に JSON で記録する。予約は形とアプリ設定だけで決まるので、稼働中のリリースの形に今のアプリ設定を当てれば、次のデプロイで付く予約が Compose ファイルを読み直さずに分かる（リポジトリが次のコミットで Compose を変えれば違いうるが、それはデプロイの手順 3 まで分からない）。設定を保存してからデプロイするまでの間、画面に稼働中の値しか出ないと、保存した設定が効いていないように見えるため。形を記録する前のデプロイが稼働中のアプリは、次の値を `null` とする。
- `GET /api/apps/{slug}/services` は、サービスごとの `memory_reservation_bytes`（稼働中のリリースの記録。予約がなければ `null`）と `next_memory_reservation_bytes`（次のデプロイで付く予約。なければ `0`、形の記録がなければ `null`）を返す。リソースタブ（9.5）の予約の欄に出す。
- システム画面（9.3）は「本番アプリのメモリの予約」として、停止していない（`desired_state` が `running` の）アプリの、稼働中のリリースの記録の合計を、ホストの RAM（`/proc/meminfo` の `MemTotal`）と並べて出す。止まって起こされるのを待っているアプリも、起きれば同じ予約を持つので数える。アプリごとの内訳も出す。値は `GET /api/system` の `memory_reservation`（オーナーだけ。メンバーには `null`。内訳にほかのアプリの名前が入るため）。
- 合計と警告は稼働中のリリースの記録で出す（今かかっている保護を知らせるため）。保存した設定で次のデプロイの予約が変わるアプリには、内訳の行に「次のデプロイから」の値を並べ、合計の横にも、すべてのアプリをデプロイしたあとの合計を出す。今は予約がないが次のデプロイで付くアプリ（staging から本番に切り替えた、`0` から変えたなど）も内訳に出す。稼働中のリリースがサービスの形（`memory_shapes`）を記録していない本番アプリは、次のデプロイの値を計算できないので、内訳の行に「このリリースは予約の設定を計算するための情報を持っていません。保存した設定は次のデプロイから反映されます」と出す。何も出さないと、保存した設定が無視されているように見えるため。
- 合計に余白（1GiB）を足すと RAM を超えるとき、システム画面に警告を出す。余白は `koroku`、Caddy、Docker、カーネル、ビルド、staging アプリが使う分の目安で、これを割り込むと予約どうしで回収を押し付け合い、保護が効かなくなる。

### 6.3 koroku に書く Compose

Compose ファイルを、リポジトリから読む代わりに koroku に書いて持てる。Compose ファイルがない、開発用の構成しかない、ポリシー検査（6 章）を通らない、というリポジトリ（App をインストールしていない public リポジトリの OSS など。5 章）を、fork せずに動かすため。fork して Compose ファイルを足せば動くが、上流を追うたびに fork を保つことになる。コードはリポジトリのコミットをそのまま取得し、Compose だけを koroku が出す。

**設定**

- アプリごとに「Compose の置き場所」（`compose_source`）を選ぶ。`repository`（既定。リポジトリのファイルを `compose_path` で読む）か `inline`（koroku に書く）。本文は `compose_content`。
- 取得のしかたによらず選べる（GitHub App で取得するリポジトリでも、`repo_url` のリポジトリでも）。
- `inline` の間、`compose_path` は使わない。`repository` に戻しても本文は残し、もう一度 `inline` にすれば同じ本文で動く。
- 保存のときの検査は、`inline` なら本文が空でないことと YAML のマッピングとして読めること、どちらでも 64 KiB 以下であること。サービスや設定の中身は見ない（`include` でリポジトリのファイルを読み込む本文もあるので、本文だけでは決まらない）。中身の検査はデプロイが行う。
- 本文は DB に平文で置く。秘密は本文に書かず、`${VAR}` でアプリの環境変数を参照する（補間は 6 章のとおり koroku の環境変数だけで決まる）。
- 本文の欄（作成画面と設定タブ）には、手元の Claude Code に利用者向けの Skill（`plugins/koroku/skills/prepare-app`）で本文を作らせるための案内を置く。プラグインの導入コマンドと、Skill に「リポジトリを変えずに Compose を作り、同じ置き方でリハーサルして、貼る内容を報告する」ことを頼むプロンプトで、どちらもコピーできる。koroku 自身は本文を生成しない。リポジトリのコードを読んでビルドと起動を試す必要があり、それは取得とビルドの済んでいない作成の時点のホストではできず、利用者の手元の Docker でリハーサルするほうが、ホストに負荷も失敗したコンテナも残さないため。

**デプロイ**

- 手順 3（7 章）で、本文をチェックアウトの直下に `koroku.compose.yaml` として書き、これをそのリリースの Compose ファイルにする。直下に置くのは、本文の相対パス（`build: .`、`./config/app.conf:/etc/app.conf:ro`、`include` と `extends.file`）がリポジトリの直下から解決されるようにするため。Compose は相対パスを Compose ファイルのあるディレクトリから解決し、`extends.file` で写したサービスのパスは写した元のファイルの場所から解決するので、チェックアウトの外に置くと blue/green の override（7.2）で写したサービスのビルドコンテキストがずれる。
- 書き方は override と同じ（一時ファイルに書いて rename。6 章）。リポジトリに同じ名前のファイルやシンボリックリンクがあれば置き換える。
- リポジトリの Compose ファイルは読まない。本文が `include` か `extends.file` で名指ししたファイルだけが、リポジトリのファイルとして読まれる。
- それより後は、リポジトリの Compose ファイルを使うアプリと同じに進む。`include` と `extends.file` の検査、正規化、override の生成、ポリシー検査、ビルドと pull の判定、blue/green の写し（`extends: { file: koroku.compose.yaml }`）、コミットの変数を渡すサービスの判定のどれも、`koroku.compose.yaml` をリポジトリの Compose ファイルとして扱う。ほかの章で「リポジトリの Compose ファイル」と書いているものは、このアプリでは `koroku.compose.yaml` を指す。
- ポリシー検査は緩めない。本文を書くのはオーナーだが、よそから写した Compose には、ホストのパスの bind mount や `privileged` がそのまま入っていることが多い。検査を通らない本文は、リポジトリの Compose と同じ理由で直すか、「検査を緩める」フラグで許可する。
- デプロイログの手順 3 に `Compose file: koroku.compose.yaml (written in koroku; the repository's own Compose files are not read)` と書く。

**反映**

- 保存した本文は、次のデプロイから使う。リリースは自分の `koroku.compose.yaml` をディレクトリに持ち、起動、ロールバック（7.1）、コマンドの実行（9.6）は、ほかのリリースのファイルと同じくそれを使う（起動前の再検査も同じ）。本文を変えても、稼働中のリリースと保持しているリリースは、デプロイした時点の本文で動く。
- 置き場所を切り替えたときも同じ。`repository` と `inline` のどちらで作ったリリースかは、リリースの記録（`release.json` の Compose ファイル名）で決まる。

**誰が見て、誰が変えるか**

- 置き場所と本文を変えられるのはオーナーだけで、再ログイン（10.3）が要る。本文は、どのイメージをどのコマンドで動かし、アプリの環境変数と volume をどこにつなぐかを決めるので、リポジトリの指定を変えるのと同じ「別のコードを動かす」変更になるため（10.7）。
- 本文を読めるのは管理以上のロール。閲覧とデプロイのロール、API トークン（10.5）には返さず、アプリの JSON の `compose_content` は `null`、`compose_content_hidden` は `true` になる（置き場所 `compose_source` は誰にでも返す）。よそから写した Compose には `POSTGRES_PASSWORD: postgres` のような値が直に書かれていることがあり、環境変数の値を見られないロールに本文を見せると、その値が見えるため。重ねた結果（9.10）は、値を伏せた形で閲覧以上に見せる。
- 操作の記録（10.7）には、本文が変わったことだけを残し、本文は残さない。

**ほかの機能との関係**

| 機能 | 扱い |
| --- | --- |
| 必要な環境変数の照合（9.4） | 照合元が `compose` なら、保存済みの本文の `${VAR}` を読む。GitHub には問い合わせず、コミットにもよらない。github.com 以外の `repo_url` のアプリでも照合できる |
| 作成画面（5 章、9.8） | 置き場所で `inline` を選ぶと本文の欄が出る。公開サービスとポートは本文から下書きせず、手で入れる。環境変数の初期値は、リポジトリの Compose ファイルを読まずに下書きする（使わないファイルの `environment` で「設定済み」と判定しないため） |
| レビューアプリ（9.9） | 置き場所と本文を親から写す |
| 昇格（9.7） | Compose は昇格先のアプリの設定で決まる。昇格先が `inline` なら、昇格元と同じコミットを昇格先の本文で動かす |
| Compose の構成（9.10） | `-f` の 1 つ目が `koroku.compose.yaml` になる |

**採らない方式**

- リポジトリの Compose に重ねる（`-f` を 1 つ足す）: マージの規則（`!override`、`!reset`）を書く人に求めることになり、リポジトリの Compose が検査を通らないときに、通る形へ直しきれない。丸ごと置き換えるほうが、何が動くかを 1 つのファイルで読める。
- リポジトリを持たないアプリ（イメージだけを動かす）: デプロイはコミットを単位にしていて（履歴、ロールバック、再デプロイ）、リポジトリなしの形を足すと、その全部に別の経路が要る。イメージだけを動かすときも、そのソフトウェアのリポジトリを指し、本文に `image:` を書く。

## 7. デプロイフロー

デプロイは `queued → running → succeeded | failed | canceled` の状態を持つ 1 ジョブで、次の順に進む。

1. **受付**: UI でブランチかコミットを選ぶ。API がコミット SHA と、そのコミットを出すブランチかタグ（`ref`）を確定し、`deployments` に `queued` で記録する。自動デプロイ（5.2）では、push されたコミットの SHA とブランチで同じように記録する。
2. **取得**: installation token を発行し、リポジトリの名前がアプリのリポジトリをいまも指すこと（5 章「リポジトリは ID で確かめる」）と、コミットが `ref` からたどれること（下の「デプロイできるコミット」）を確かめてから、`releases/<deploy_id>/` に `git fetch --depth 1 origin <sha>` でチェックアウトする。
3. **生成**: env ファイル（0600）と `koroku.override.yaml` を書き出す。Compose を koroku に書いたアプリでは、先に本文を `koroku.compose.yaml` として書き出す（6.3）。
4. **検証**: `docker compose config --format json` を実行し、6 章のポリシー検査をかける。
5. **ビルド／pull**: 6 章の判定に従う。
6. **起動**: `docker compose up -d --pull missing --remove-orphans --wait --wait-timeout 300`。`--wait` はサービスが running または healthy になるまで待つ（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。`--pull missing` は、ホストにないイメージだけを取得する。Compose ファイルの `pull_policy`（`always` など）より優先されることを、手元の Compose で確かめている。pull は手順 5 で済んでいるので、起動では取得しない。ロールバック（下の「失敗時」と 7.1）と起動（10 章「起動制御と削除」）も同じ `up` を使う。リリースが持っているイメージで動かすので、レジストリに届かなくても、レジストリからタグが消えていても戻せる。ホストから消えたイメージだけは、アプリのレジストリの認証（6.1）で取得し直す。Compose は設定かイメージが変わったサービスだけを作り直す。`env.release` を受け取るサービス（公開サービス、ワンショット、ビルドするサービス、定義がコミットに従うサービス。6 章「コミットの変数を渡すサービス」）はデプロイのたびに作り直され、ジョブを動かすだけのサービス（スケジューラー、ジョブキューのワーカー）も、アプリのイメージをビルドするならここに入る。DB のようにどれにも当たらないサービスは、アプリの環境変数か、そのサービスの定義を変えたデプロイでだけ作り直す（環境変数のときはログに理由を書く。6 章）。ロールバックも同じく作り直し、停止・再起動・休止（8.4）もコンテナを止める。Docker は古いコンテナに SIGTERM を送り、猶予（`stop_grace_period`、既定 10 秒）を過ぎたら SIGKILL する。猶予の上限とタイムアウトは 6 章「停止の猶予」。
7. **経路**: Caddy 設定を再生成し、内容が変わっていれば投入する（8 章）。
8. **確認**: `http://127.0.0.1:<port><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒リトライする。
9. **完了**: `succeeded` にして `current_deployment_id` を更新。リリースディレクトリは直近 3 件を残し（消すリリースのイメージも消す。6 章）、`docker image prune -f` を実行する。

**デプロイできるコミット**

デプロイできるのは、アプリのリポジトリのブランチかタグからたどれるコミットだけ。デプロイは `ref`（ブランチかタグの名前）を持ち、コミットがその `ref` の先頭か、その祖先でなければ、手順 2 で失敗する。チェックアウトも、ビルドも、起動もしない。

- **理由**: GitHub は、リポジトリのどれかの ref からたどれるコミットなら、SHA を指定した取得に応じる。その ref には fork からの pull request の head（`refs/pull/<番号>/head`。閉じられてマージされなかったものを含む）があり、リポジトリに書けない人でもコミットを置ける。SHA の形だけを見てデプロイすると、デプロイのロール（10.7）のメンバーが、リポジトリに push できなくても、他人が fork の pull request に置いたコードを、アプリの環境変数・volume・DB につないで動かせてしまう。ブランチとタグを動かせるのは、リポジトリに書ける人だけ。レビューアプリが fork からの pull request を断る（9.9）のと同じ線引きになる。
- **`ref` の決まり方**: 受付のときに決めて `deployments.ref` に記録する。積まれたあとでアプリの追うブランチを変えても、そのデプロイが照合する `ref` は変わらない。

| デプロイの始まり | `ref` |
| --- | --- |
| 手動（UI、API）で `ref` を指定 | 指定したブランチかタグ。`sha` を一緒に指定すれば、そのコミットを `ref` と照合する |
| 手動で `sha` だけを指定 | アプリの追うブランチ。自動デプロイ（5.2）が有効ならその追うブランチ（`auto_deploy_branch`、空なら `default_branch`）、無効なら `default_branch` |
| 自動デプロイ（5.2） | push されたブランチ |
| レビューアプリを作ったときのデプロイ（9.9） | pull request の head のブランチ |
| 昇格（9.7） | 昇格元のデプロイの `ref`。昇格元が `ref` を持たなければ、昇格先のアプリの追うブランチ |

- **読む ref**: `refs/heads/<ref>` と `refs/tags/<ref>` だけ。`pull/12/head` や `refs/pull/12/head` を `ref` に書いても、その名前のブランチもタグもないので失敗する。同じ名前のブランチとタグがあれば、どちらかからたどれればよい。
- **確かめ方**: git だけで行う（`internal/gitrepo`）。GitHub App で取得するアプリにも、`repo_url` のアプリにも同じに効き、手動・自動・昇格のどの経路も手順 2 を通る。受付の API は `ref` と `sha` の形だけを見る。
  1. `git ls-remote` で `ref` の先頭を読む。コミットが先頭（注釈付きタグなら、タグが指すコミット）と同じなら、それで済む。ブランチの先頭を出すふつうのデプロイに足されるのは、この 1 往復だけ。
  2. 先頭でなければ、`git fetch --filter=tree:0 --no-tags` で `ref` の履歴をコミットだけ（ツリーとファイルの中身なし）取得し、`git rev-list` の出力にそのコミットがあるかを見る。`git merge-base --is-ancestor` にコミットを渡さないのは、partial clone では、リポジトリにないコミットを名前で指すと git がサーバーへ取りに行くため。
  3. 履歴を取得したリポジトリは消し（partial clone の設定に URL が残るため）、新しいリポジトリでコミットを取得する。
- トークンは取得と同じく、環境変数経由の追加ヘッダーで渡す（5 章）。確かめは全体を 5 分で打ち切り、超えたらデプロイを失敗にする（1 つしかないデプロイのワーカーを塞ぎ続けないため）。履歴の取得は、小さなリポジトリで 1〜2 秒、コミットが 9 万件ほどのリポジトリで 5 秒・一時的に 40 MiB ほど。partial clone に応じないサーバーは履歴をファイルごと送るので、そのぶん遅い（git は警告を出して続ける）。
- **先頭でなくなったコミット**: 積まれて待つ間に次の push が来た、コマンドの実行中に預かった（5.2）、といったデプロイのコミットは、先頭でなくなっていても祖先なのでデプロイできる。
- **force-push でブランチから外れたコミットは、SHA を指定してもデプロイできない**（再デプロイも昇格も同じ）。ブランチに残っていないコミットは、リポジトリに書ける人が置いたものかを確かめられないため。失敗の理由は `fetch source: commit <SHA の頭 8 桁> is not reachable from "<ref>": ...`、`ref` のブランチもタグもなければ `fetch source: the repository has no branch or tag named "<ref>" ...` で、どちらも、そのコミットを含むブランチかタグからデプロイし直すよう続ける。外れたコミットを出したいときは、それを指すブランチかタグを push してから、その名前を `ref` にしてデプロイする。
- 保持しているリリースへのロールバック（7.1）は取得をしないので、この確かめを受けない。すでにホストで動いていたリリースを起動し直すだけで、新しいコードを持ち込まないため。ブランチから外れたコミットのリリースにも戻せる。
- **画面からの操作は確かめに掛からない**: デプロイタブは、選んだブランチと、そのブランチのコミットの一覧から選んだコミットを一緒に送る。履歴の「このコミットを再デプロイ」は、その行の `ref` を一緒に送る（ロールバックの記録の行では、戻した先のデプロイの `ref`）。確かめで失敗したデプロイの詳細には、理由を日本語で出し、koroku が書いた英語の理由を添える。

**失敗時**

- 手順 5 までの失敗は稼働中のコンテナに触れないので、`failed` を記録するだけ。
- 手順 6 以降の失敗は、直前の成功リリースのディレクトリで `up` し直して旧版に戻す。
- `koroku` 再起動時に `running` のまま残ったジョブは `failed (interrupted)` にする。
- 直前の成功リリースがない（初回デプロイの）場合は戻す先がないので、`docker compose down` で起動しかけたコンテナを消す（volume は残す）。
- 手順 6 の `up` か手順 8 の確認で失敗したら、戻す前に `docker compose logs --tail 30` の出力をデプロイログのその手順に書く（見出しは `Last output of the containers:`）。起動に失敗した理由はたいていコンテナの出力にあるが、ロールバックでコンテナが置き換わると、アプリのログ画面からしか追えなくなるため。この出力もデプロイログのマスクを通る。そのうちエラーらしい最後の 1 行（`error`・`exception`・`fatal`・`panic` を含む行。字下げしたスタックの行と Ruby の `from ...` は除く。Rake の `<タスク> aborted!` は理由が次の行にあるので、次の行とつなげる）を、デプロイの失敗の理由の末尾に `(container output: <コンテナ>: <行>)` として 200 文字まで添える。`up` の失敗では、その前に Compose が最後に出した行（`service "migrate" didn't complete successfully: exit 1` のように、どのサービスがどう失敗したか）も `: <行>` として添える。理由の頭は公開サービスの名前（`start web-green`）なので、ワンショットのサービスが失敗したときに、どのサービスかが分かるようにするため。失敗の理由が `docker: exit status 1` だけでは、ログを読まないと原因が分からないため。該当する行がなければ、関係のない行を理由に見せないよう何も添えない。読み出しに失敗してもデプロイの結果は変えない。キャンセルや `koroku` の停止による失敗では読まない。
- ロールバックでは環境変数も戻す。env ファイルはリリースごとにスナップショットを取り（`release-state/<deploy_id>/env`）、稼働中の `apps/<slug>/env` を書き換えるのは手順 6 の直前にする。こうすると、手順 5 までの失敗では稼働中のリリースの env に触れず、手順 6 以降の失敗では旧版を「当時の環境変数」で起動し直せる。環境変数の変更が原因で起動に失敗した場合にも旧版に戻れるようにするため。
- `koroku` の停止（SIGTERM）と重なったデプロイは `failed (interrupted)` にし、停止処理の中ではロールバックを走らせない。コンテナが中途半端な状態で残りうるので、`koroku` の更新はデプロイ中を避ける。
- `docker compose up --wait` は、ヘルスチェックを持たないコンテナが起動直後に落ちて再起動を繰り返していても成功を返す。起動失敗の検出は手順 8 の HTTP 確認が担う。

**その他の規則**

- 同時実行は全体で 1。同じアプリに後続のデプロイが積まれたら、古い `queued` は `canceled` にする。
- 各手順の stdout/stderr を行単位でログファイルに追記し、SSE で配信する。環境変数の値とトークンはマスクする。
- 手動のロールバックは、保持しているリリースを再ビルドせずに起動し直す（7.1）。保持していない古い版へは「過去の SHA を同じフローで再デプロイ」する。どちらも DB マイグレーションの巻き戻しは行わない。
- マイグレーション等のリリース処理は Compose 側で表現する（起動コマンド、またはワンショットのサービスと `depends_on`）。基盤側に専用機能は持たない。手で 1 回だけ走らせたいコマンド（`db:migrate:status`、データの手直しなど）は、デプロイとは別にワンオフのコマンドとして実行する（9.6）。
- ワンショットのサービス（`restart: "no"` で終わるもの）は、別のサービスから `depends_on` の `condition: service_completed_successfully` で待たれていなければならない。待たれていないと、手順 6 の `up -d --wait` が `container ... exited (0)` で失敗する。基盤では対処せず、利用者向けの Skill（`plugins/koroku/skills/prepare-app`）で案内する。

**デプロイ時の断の範囲**: 断はデプロイ対象のアプリだけに起きる。`docker compose up` は `-p koroku-<slug>` で指定したプロジェクトのコンテナしか操作せず、その中でも設定かイメージが変わったサービスだけを再作成する（[docker compose up](https://docs.docker.com/reference/cli/docker/compose/up/)）。koroku のデプロイでは、`env.release` を受け取るサービス（公開サービス、ビルドするワーカーなど。6 章）が毎回作り直され、DB などはアプリの環境変数を変えたデプロイでだけ作り直される（手順 6）。作り直しの間に来たリクエストは、Caddy が最長 30 秒待たせ、アプリが応答し始めたら届ける（どのリクエストを待たせられるかは 8 章「再起動中のリクエストの保留」）。他アプリへの間接的な影響は 2 つある。

- ビルド中の CPU・メモリ負荷で他アプリの応答が遅くなりうる（停止はしない）。8.2 のメモリ上限とビルド直列化で抑える。
- Caddy 設定は内容が変わったとき（アプリの作成、ドメイン・ポートの変更、起動・停止、blue/green の切り替え（7.2）など）だけ投入し、通常の再デプロイでは投入しない。投入しても他アプリの SSE と通常のリクエストは切れない。WebSocket などのアップグレード済み接続は 8 章の `stream_close_delay` の猶予のあとに 1 回切れる。

### 7.1 保持しているリリースへの即時ロールバック

koroku は直近のリリースを、ディレクトリ（チェックアウトと override）、リリースごとのイメージ（`koroku-<slug>-<service>:r<id>`）、その時の環境変数のスナップショットごと残している（7 章の失敗時の自動ロールバックのため）。手動のロールバックもこれを使い、取得・ビルド・pull をせずに前の版を起動し直す。小さい VPS ではビルドに数分かかり、障害の最中に待つことになるため。Heroku の `releases:rollback` に相当する。

- **ロールバックもデプロイの 1 件として記録する**。`deployments` に `source_deployment_id`（戻す先のデプロイ）を足す。コミットとメッセージは戻す先のものを写し、`ref` は `rollback` にする。キュー、同時実行 1、キャンセル、ログ、SSE、本番の再ログインは通常のデプロイと同じ。
- **手順**は `prepare`（戻す先のリリースのファイルと環境変数のスナップショットが残っているか確かめる）→ `validate`（7 章の起動前の再検査）→ `start`（スナップショットの環境変数を稼働中の env ファイルに書き、戻す先のリリースのファイルで `up`）→ `route` → `check`。取得、ビルド、pull、ディスク使用率の検査は行わない。取得をしないので、リポジトリの ID の確かめ（5 章）も、コミットが `ref` からたどれるかの確かめ（7 章「デプロイできるコミット」）もない。`start` 以降で失敗したら、通常のデプロイと同じく直前の成功リリースへ自動で戻す。
- **環境変数も戻す**。リリースは「コードとその時の設定の組」なので、戻す先のリリースが動いていた時の値で起動する（失敗時の自動ロールバックと同じ考え方）。今 DB に保存している値とは違いうるので、環境変数の画面には「未反映の変更」として差が出る。指紋（9.2）は戻す先のものを写す。
- **成功したら、アプリの現在のリリース（`current_deployment_id`）は戻す先のデプロイを指す**。ロールバックの記録自体はリリースのファイルを持たないため。起動・停止、次回の失敗時の自動ロールバック、環境変数の差の計算は、そのまま戻す先のリリースを使う。
- **リリースの保持**: 成功後の整理は、成功したデプロイを新しい順にたどり、異なるリリースを N 個（既定 3）残す。ロールバックの記録は、それが指すリリースとして数える。現在のリリースも必ず残す。成功したデプロイ N 件で数えると、ロールバック直後の整理でいま動いているリリースが消え、ロールバックを 2 回続けると戻し先の新しい版も消えてしまうため。
- **戻せる版**: 成功していて、ファイルとスナップショットが残っているもの。ロールバックの記録を指定したときは、その戻す先に読み替える。デプロイの JSON に `rollback_available`（真偽値）と `source_deployment_id` を足し、UI は戻せる版にだけ「この版に戻す」を出す。保持していない古い版は、SHA の再デプロイで戻す。
- **DB のマイグレーションは戻さない**。新しい版で DB を変えていれば、古い版が動かないことがある。UI の確認ダイアログでそのことを伝える。

### 7.2 公開サービスの無停止デプロイ（blue/green）

アプリごとに選ぶと（既定は無効）、デプロイは公開サービスだけを blue/green で入れ替える。新しい版の公開サービスを、動いている古い版と並べて別のポートで起動し、HTTP の確認が通ってから Caddy の転送先を切り替え、古い版を止める。利用者のリクエストは、切り替えの前は古い版に、後は新しい版に届き、その間に断がない。無効のアプリの振る舞いは 7 章のとおりで変わらない。

入れ替えるのは公開サービスだけで、ほかのサービス（DB、キャッシュ、ジョブキューのワーカー、スケジューラー）は 7 章と同じ規則で作り直す（ビルドするワーカーは毎回、DB はアプリの環境変数を変えたデプロイでだけ）。公開サービスを 2 つ並べられるのは、それがリクエストを受けるだけで状態を持たない（持たないことを有効にする条件にする。下記）からで、DB を 2 つ並べることはできない。ワーカーの断は利用者のリクエストには見えないので、作り直しで足りる。

**設定（アプリ単位）**

| 項目 | 内容 |
| --- | --- |
| `blue_green` | 公開サービスを blue/green で入れ替えるか（真偽値、既定は偽） |
| `alt_host_port` | 2 つ目の割当ポート。初めて有効にしたときに、20000 番台から `host_port` と同じ規則で割り当てる（`host_port` と `alt_host_port` を合わせて全アプリで一意）。無効にしても返さない（次に有効にしたときに同じ番号を使う） |
| `web_color` | いま利用者のリクエストを受けている色（`blue` / `green`）。blue は `host_port`、green は `alt_host_port` で待ち受ける。blue/green で一度もデプロイしていないアプリは NULL で、そのとき公開サービスは 7 章のとおり `host_port` で動いている（blue と同じポート） |

- 設定はデプロイタブの「デプロイの設定」（9.1）の「無停止デプロイ」の欄で、`PATCH /api/apps/{slug}` の `blue_green` と、切り替えのあと旧色を止めるまでの待ち時間 `blue_green_drain_seconds`（0〜300 秒、既定 10 秒。長い処理を抱えるリクエストが多いアプリは延ばす）。変えられるのは管理以上のロール（10.7）。再ログイン（10.3）は求めない。別のコードを動かすことにも、秘密やデータを外へ出すことにもつながらないため。
- 本番アプリで有効にするときは、確認ダイアログで次の 2 つを示す。マイグレーションは古い版が動いている間に流れるので、古い版のコードと両立する形（下の「マイグレーションの条件」）で書く必要があること。デプロイの間、公開サービスのメモリが一時的に 2 つ分要ること。無効にするのに確認は要らない。
- 設定の変更は次のデプロイから効く（設定の保存でコンテナを動かさない）。有効にした直後のデプロイが、公開サービスを 7 章の `web` から色の付いたサービスへ移す（下の「有効にする・無効にするときの移行」）。
- 欄には、有効にする条件（下記）と、条件を満たさない Compose ではデプロイが検証の手順で止まることを書き、利用者向けの Skill（`plugins/koroku/skills/prepare-app`）の該当の節を案内する。稼働中のリリースの Compose が条件を満たさないと分かっているときは、欄に警告と理由を出す。判定は、デプロイの手順 3 で（有効かどうかによらず）リポジトリの Compose を正規化した結果に下の「検査」をかけ、リリースの記録（`release-state/<id>/release.json`）に残したもの。アプリの JSON の `blue_green_problems` で返す。設定の画面を開くたびに `config` を走らせないため。公開サービスが `include` した別ファイルにあることは、有効にしてデプロイするまで分からない（下の「検査」）。
- アプリの作成（`POST /api/apps`）では有効にできない。作ったあとに設定で有効にし、そこで 2 つ目のポートを割り当てる。
- レビューアプリ（9.9）は親から写さない（無効で始まる）。レビューの環境に無停止は要らず、2 つ目のポートとメモリを使うだけなので。

**override の形**

公開サービスを `web` とすると、override は次のようになる（本番アプリの例。7 章と同じ項目は省く）。

```yaml
services:
  web:
    profiles: ["koroku-disabled"]   # up では起動しない。run（9.6）とビルドの対象として残す
    image: koroku-app-a-web:r43
    ports: !reset []
    env_file: [/var/lib/koroku/apps/app-a/env, /var/lib/koroku/apps/app-a/env.release]
    mem_limit: "512m"
  web-blue:
    extends: { file: compose.yaml, service: web }
    profiles: !reset []
    build: !reset null
    image: koroku-app-a-web:r43
    pull_policy: never
    ports: !override
      - "127.0.0.1:20001:3000"
    env_file: [/var/lib/koroku/apps/app-a/env, /var/lib/koroku/apps/app-a/env.release]
    restart: unless-stopped
    mem_limit: "512m"
    oom_score_adj: -400
    mem_reservation: "256m"
  web-green:
    # web-blue と同じで、ポートだけが 127.0.0.1:20002:3000
```

- 色の付いたサービス（`<公開サービス>-blue`、`<公開サービス>-green`）は、リポジトリの Compose ファイルの公開サービスを `extends: { file: <Compose ファイル>, service: <公開サービス> }` で写す。`extends` を `file` なしで書くと、同じ override ファイルの中のサービスしか探さず、リポジトリ側の `web` が見えない（`has neither an image nor a build context specified` で失敗する）。`file` を付ければ、`depends_on`（`condition` を含む）、`environment` の補間、`healthcheck` などが写ることを、手元の Compose（v5.8.0）で確かめている。
- 写した定義に override の項目（ポート、env ファイル、メモリ上限・予約、`oom_score_adj`、`restart`）を重ねる。どちらの色も毎回のリリースの override に書く。どちらの色を起動するかは override ではなく、`up` に渡すサービス名で決める（下の「手順」）。ロールバック（7.1）で古いリリースのファイルを使うときも、そのリリースの override で反対の色を起動できるようにするため。
- ビルドは元のサービス（`web`）だけが行う。色の付いたサービスは `build: !reset null` と `pull_policy: never` で、リリースのイメージ（`koroku-<slug>-<サービス>:r<deploy_id>`、6 章）をそのまま使う。両方の色に `build:` を残すと、同じイメージを 2 回ビルドすることになる。`image:` だけの公開サービスは、写した `image:` をそのまま使う（`pull_policy` も書き換えない）。
- 元のサービスは `profiles: ["koroku-disabled"]` で `up` と `start` の対象から外し、ポートを消す。`docker compose build web` と `docker compose run --no-deps web`（9.6）のように名前を明示すると、無効にしたプロファイルのサービスでも使える（手元の Compose で確かめている）。ワンオフのコマンドと定期タスクは、これまでどおり `web` を指定して、同じイメージ・環境変数・メモリ上限のコンテナで動く。
- 無効にしたプロファイルのサービスは、`docker compose config` の出力にも、名前を付けない `build` の対象にも入らない（手元の Compose v5.8.0 で確かめている）。そこで blue/green のリリースの `config`（検証、起動前の再検査、Compose の構成の表示）、`build`、`pull` には `--profile koroku-disabled` を付け、元のサービスを含めて検査し、ビルドする。`up`、`start`、`restart` には付けない。`pull` は `pull_policy: never` の色の付いたサービスを飛ばす（同じく確かめている）ので、リリースのイメージを取りに行かない。
- 色の付いたサービスは、元のサービスが入るネットワークに、元のサービス名を別名（`aliases`）として持つ。ほかのサービスが `http://web:3000` のように公開サービスを名前で呼んでいても届く（結合テストで、DB のコンテナから `http://web:8080/` が色の付いたサービスに届くことを確かめている。本物のホストでも、5 秒ごとに `http://web:3000/up` を呼ぶサービスが、切り替えとロールバックをまたいで 200 を受け取り続ける）。両方の色が動いている間は、名前がどちらにも解決される。公開サービスが `network_mode` を持つときは、ネットワークに入らないので別名を付けない。
- `--remove-orphans` は、プロファイルで外したサービスのコンテナを消さない（手元の Compose で確かめている）。プロファイルを外したサービスは「Compose ファイルにないサービス」ではないため。7 章の `web` から移すときと、色の付いたコンテナを片付けるときは、`docker compose rm --stop --force <サービス>` で明示して消す。

**手順**

7 章の手順 1〜5（受付、取得、生成、検証、ビルド／pull）と、デプロイ前のバックアップは同じ。手順 6 以降を次に置き換える。いま受けている色を「旧色」、反対の色を「新色」と呼ぶ。`web_color` が NULL のアプリの旧色は、`host_port` で動いている `web`（7 章の形）で、新色は green。

| キー | 手順 | 内容 |
| --- | --- | --- |
| `start` | 新色の起動 | 稼働中の env ファイルを新しいリリースの値に書き換え（7 章と同じ位置）、`docker compose up -d --no-deps --wait --wait-timeout 300 <ワンショットのサービス...> <まだコンテナのないサービス...> <新色>`。旧色とほかのサービスには触れない |
| `check` | 新色の確認 | 7 章の手順 8 と同じ HTTP の確認を、新色のポート（`http://127.0.0.1:<新色のポート><ヘルスチェックパス>`）に対して行う |
| `switch` | 切り替え | DB の `web_color` を新色に書き、Caddy の設定を投入する。以後のリクエストは新色に届く |
| `services` | ほかのサービスの作り直し | `docker compose up -d --no-deps --wait --wait-timeout 300 <公開サービスとワンショット以外のサービス...>`。設定かイメージが変わったサービス（7 章と同じ規則。ビルドするワーカーは毎回、DB はアプリの環境変数を変えたときだけ）を作り直す |
| `drain` | 旧色の片付け | 切り替えのあとの待ち時間（アプリの設定 `blue_green_drain_seconds`。既定 10 秒）だけ待ってから、旧色を `docker compose stop` し（猶予は旧色のコンテナの `stop_grace_period`）、`docker compose rm --force` で消す。最後に `--remove-orphans` 相当の片付け（Compose ファイルから消えたサービスのコンテナを消す。新しいリリースのファイルで `up -d --no-deps --remove-orphans --wait <新色>` を行う。新色は変わっていないので作り直されない）を行う |
| `done` | 完了 | 7 章の手順 9 と同じ |

- **起動時の後始末と同じ片付け**: 手順 `start` の最初に、`web_color` でない色のコンテナが残っていれば消す（下の「起動時の後始末」）。前のデプロイが中断して残した新色が、ポートを握っているため。
- **新色の起動に一緒に渡すサービス**: ワンショットのサービス（ほかのサービスが `condition: service_completed_successfully` で待つもの。6.2 と同じ判定）と、新しいリリースで初めて現れ、まだコンテナのないサービス（このコミットで足した Redis など）。`--no-deps` を付けても、並べたサービスの間の依存の順序は守られ、新色はマイグレーションの完了を待ってから起動する（手元の Compose で、`up --no-deps <ワンショット> <新色>` がワンショットを作り直して終わるのを待ち、それから新色を起動し、旧色とワーカーに触れないことを確かめている）。並べないサービス（DB など）は作り直さない。`--no-deps` を付けないと、アプリの環境変数を変えたデプロイで、新色が依存する DB も切り替えの前に作り直され、旧色の DB が止まる。
- **ワーカーを切り替えのあとに作り直す理由**: 新色が確認に通らなかったとき、ワーカーやスケジューラーに触れていなければ、旧色とそれらの組がそのまま動き続け、戻す操作が要らない。切り替えの前に作り直すと、確認に失敗したときに古い版でもう一度作り直すことになる。切り替えから作り直しまでの数秒〜数十秒は、新しい版の公開サービスと古い版のワーカーが並ぶ。新しい版がキューに入れたジョブを古い版のワーカーが受けることがあるので、ジョブの引数の形も、マイグレーションと同じく前後の版で両立させる（下の「マイグレーションの条件」）。
- **DB などの作り直し**: 手順 `services` は 7 章と同じ規則で作り直す。コミットの変数は blue/green かどうかによらず `env.release` に分かれていて、色の付いたサービス、元の公開サービス、ワンショット、ビルドするサービス、定義がコミットに従うサービスにだけ渡る（6 章「コミットの変数を渡すサービス」）。DB はアプリの環境変数を変えたデプロイでだけ作り直され、その数秒は新色も DB につながらない。そのときは手順 `services` に 6 章の `Recreating <サービス> because the app's env file changed (...)` を書く。補間には `--env-file` を 2 つ（`env` と `env.release`）渡す（7 章の形のアプリと同じ）。結合テストで、blue/green を有効にした最初のデプロイと、コードだけを変えたデプロイの前後で、DB のコンテナの作成時刻が変わらないことを確かめている。
- **切り替えの順序**: DB に書いてから Caddy に投入する。投入に失敗したら DB を旧色に戻し、デプロイを失敗にする（下の「失敗したとき」）。DB を正とし、Caddy の設定は常に DB から作る（8 章）ので、投入の失敗のあとで 5 分ごとの投入が誤った色を入れることはない。
- **他アプリへの影響**: 切り替えは Caddy の設定の投入で、通常の再デプロイでも 1 回入る（7 章では内容が変わらないので投入しない）。投入しても他アプリの SSE と通常のリクエストは切れず、アップグレード済みの接続（WebSocket）は `stream_close_delay`（5 分）の猶予のあとに 1 回切れる（8 章で確かめた事実）。blue/green のアプリのデプロイのたびに、他アプリの WebSocket が 5 分後に 1 回切れることになる。
- **切り替えたアプリ自身の接続**: 投入の時点で旧色に流れているリクエストは、Caddy の古いサーバーが最後まで旧色へ中継する（他アプリの SSE が投入の間に欠けないのと同じ仕組み）。新しいリクエストは新しい設定で新色へ行く。結合テストで、起動に 2 秒かかる公開サービスと DB、ワンショットのマイグレーションを持つアプリを Caddy 2.11 の前に置き、0.2 秒ごとの `GET` を送りながら、コードだけのデプロイ、足すだけのマイグレーションを含むデプロイ、起動しないコミットのデプロイ、即時ロールバック、デプロイ中の `koroku` の再起動を続けて行い、5xx と接続エラーが 0 件であることを確かめている（約 530 件）。本物のホスト（TLS と HTTP/2 で外から届くリクエスト、Rails と PostgreSQL のアプリ）でも、外から 0.2 秒ごとに `GET` と `POST` を送りながら同じ操作を行い、切り替えをまたいで `POST` も失敗しない。ただし、投入の瞬間に張られかけた外からの接続が、応答もなく Caddy のエラーログにも残らずに切れることがまれにある（7 回の切り替えのうち 1 回、1 件）。Caddy が投入で古いサーバーを閉じ新しいサーバーに受け付けを移すときの、接続の受け付けの競合と見ている。アプリに届く前に切れるので、リクエストが 2 回処理されることはなく、クライアントが送り直せば届く。`drain` の待ち時間は、投入の直前に受け付けたリクエストが旧色に届き終わるまでの余白で、旧色の停止は `stop_grace_period` の間、処理中のリクエストを終えるのを待つ（Puma などは `SIGTERM` で新しい接続を断り、処理中のものを終える）。旧色に張られていた WebSocket と SSE は、旧色を止めたときに切れる。
- **ログの見出し**: 表の `start` は `Starting <新色> on port <ポート>`、`switch` は `Switching traffic from <旧色> to <新色>`、`drain` は `Stopping <旧色>` を表題にする。`check` と `done` は 7 章と同じ。新しい手順のキー `switch`、`services`、`drain` は、管理 UI の手順表示（9.1）と `docs/API.md` の UI が解釈する行に加える。

**ホストのメモリが足りないとき**

新色と旧色が並ぶ間、公開サービスのメモリは最大で上限 2 つ分になる（512m の上限なら、一時的に 512m 増えうる）。手順 `start` の直前に、`/proc/meminfo` の `MemAvailable` と `SwapFree` の合計が新色のメモリ上限に満たなければ、新色を起動せずにデプロイを `failed` にする。旧色、DB、ワーカーには触れていないので、アプリは動き続ける。

- デプロイログに `Not enough memory to run two <公開サービス> containers (<空き> available, <上限> needed).`（値は MiB 単位）と書き、失敗の説明文にも同じ理由を残す。上限は新しいリリースの新色のメモリ上限（override の値。なければリポジトリ自身の値）。`/proc/meminfo` が読めなければ、ログに書いて起動を続ける。履歴に「メモリが足りないため無停止で入れ替えられません」と出す。
- 並べずにその場で作り直す形に切り替えないのは、無停止を選んだアプリで、気づかないうちに断が起きるのを避けるため。メモリを空ける（ほかのアプリを止める・休止させる、上限を下げる）か、無停止デプロイを外してからデプロイし直す。上限の 1 つ分を見るのは、新色が起動直後から上限まで使うとは限らないが、それを当てにすると OOM の順序（6.2）で本番の別のプロセスが止まりうるため。
- 本番のメモリの予約（6.2）は両方の色に付く。並ぶ間は予約も 2 つ分になるが、数十秒のことなので、システム画面の予約の合計（稼働中のリリースの記録。サービス名は元の公開サービスで記録する）には数えない。
- リソースタブの「メモリの上限と予約」の節と、デプロイの設定の「無停止デプロイ」の欄に、デプロイの間は公開サービスの上限 1 つ分が余分に要ることを書く。

**失敗したとき**

- 手順 `start` か `check` で失敗したら、新色を `docker compose rm --stop --force` で消し、稼働中の env ファイルを稼働中のリリースのスナップショット（`release-state/<id>/env`、7 章）に書き戻して、デプロイを `failed` にする。旧色、DB、ワーカーには触れていないので、ロールバックは要らない。`failed` の説明文（5.1）に `; the previous release was restored` は付けない（戻していない。もともと動き続けている）。ワンショットのサービス（マイグレーション）は流れたあとかもしれない（下の「マイグレーションの条件」）。
- 手順 `switch` で Caddy への投入に失敗したら、`web_color` を旧色に戻して同じく新色を消す。
- 切り替えたあとで旧色に戻したデプロイは、`deployments.switched_at` を NULL に戻す（履歴に「切り替え」を出さない）。
- 手順 `services` で失敗したら（ワーカーが起動しないなど）、`web_color` を旧色に戻して Caddy に投入し（旧色はまだ動いている）、稼働中のリリースのファイルとスナップショットで `docker compose up -d --no-deps --wait <公開サービスとワンショット以外のサービス...>` を行ってワーカーなどを古い版に戻し、新色を消す。7 章の自動ロールバックに当たり、説明文に `; the previous release was restored` を付ける。
- 手順 `drain` の失敗（旧色が止まらない）は、デプロイを `succeeded` のままにしてログに残す。リクエストはすでに新色に届いていて、旧色は次のデプロイの `start` の前に消す（下の「起動時の後始末」）。
- 7 章の「失敗時」の `docker compose logs --tail 30` は、新色と、作り直したサービスについて書く。
- **キャンセル**（7 章の取り消しと `koroku` の停止を除く）は、その時点の手順の失敗と同じに扱う。`switch` より前なら新色を消すだけ、`services` の途中なら旧色に戻す。`drain` の待ち時間の途中のキャンセルは待ちを打ち切って旧色を止め、デプロイは `succeeded` のまま（リクエストはすでに新色に届いている）。
- `koroku` の停止と重なったときは、新色を消すことも旧色に戻すこともしない（7 章と同じく停止処理の中では戻さない）。DB の `web_color` のとおりに経路が残り、下の「起動時の後始末」がもう一方の色を消す。

**`koroku` の再起動をまたぐ**

- 実行中に `koroku` が止まったデプロイは、7 章と同じく `failed (interrupted)` にする。DB の `web_color` が、Caddy に入れるべき色を決めている（起動時の投入も DB から作る）。
- **起動時の後始末**: 起動時（中断したデプロイを `failed` にしたあと）と、blue/green のデプロイの手順 `start` の前に、`web_color` でない色のコンテナが残っていれば `docker compose rm --stop --force` で消す（`web_color` が NULL なら両方の色）。対象は、blue/green が有効か、色を持つか、2 つ目のポートを持ったことのあるアプリ。切り替えの前に止まったなら新色が、切り替えのあとに止まったなら旧色が残っていて、どちらも利用者のリクエストを受けていない。ワーカーなどが古い版のままか新しい版になっているかは、7 章の中断と同じく次のデプロイがそろえる。
- 後始末は、そのアプリの操作ロック（10 章）の中で行う。

**有効にする・無効にするときの移行**

- 有効にして最初のデプロイでは、旧色は 7 章の形の `web`（`host_port`）で、新色を green（`alt_host_port`）にする。`drain` で `web` を `docker compose rm --stop --force web` で消す（`--remove-orphans` では消えないため。上記）。以後は blue と green を交互に使う。
- 有効にするときも無効にするときも、公開サービス以外のサービスに渡る env は変わらない（コミットの変数を `env.release` に分ける規則が、7 章の形のアプリと同じなので。6 章）。アプリの環境変数を変えていなければ、切り替えのデプロイで DB は作り直されない。
- 旧色が動いていない（初回のデプロイ、停止中、休止中（8.4）、ホストの再起動のあとで起動していない）ときは、並べる相手がいないので、`web_color` の色（NULL なら blue）のまま 7 章の手順 6 と同じく全サービスを `up` する（色の付いたサービスのうち、その色だけを並べる）。切り替えはせず、断を避ける相手もいない。
- 無効にしたあとの最初のデプロイは、7 章の形に戻す。手順 6 の `up` の前に、色の付いたコンテナを `docker compose rm --stop --force` で消す（`web` が `host_port` を使うので、green が動いていなくても blue が `host_port` を握っている）。この 1 回は数秒の断がある。`web_color` は NULL に戻す。

**ほかの操作との関係**

- **即時ロールバック（7.1）**: 戻す先のリリースのファイルとスナップショットで、上の手順を `start` から行う（`prepare` と `validate` は 7.1 のとおり）。戻す先の override にも両方の色があるので、反対の色で起動できる。ワンショットのサービスは 7.1 と同じく流れる。DB のマイグレーションを戻さないことは 7.1 と同じ。戻す先のリリースが blue/green を有効にする前のもの（override に色の付いたサービスがない）なら、7 章の形の `up` で戻す（数秒の断がある。ロールバックの確認ダイアログにそう出す）。
- **昇格（9.7）**: 通常のデプロイなので、本番アプリで有効なら blue/green で出る。
- **自動ロールバック（7 章）**: 上の「失敗したとき」に置き換わる。
- **起動・停止・再起動（10 章）**: 停止は `docker compose stop`（全サービス）。起動は `web_color` の色と公開サービス以外のサービスを名前で並べた `up -d --wait`。再起動は同じ並びの `docker compose restart` で、数秒の断がある（無停止にするのはデプロイとロールバックだけ）。名前を並べるのは、`up` と `start` にサービスを渡さないと両方の色を起動しようとし、`start` はコンテナのない色を `missing dependency` として失敗するため（手元の Compose で、`start --wait` が終了コード 1 になることを確かめている）。並べる名前は、リリースの記録に残した正規化後のサービス名から、元の公開サービスと `web_color` でない色を除いたもの。`start` と `restart` はプロジェクト名だけ（`-f` なし）でサービス名を渡しても動く（同じく確かめている）。起動の `up` は、もう一方の形の公開サービス（元のサービスと反対の色、または 7 章の形のアプリでは色の付いたサービス）のコンテナを先に消す。
- **アクセスがない間の休止（8.4）**: 休止は `docker compose stop` のままでよい。起こす処理の `start --wait` には、`web_color` の色と公開サービス以外のサービスを名前で渡す（同じ理由）。起こす経路の転送先とヘルスチェックは、`web_color` の色のポート。
- **アクセスの保護（8.3）、独自ドメイン（8.1）、プライマリへのリダイレクト**: 変わらない。Caddy のルートのうち、最後の転送先のポートだけが色で変わる。休止中の起こす経路で `koroku` が転送する先も同じ。
- **ワンオフのコマンドと定期タスク（9.6）**: サービスに公開サービスの名前（`web`）を指定すれば、プロファイルで外した元のサービスで `run --rm --no-deps` する（上の「override の形」）。色の付いたサービスの名前は、タスクのサービスの選択肢に出さず、API も断る（`422 validation_failed`）。休止中の定期タスクの依存先の起動（9.6）は、公開サービスに依存するタスクがないので変わらない。
- **バックアップのダンプフックと DB のコピー（10.2、10.6）**: フックやコピー後のコマンドのサービスが公開サービスなら、`exec` は `web_color` の色のコンテナで行う。DB のコピーの `stop` と `start` の手順は、公開サービスを `web_color` の色に読み替える。
- **ログの収集（10.1）**: `docker compose logs` は色の付いたサービスを `web-blue-1` のように出す。コレクタは、サービス名が `<公開サービス>-blue` か `<公開サービス>-green` の行を公開サービスの名前に読み替えて記録する（ログのファイル、検索、退避のキーは `web` のまま）。並ぶ間は 2 つのコンテナの行が時刻順に混ざる。行の本文には色を足さない（デプロイログの `switch` の時刻で見分けられる）。
- **メモリの履歴（9.5）とリソースの内訳（9.3）**: 同じく色の付いたサービスを公開サービスに読み替える。並ぶ間は、9.5 の「同じサービスのコンテナが複数あるときは一番大きい値」の規則がそのまま効く。色が変わると新しいコンテナになるので、再起動の回数は数えない（9.5 と同じ）。
- **概要とリソースのタブ、`GET /api/apps/{slug}/services`**: 公開サービスを 1 行で出し、いまの色を添える（`color`）。並んでいる間（デプロイの `start` から `drain` まで）は、両方の色のコンテナを行の中に出す。
- **Compose の構成（9.10）**: override と重ねた結果は、両方の色と、プロファイルで外した元のサービスを含んだまま出す。コンテナの照合では、`web_color` の色のコンテナを稼働中のリリースの色の付いたサービスと比べる。元のサービスにコンテナがないことは食い違いにしない。`web_color` でない色のコンテナが、デプロイが積まれても動いてもいないときに残っていれば `orphan` にする（起動時の後始末で消えるはずのもの）。
- **ポリシー検査（6 章）**: ポート公開の検査は、両方の色がそれぞれの割当ポートだけに公開し、元のサービスが何も公開していないことを確かめる。猶予の上限（6 章）の合計は、公開サービスを 1 つとして数える（旧色の停止は `drain` で単独に行うため）。
- **レビューアプリ（9.9）**: 上記のとおり写さない。
- **GitHub Deployments（5.1）**: 変わらない。`success` は `done` で送る。
- **deploy-status Skill**: デプロイの手順のキー（`switch`、`services`、`drain`）を読めるようにする。

**有効にする条件と検査**

blue/green のアプリのデプロイは、手順 4（検証）で次を確かめ、満たさなければ `failed` にする（ルール `blue_green`。「検査を緩める」でも許可しない。満たさないまま並べると、壊れ方がアプリの外から見えないため）。設定の欄も、稼働中のリリースについて同じ判定を出す。

| 条件 | 理由 |
| --- | --- |
| 公開サービスに `container_name` がない | 2 つ並べると名前が衝突する |
| 公開サービスがワンショットでなく、`deploy.replicas` が 1 以下 | 並べる単位が 1 コンテナであること |
| ほかのサービスが公開サービスに `depends_on` していない、`network_mode` / `ipc` / `pid` / `volumes_from` の `service:<公開サービス>` で参照していない | 元のサービスはプロファイルで外すので、依存の先がなくなる |
| `<公開サービス>-blue`、`<公開サービス>-green` という名前のサービスがない | 色の付いたサービスと衝突する |
| 公開サービスが Compose ファイル（`compose_path`）の中で定義されている（`include` した別ファイルにない） | `extends: { file: }` で写すため。写せないと `config` が `cannot extend service "web-blue" in ...: service "web" not found in compose.yaml` で失敗する（手元の Compose で確かめている）。この出力のときは、エラーをデプロイログに出したうえでルール `blue_green` の違反にする |

koroku が確かめられないが、満たさないと壊れるもの（Skill と設定の欄で案内する）:

- **公開サービスが状態を持たない**。セッションは Cookie か DB・Redis に置く。アップロードしたファイルは named volume（両方の色が同じ volume をマウントする）かオブジェクトストレージに置く。公開サービスのコンテナの中の SQLite に書くアプリは、並ぶ間に 2 つのプロセスが同じファイルに書き、片方は古い版のコードで書く。
- **同時に 2 つ動くと困る処理を公開サービスの中で動かさない**。プロセス内の cron（`rufus-scheduler` など）、起動時に 1 回だけ行う前提の処理は、並ぶ間に 2 回走る。ジョブキューのワーカーを Puma の中で動かす形（Solid Queue の `plugin :solid_queue`、Sidekiq の埋め込み）は、キューの側で排他されるので 2 つ並んでも壊れないが、その処理は旧色の停止で `stop_grace_period` の間に終わる必要がある。
- **ヘルスチェックのパスが速く、準備ができてから 5xx 以外を返す**。koroku は新色の確認が通った時点で切り替える。準備の前に 200 を返すと、切り替えの直後のリクエストが失敗する。
- **マイグレーションの条件**（下）。

**マイグレーションの条件（expand / contract）**

マイグレーションは、7 章と同じく Compose 側のワンショットのサービス（か起動コマンド）で、新色の起動の前に流れる。そのときも、切り替えまでの間も、旧色（古い版のコード）が利用者のリクエストを受けている。そこで、アプリは次を守る。

- **1 回のデプロイのマイグレーションは、古い版のコードで動く形にする**。足す（テーブル、NULL を許すか既定値のあるカラム、インデックス）のは 1 回でできる。消す・名前を変える・型を変える・NULL を禁じるのは 2 回に分ける。1 回目でコードがそのカラムを使わないようにし（Rails なら `ignored_columns`）、2 回目のデプロイで消す。名前の変更は、新しいカラムを足して両方に書き、読み替えてから古いものを消す。
- **キューに入れたジョブの引数も、前後の版で読める形にする**（上の「ワーカーを切り替えのあとに作り直す理由」）。
- 守らないと、マイグレーションが流れてから切り替えまで（新色の起動と確認の時間。数秒〜数十秒）、旧色がエラーを返す。新色が確認に通らなければ、旧色は新しいスキーマのまま動き続け、直すまでエラーを返し続ける（マイグレーションは戻さない。7.1 と同じ）。7 章の作り直しで同じマイグレーションを流したときより悪くなりうるので、守れないデプロイでは一時的に無効にしてからデプロイする、と Skill と設定の欄に書く。
- koroku はマイグレーションの中身を見ない（ORM も DB の種類も知らない。7 章）。伝える手段は、利用者向けの Skill（`plugins/koroku/skills/prepare-app` の要件の節）と、設定の欄・本番で有効にするときの確認ダイアログの説明。

**無停止にならないもの**

次は受け入れる。どれも、公開サービスを並べる仕組みでは避けられないか、避けるのに見合わない。

- **公開サービスへの長い接続**: WebSocket と SSE は旧色を止めたときに切れる。クライアントの再接続に任せる（8 章の `stream_close_delay` と同じ考え方）。
- **他アプリの WebSocket**: 切り替えの投入で、`stream_close_delay` のあとに 1 回切れる（上記）。
- **ワーカー、スケジューラー、DB**: 7 章と同じ規則で作り直す。ワーカーはジョブの合間に止まるだけで、利用者のリクエストには見えない。DB はアプリの環境変数を変えたデプロイでだけ作り直され、その数秒は新色もエラーを返しうる。
- **両立しないマイグレーション**: 上記。
- **メモリが足りないときのデプロイ**: 上記のとおり失敗にする。
- **再起動、起動、停止、ホストの再起動**: 断がある。上流が再起動している間リクエストを待たせて送り直す Caddy の設定（8 章）が、その断を短い待ちに変える。blue/green はこの設定と組み合わせて使い、切り替えの後の転送先にも同じ設定が付く。切り替えは転送先を変えるだけなので、待たせる設定が働くのは、新色が切り替えの直後に落ちたときと、上の断のあるときだけになる。
- **複数のコンテナへの負荷分散とオートスケール**: しない（1 章）。並ぶのはデプロイの間だけで、ふだんは公開サービスのコンテナは 1 つ。

**データ**: `apps` に `blue_green`（0 / 1）、`blue_green_drain_seconds`（切り替えのあと旧色を止めるまでの待ち時間。0〜300 秒、既定 10）、`alt_host_port`（NULL 可、`host_port` と合わせて一意）、`web_color`（NULL / `blue` / `green`）。`deployments` に `web_color`（そのデプロイが起動した色。7 章の形なら NULL）、`switched_at`（切り替えた時刻）。デプロイの履歴と詳細に、色と切り替えた時刻（「12:03:41 に green へ切り替え」）を出す。

**採らない方式**

- **プロジェクトごとの blue/green**（Compose プロジェクトを 2 つ持ち、全サービスを並べる）: DB とその volume も 2 つになり、データが分かれる。DB を外へ出すと「1 アプリ = 1 Compose プロジェクト」（2 章）が崩れ、メモリも全サービス分が 2 倍になる。
- **公開サービスを `--scale web=2` で 2 つにする**（`docker rollout` などの方式）: 1 つのサービスのコンテナは同じ定義なので、`--no-recreate` で古いコンテナを残して新しいコンテナを足す、という Compose の振る舞いに頼ることになる。縮めるときにどのコンテナを消すかも Compose の内部の順序（古い設定のものから）に頼る。ポートは 1 つのサービスに 1 つしか固定できず、範囲で公開するとどちらのコンテナがどのポートかを毎回 inspect で調べることになる。Caddy をコンテナのネットワークに入れてコンテナの IP へ転送する方法は、Caddy をホストに直置きしてループバックの割当ポートへ転送する構成（2 章）と合わない。色の付いたサービスなら、どれも Compose の明示的な指定（サービス名、固定のポート）だけで決まり、DB から Caddy の設定が決まる。
- **Docker Swarm のローリング更新（`order: start-first`）、Kubernetes（k3s など）、Nomad**: 無停止の入れ替えはそのまま手に入るが、Swarm の `docker stack deploy` は `build:` を扱わずレジストリを前提にし、Compose の書式の一部（`depends_on` の条件など）を無視する。k3s と Nomad は常駐のメモリだけで 4GB のホストの相当部分を使い、Compose のリポジトリをそのまま動かすという前提（1 章）から外れる。
- **Caddy の負荷分散に両方のポートを並べ、つながる方へ送る**（受動的なヘルスチェック）: 切り替えの投入が要らないが、どちらへ送るかが Caddy のメモリの状態で決まり、DB から決まらない（8.4 の不採用案と同じ理由）。確認の通っていない新色にリクエストが届きうる。
- **公開サービスの再起動の間 Caddy で待たせるだけにする**（8 章の待たせる設定）: 単独で数秒の断を待ちに変えられ、全アプリに効くので、こちらも使う。ただ、待つ間は応答が止まり、新しい版が起動しなければ待った末にエラーになる。blue/green は確認に通るまで古い版が応え続けるので、起動に失敗する版を出したときにも利用者に見えない。

## 8. ルーティングと TLS

DNS にワイルドカードの A レコードを 1 本置き、証明書は Caddy がホスト名ごとに自動取得する。

- **DNS**: `*.dev.example.com` を VPS の IP に向ける。アプリは `<slug>.dev.example.com`、管理 UI は `console.dev.example.com`。
- **証明書**: Caddy は設定にホスト名があると自動 HTTPS を有効にし、証明書の取得・更新と HTTP→HTTPS リダイレクトを行う（[Automatic HTTPS](https://caddyserver.com/docs/automatic-https)）。
- **発行上限**: Let's Encrypt は登録ドメインあたり 7 日間で 50 枚まで（[Rate Limits](https://letsencrypt.org/docs/rate-limits/)）。3〜5 アプリでは問題ないが、開発中は staging 環境を使う。
- **設定投入**: `koroku` が DB から Caddy の JSON 設定全体を生成し、admin API の `POST /load` で置き換える（[API Tutorial](https://caddyserver.com/docs/api-tutorial)）。差分更新はしない。`koroku` 起動時にも必ず実行し、DB を唯一の正とする。
- **停止中アプリ**: ルートは残し、503 の固定ページを返す。
- **休止中アプリ**: アクセスがない間に止めたアプリ（8.4）。ルートは残し、転送先を `koroku` の起こす処理にする。
- **静的ファイル**: Caddy はアプリのファイルを配らず、アプリのホスト名へのリクエストはすべて公開サービスへ転送する。静的ファイルはアプリのプロセスが配る（利用者向けの Skill がそう案内する）。Caddy に配らせると常駐のプロセスは増えないが、ファイルはイメージの中にあり、Caddy のユーザーは `koroku` のデータディレクトリ（root 専用）を読めない。リリースごとにイメージから取り出して置き、ロールバックのために残し、無停止デプロイ（7.2）の切り替えの間は新旧の版を両方置き、アプリが置いたシンボリックリンクで Caddy のユーザーが読めるホストのファイル（証明書の秘密鍵など）が出ないようにする仕組みが要る。アプリのプロセスが配る形でも、ファイルの中身はカーネルのページキャッシュに載るだけでプロセスごとには増えず、浮くのは主にアプリのワーカーの時間なので、この仕組みは持たない。

**Caddy 設定の細則**

- サーバーは `:80` 用と `:443` 用の 2 つに分ける。1 つのサーバーに両方のポートを持たせると、Caddy が自動リダイレクトを挿入する位置がホスト別ルートの後ろになり、`http://<slug>.<base>` へのアクセスが平文のままアプリへ転送されてしまう。この構成では HTTP は 308 で HTTPS へ、未知のホスト名は `:80` で 404、`:443` では TLS ハンドシェイク失敗になる。
- 一度もデプロイしていないアプリも、停止中アプリと同じ 503 の固定ページにする（転送先がまだないため）。
- 設定の投入は、DB から生成した内容が前回投入したものと同じなら省く。これで通常の再デプロイでは Caddy に触れない。`koroku` 起動時だけは必ず投入する（Caddy 側は同一内容なら `config is unchanged` として何もしない）。投入に失敗したら次回必ず投入し直し、5 分ごとの定期処理でも再試行するので、Caddy が後から起動しても追いつく。
- アプリへの `reverse_proxy` には `stream_close_delay: 5m` を付ける。Caddy は設定を入れ替えるとサーバーを作り直し、既定ではその瞬間に、他アプリを含む WebSocket などのアップグレード済み接続を切る。この指定は切断をなくすのではなく先送りするもので、アプリの作成・ドメインやポートの変更・起動や停止のたびに、他アプリの WebSocket は最長 5 分後に 1 回切れる（クライアントの再接続に任せる）。SSE と通常のリクエストは、平文でも TLS + HTTP/2 でも、入れ替えの間に欠けない。
- 生成する設定には Caddy 2.8 以上が要る（`stream_close_delay` は 2.7 から、アクセスログの `log_append` は 2.8 から。2.7 は `log_append` を知らず、設定全体の投入を拒む）。Ubuntu 標準の apt パッケージではなく Caddy 公式リポジトリのものを入れる（プロビジョニングスクリプトで対応）。
- アクセスログはアプリごとに `<access_log_dir>/<slug>.log`（既定 `/var/log/caddy/apps`）へ出す。`koroku` のデータディレクトリは root 専用（0700）で Caddy のユーザーが書けないため、置き場所を分ける。ロガー名は `http.log.access.<slug>.app` とする。Caddy はロガー名を前方一致で選ぶので、slug そのままだと `shop-staging` のログが `shop.log` にも入ってしまう。管理 UI と未知のホスト名のアクセスログは出さない。中身は下の「アクセスログの内容」。
- Caddy の admin API は認証がなく設定全体を書き換えられるので、`caddy.admin_url` はループバックアドレスしか受け付けない。
- `koroku` がデプロイの成功を記録したあとにも、もう一度設定を同期する。ルートの更新（手順 7）はデプロイが「現在のリリース」になる前に走るので、そこだけでは初回デプロイ（と停止中からのデプロイ）のアプリが、5 分ごとの定期処理まで 503 の固定ページのまま残ってしまう。

**アクセスログの内容**

アクセスログは 1 リクエスト 1 行の JSON で、リクエストとレスポンスのヘッダーを入れない。アプリのホスト名へのリクエストは、停止中の 503 やゲート（8.3）の 302 も含めて、ログインしていない相手のものまですべて記録される。ヘッダーを残すと、相手は 1 リクエストあたり約 20 KB（Caddy が受け付けるリクエスト行とヘッダーの上限）までの行を好きなだけ書かせられ、全アプリが共有するディスクを埋められる。ヘッダーには `Cookie` や `Authorization` も入る。

| 残すもの | 内容 |
| --- | --- |
| `ts` | 時刻（Unix 秒、小数） |
| `request` | `remote_ip`・`remote_port`・`client_ip`、`proto`、`method`、`host`、`uri`（クエリ文字列を含む） |
| `status`、`duration`、`size`、`bytes_read` | 応答のステータス、かかった秒数、応答の大きさ、読んだリクエスト本文の大きさ |
| `user_agent` | リクエストの `User-Agent` |
| `location` | 応答の `Location`（リダイレクト先。ないときは空文字） |
| `level`、`logger`、`msg`、`user_id` | Caddy が付ける固定の項目 |

- 残さないもの: リクエストヘッダー（`Cookie`、`Authorization`、`Referer` など）、レスポンスヘッダー（`Set-Cookie` など）、TLS の詳細（`request.tls`）。`uri` のクエリ文字列と `location` は残るので、URL に秘密を載せるアプリではログに入る。
- Caddy のログのエンコーダーを `filter`（中は `json`）にし、`request>headers`・`request>tls`・`resp_headers` を `delete` で落とす。`filter` には「これだけ残す」指定がないので、残したい 2 つのヘッダーは、アプリのホスト名に一致する先頭のルート（終端にしない）に置いた `log_append` で `user_agent` と `location` という項目として足す。`log_append` は後ろのハンドラーが応答したあとに値を読むので、`Location` も取れる。
- 相手が決められる項目は、`regexp` のフィルターで長さを切り、切ったところに `...` を付ける: `method` 32、`host` 300、`uri` 1000、`user_agent` 200、`location` 500 バイト。印字できる ASCII 以外のバイトと `"`・`\` が出たら、そこで切る（JSON にすると 1 文字が最大 6 バイトに膨らむため。正しくエンコードされた URL とふつうのヘッダーの値には出てこない）。これで 1 行は通常 400〜500 バイト、どう作っても 2.5 KB ほどに収まり、コレクタが 1 行として持つ 16 KiB（10.1）で途中から切れることがない。
- 休止の見張り（8.4）は `status` と `location` を読む。画面と `deploy-status` の Skill は行をそのまま表示する。
- Caddy 自身のログ（journald）に出るエラー（転送先につながらない 502 など）は、この設定の対象外で、Caddy の既定のまま（リクエストヘッダーが入り、`Cookie` の値は Caddy が伏せる）。
- 実際の Caddy に生成した設定を読ませ、ヘッダーの値が行に入らないこと、上限いっぱいのリクエストでも行が収まること、休止の見張りがその行を読み分けることを結合テストで確かめる。

**再起動中のリクエストの保留**

デプロイ（7 章の手順 6）、ロールバック、再起動でアプリのコンテナを作り直す間、割当ポートの先ではアプリが応答しない。そのまま転送すると利用者には 502 が返るので、アプリへの `reverse_proxy` に `load_balancing` の `try_duration: 30s` と `try_interval: 250ms` を付ける。Caddy は転送に失敗したリクエストを手元に留め、250 ms ごとにつなぎ直す。短い作り直しは、エラーではなく遅い応答になる。

- つなぎ直すのは、リクエストを 2 回送っても困らないときだけ。Caddy は、上流への接続そのものに失敗した（`connection refused`）ときは、メソッドによらずつなぎ直す。リクエストはまだ 1 バイトも送っていないので、本文も読んでおらず、つなぎ直した先にそのまま送れる。接続したあとに失敗したときは、Caddy の既定どおり `GET` だけをつなぎ直す（`retry_match` は使わない）。アプリが受け取ったかもしれない `POST` などを 2 回送らないため。
- Docker は割当ポートを `docker-proxy`（userland proxy、Docker の既定）で受ける。`docker-proxy` はコンテナが動いている間ずっと接続を受け付け、コンテナの中でアプリが待ち受けていなければ、受け付けた接続を閉じる。Caddy にはこれが「接続したあとの失敗」に見える。そのため、作り直しの間の振る舞いは段階で分かれる。

| 段階 | 割当ポート | 本文のない `GET` | ほかのメソッド |
| --- | --- | --- | --- |
| 古いコンテナが止まるまで（終了処理でアプリが待ち受けをやめたあと） | `docker-proxy` が受けて閉じる | 新しい版が応答するまで待つ | 502 |
| 古いコンテナが消えてから新しいコンテナが動くまで | 誰も待ち受けない | 新しい版が応答するまで待つ | 待つが、次の段階に入ると 502 |
| 新しいコンテナの中でアプリが起動している間 | `docker-proxy` が受けて閉じる | 新しい版が応答するまで待つ | 502 |
| アプリが待ち受けたあと | アプリ | 応答 | 応答 |

- 手元の Docker（`docker-proxy` あり）と Caddy 2.11 で、待ち受けまで 5 秒かかるアプリを `up` で作り直しながら 200 ms ごとに `GET` と `POST` を送って確かめた。保留なしでは作り直しの間の約 6.5 秒、どちらも 502 になる。保留ありでは `GET` はすべて新しい版の 200 になり（最も長いもので 7.5 秒待った）、`POST` は 502 のまま（コンテナがない間に来たものは、新しいコンテナが動き出すまで待ってから 502）。Rails のような起動の遅いアプリでは 3 段目が長いので、作り直しの間に救えるのは実質的に `GET`、つまりページの表示と読み込みになる。フォームの送信や API の更新は、これまでどおり 502 になりうる。
- 待つのは最長 30 秒。Rails の起動（5〜15 秒ほど。8.4）と停止の猶予の既定（10 秒。6 章）を足しても収まり、ブラウザーの待ち時間（数分）より短い。作り直しではなくアプリが落ちたまま（止まっている、起動に失敗し続けている）のときは、30 秒待ってから 502 を返す（手元で確かめた。保留がなければすぐに 502）。上限があるので、待たせるリクエストが溜まり続けることはない。待つ間の Caddy の負荷は、250 ms ごとのループバックへの接続の試みだけ。
- Caddy の設定を投入すると、その時点で待たせていたリクエストは 502 で終わる（手元で確かめた）。通常の再デプロイでは設定を投入しない（上記）ので重ならないが、作り直しの最中にほかのアプリの操作で投入が起きると、待っていたリクエストはそこで失敗する。
- 本文のある `GET` は、接続したあとに失敗すると、Caddy が読み終えた本文を送り直せず（`ContentLength=… with Body length 0`）、30 秒つなぎ直しに失敗し続けてから 502 になる（手元で確かめた）。本文付きの `GET` を使うアプリはまれなので、対処しない。`request_buffers` で本文を溜めればつなぎ直せるが、すべてのリクエストの本文をメモリに溜めることになり、上限を超えた本文はやはり送り直せない。
- 付けるのはアプリの割当ポートへの転送だけ（保護したアプリでゲートを通ったあとの転送を含む）。停止中の 503 の固定ページ、休止中の起こす経路（`koroku` が自分で最長 60 秒待たせる。8.4）、管理 UI、ゲートの問い合わせ（8.3）には付けない。
- デプロイの手順 8 の HTTP の確認と、起こす処理（8.4）の確認は割当ポートへ直接行くので、保留の影響を受けない。
- アプリごとの設定にはしない。値は待つ上限でしかなく、起動の速いアプリでは実際の待ちは短く、遅いアプリでも 30 秒を超えた分がそのまま 502 になるだけで、アプリごとに変える理由がないため。
- Caddy のヘルスチェックは使わない。転送先が 1 つなので、使えないと判断しても回す先がなく、待たせるだけになる。能動のヘルスチェック（Caddy がアプリのパスを定期的に叩く）は、使えないと判断している間は `POST` も送らずに待たせられるが、確認の間隔の分は取りこぼし、アプリのログに定期的なリクエストが増え、ヘルスチェックのパスが 5xx を返す間（DB の停止など）は、アプリ自身のエラー応答の代わりにすべてのリクエストを 30 秒待たせて 502 にしてしまう。受動のヘルスチェックは、1 回の失敗でアプリ全体へのリクエストを一定時間止める。
- WebSocket や SSE など、つながったままの接続は、古いコンテナが止まるときに切れる（クライアントの再接続に任せる）。保留が効くのは、作り直しの間に来た新しいリクエストだけ。
- 生成する設定は golden file（`testdata/caddy/*.json`）で固定する。実際の Caddy の前で、待ち受けのないポートへの `GET` と本文付きの `POST` が、あとから待ち受けたアプリに届くことを結合テストで確かめる。

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

アプリごとに 20 個までの独自ドメインを UI から追加できる。自動割当の `<slug>.dev.example.com` は常に残り、独自ドメインはそこに足される別名として扱う。

1. **登録**: UI でホスト名（例 `example.com`、`www.example.com`）を入力する。状態は `pending`。
2. **DNS 設定（利用者が実施）**: サブドメインは `<slug>.dev.example.com` への CNAME、apex は VPS の IP への A レコード。UI に設定すべき値を表示する。
3. **検証**: `koroku` がホスト名を名前解決し、`config.toml` の `public_ip` と一致したら `active` にする。「再検証」ボタンと 5 分間隔の自動再試行を持つ。
4. **反映**: `active` のホスト名だけを Caddy 設定に含めて `POST /load` する。証明書は自動割当ドメインと同じく Caddy がホスト名ごとに取得する。

- 検証前のホスト名を Caddy に渡さないのは、DNS が向いていない状態で証明書発行を試み続けるのを避けるため。
- 検証は「名前解決で得た全アドレス（A・AAAA とも）が `public_ip` と一致すること」を条件にする。正しい A レコードに加えて別ホストを指す AAAA が残っていると、一部の通信や証明書の検証がそちらへ向かうため。
- 5 分間隔の自動再試行は `pending` のものだけを対象にする。`active` になったドメインは、後で DNS が変わっても自動では `pending` に戻さない（一時的な名前解決の失敗で本番ドメインを止めないため）。「再検証」ボタンを押したときは結果どおりに更新する。
- プライマリにできるのは `active` のドメインだけ。プライマリのドメインが `pending` に戻ったら、プライマリ指定も外れる（リダイレクト先が応答しなくなるのを避けるため）。
- 追加した直後に 1 回検証するので、DNS を先に設定してあれば待たずに `active` になる。
- CDN を前段に置くなど IP が一致しない構成のために、検証を飛ばす「強制有効化」を用意する。強制有効化と、その解除はオーナーだけができる（10.7）。検証は、ホスト名がこのホストを向いていることの唯一の裏付けで、飛ばすと、裏付けのないホスト名に経路と証明書の発行の試みが付く。ホスト名は全アプリで一意なので、ほかのアプリがあとで使う名前を先に押さえることにもなる。保護したアプリでは、管理 UI の `/auth/gate?host=<ホスト名>` がログイン済みの利用者を 1 回限りのコード付きでそのホスト名へ送る（8.3）ので、管理 UI の URL から任意のホストへのリダイレクトにもなる。管理のメンバーは、検証を通ったドメインと、オーナーが強制有効化したドメインを扱う（プライマリの指定、再検証、削除）。
- アプリごとに「プライマリドメイン」を 1 つ選べる。選んだ場合、他のホスト名へのアクセスはプライマリへ 308 リダイレクトする（`www` ありなしの統一用）。
- ホスト名は全アプリで一意。`*.dev.example.com` 配下は独自ドメインとして登録できない。
- 独自ドメインはアプリごとに 20 個まで（`pending` のものを含む。定数は `internal/store` の `MaxDomainsPerApp` の 1 か所）。有効になったホスト名の 1 つ 1 つについて、Caddy は全アプリで共有する ACME アカウントから証明書を注文し（新しい注文の上限はアカウントごと）、追加と削除のたびに Caddy の設定全体を投入し直す。上限がないと、ワイルドカードの DNS レコードを持つ管理のメンバーが、検証を通るホスト名をいくらでも足せて、ほかのアプリの証明書の発行と経路の反映を妨げられる。3〜5 アプリの個人用のホストでは、`www` の有無や旧ドメインからの移行を含めても 20 個で足りる。上限に達しているアプリへの追加は `422 too_many_domains` で断り、UI は上限と「使っていないものを削除する」ことを伝える。

**データと API**

| 種別 | 内容 |
| --- | --- |
| テーブル `app_domains` | `id`, `app_id`, `hostname`（一意）, `status`（pending / active）, `is_primary`, `force_active`, `last_checked_at`, `last_error` |
| `GET / POST /api/apps/{slug}/domains` | 一覧、追加（アプリごとに 20 個まで） |
| `POST /api/apps/{slug}/domains/{id}/verify` | 再検証 |
| `PATCH / DELETE /api/apps/{slug}/domains/{id}` | プライマリ指定・強制有効化（`force_active` を変えられるのはオーナーだけ）、削除 |
| UI | アプリ詳細の設定タブの「公開」に「ドメイン」を置く。状態、設定すべき DNS レコード、直近のエラーを表示 |

### 8.2 staging と本番の分け方

staging と本番は同じ VPS に置き、「環境ごとに別アプリ」として分ける。環境という概念を基盤に足さず、同じリポジトリを複数のアプリに紐づけられるようにするだけで済む。

|  | staging アプリ | 本番アプリ |
| --- | --- | --- |
| slug の例 | `myapp-staging` | `myapp` |
| URL | `myapp-staging.dev.example.com` のみ | 独自ドメイン（プライマリ）+ `myapp.dev.example.com` |
| デプロイ元 | 任意のブランチ | `main` のコミット |
| 環境変数・volume | アプリ単位で独立 | アプリ単位で独立 |
| 閲覧 | 既定で管理 UI にログインした、そのアプリを扱える人だけ（8.3） | 既定で公開 |

- `apps` の `environment`（production / staging）は、一覧のラベルと確認ダイアログのほかに、メモリ不足のときの優先度を決める（本番のコンテナを後に止め、回収から守る。6.2）。本番アプリのデプロイと削除には確認ダイアログを出す。
- push での自動デプロイ（5.2）はどちらでも使える。staging は追うブランチ（例 `main`）を自動で出し、本番は昇格（9.7）か手動のデプロイで出す、という分け方を想定する。本番で有効にするときは確認ダイアログと再ログインを経る。
- 同居の弱点は、staging のビルド負荷やメモリ・ディスクの枯渇が本番に波及すること。ビルドの同時実行 1、サービスごとのメモリ上限（override で既定値を付与、アプリ設定で変更可）、本番を後回しにする OOM の順序とメモリの予約（6.2）、ディスク使用率 85% 超でデプロイ拒否、ログの量の上限（10.1）、で抑える。
- staging で確かめたリリースを本番へ出す「昇格」と、そのときのイメージの再利用は 9.7。
- より強い分離が要るようになったら、同じプロビジョニングスクリプトで 2 台目を立てて `koroku` をもう 1 組動かす。マルチノード対応はしない。

**本番利用で残る制約**: デプロイ時にコンテナ再作成による数秒の断がある（`GET` は Caddy で待たせて遅い応答になり、フォームの送信などは 502 になりうる。8 章「再起動中のリクエストの保留」。公開サービスを blue/green で入れ替えるアプリを除く。7.2）。VPS 1 台なのでホスト障害時は全アプリが止まる。デプロイ時の断はデプロイ対象のアプリに限られる（7 章）。ホスト障害時の全停止は許容する。

### 8.3 アクセスの保護

staging アプリは `<slug>.<base>` で誰でも開ける。未公開の機能やテスト用のデータが見えてしまうので、**管理 UI にログインしていて、そのアプリを扱える人（オーナーと、そのアプリにロールを持つメンバー。ロールの種類は問わない。10.7）だけが開ける**ようにする。アプリには手を入れない（アプリ側にログインの仕組みを足さない）で、Caddy の段階で止める。

**設定（アプリ単位）**

| 項目 | 内容 |
| --- | --- |
| `access_protection` | 保護するか（真偽値）。新しく作るアプリは `environment = staging` なら有効、`production` なら無効で始まる。既存のアプリもマイグレーションで同じ規則で決める。その後は `environment` と独立で、環境を変えても連動しない（本番を一時的に閉じる、staging を公開する、の両方がありうるため） |
| `protection_exempt_paths` | 保護を外すパスの前方一致の一覧（例 `/webhooks/`）。ログインできない呼び出し元（GitHub や決済サービスの Webhook、外部の死活監視）のため |

- 保護を外す変更（有効 → 無効）と、外すパスの一覧の変更は、10.3 の再ログイン（直近 10 分以内のログイン）を求める。アプリを外へ開く方向の変更で、セッションの Cookie だけを盗まれた場合に開けられないようにするため。保護を有効にするほうは求めない。本番アプリでは、どちらの向きでも画面で確認ダイアログを出す（閉じれば利用者が使えなくなり、開ければ公開される）。
- 外すパスは `/` で始まり、英数字と `. _ ~ - / @ : +` だけからなる 200 文字以内の文字列で、20 件まで。`/` そのもの（保護を無効にするのと同じ）、`/.koroku` で始まるもの、`.` や `..` の区切り、`//` は受け付けない。`*` や `%`、`{}` を受け付けないのは、Caddy のパスの書式（ワイルドカード、エスケープ、プレースホルダー）として解釈されないようにするため。照合は前方一致なので、ディレクトリを外すときは末尾に `/` を付ける（`/webhooks` だと `/webhooks-admin` も外れる）。
- 外れるのは、クライアントが送った URI が、登録したとおりの綴り（大文字・小文字も同じで、パーセントエンコードしていない）で始まり、パスに紛らわしい書き方を含まないリクエストだけ。`/WEBHOOKS/x` や `/%77ebhooks/x`、`/admin/../webhooks/x` は外れない（後述「外すパスの照合」）。外れなかったリクエストは断らず、ほかのパスと同じくゲートの確認に回す（ログインしていれば通る）。

**流れ**

```text
ブラウザー            Caddy (app host)             koroku                     console
GET /page   ───────▶ forward_auth ──────────────▶ GET /auth/gate/check
                                                  (Cookie なし・無効) 302 ──┐
            ◀──────────────────────────────────────────────────────────────┘
GET https://console.<base>/auth/gate?host=<app host>&return=/page
            ───────────────────────────────────────────────────────────────▶ セッションを確認
                                                                              （なければ GitHub ログインを経て戻る）
                                                                              1 回限りのコードを発行
            ◀─── 303 https://<app host>/.koroku/auth/callback?code=...&return=/page
GET /.koroku/auth/callback ──▶ koroku へ転送 ──▶ コードを消費してグラントを作る
            ◀── 303 /page + Set-Cookie: __Host-koroku_gate=...
GET /page   ───────▶ forward_auth ──────────────▶ 204（有効）
                     Cookie から __Host-koroku_gate を除いてアプリへ転送
```

- 有効でないとき、元のメソッドが `GET` か `HEAD` ならコンソールの `/auth/gate` へ 302 で送り、ほかのメソッドは 401 を返す（フォームの送信や `fetch` をログイン画面へ転送しても元の操作は再現できず、本文も失われるため）。
- 保護は Caddy の `forward_auth`（`reverse_proxy` の `rewrite` と `handle_response` で書く）で、すべてのリクエストに `koroku` への問い合わせが 1 回加わる（ループバックで、SQLite の主キー引き 1 回）。staging の用途では問題にならないと判断した。
- デプロイのヘルスチェックはループバックの割当ポートへ直接行くので、保護の影響を受けない。停止中・未デプロイのアプリの 503 の固定ページと、プライマリ以外のホスト名からの 308 リダイレクトは、保護の手前で返す（どちらもアプリの中身を見せないため）。

**Caddy の設定の形（保護したアプリ）**

アプリのホスト名のルートの中を `subroute` にし、上から順に次を置く。

1. （staging なら）応答に `X-Robots-Tag: noindex` を付ける（後述）。
2. `/.koroku/auth/*` は `koroku` へ転送する。クライアントが送った `X-Koroku-Wake`（8.4）は取り除く。
3. それ以外の `/.koroku/*` は 404。アプリには届かない（`koroku` の予約パスをアプリが知る必要はなく、アプリが同じパスで別のものを返して利用者を惑わすこともできなくする）。
4. 外すパスに当たらないリクエストは `forward_auth`。`koroku` の `GET /auth/gate/check` に、元のホスト名（`Host`、`X-Forwarded-Host`）、`X-Forwarded-Method`、`X-Forwarded-Uri` を付けて問い合わせ、2xx なら次へ進み、それ以外は `koroku` の応答（302 か 401）をそのまま返す。この問い合わせからも、クライアントが送った `X-Koroku-Wake` を取り除く。
5. `Cookie` ヘッダーから `__Host-koroku_gate` だけを取り除いて（`headers` の正規表現の置換）アプリへ転送する。外すパスへのリクエストにも同じ処理をする（ブラウザーは同じホストのすべてのパスに Cookie を送るため）。

- Caddy の `path` の照合は、パーセントエンコードを解き、`.` と `..` の区切りと重複した `/` を整え、小文字にそろえたパスで行う。転送するのはクライアントが送った URI そのままで、ヘッダーもクライアントのものが付く。`/graphql/../.koroku/auth/x`、`/graphql/..%2F.koroku/auth/x`、`/.KOROKU/auth/x` はどれも 2 のルートに当たり、その綴りのまま `koroku` に届く。`koroku` は同じ形に整えたパスで予約パスかどうかを決める（後述「`koroku` 側の入口を絞る」）。
- 保護しないアプリのルートには `subroute` を置かず、Cookie の除去もしない。保護を外した直後のブラウザーが古いグラントの Cookie を送ってきてもアプリに届くが、そのホストでしか有効にならず、保護を外したアプリでは使い道がない。

**外すパスの照合**

リクエストが `forward_auth` を飛ばすのは、次の 3 つをすべて満たすときだけ。1 つでも欠ければ、外すパスに当たらないリクエストとして `forward_auth` に回す（断らない。ログインしていれば通る）。

1. Caddy が整えたパス（デコードして `.`・`..`・重複した `/` を整え、小文字にそろえたもの）が、外すパスのどれかで始まる（`path` の照合）。
2. クライアントが送った URI が、外すパスのどれかの登録したとおりの綴りで始まる。大文字・小文字を区別し、パーセントエンコードした書き方は当たらない（`/webhooks/` に対して `/WEBHOOKS/x`、`/%77ebhooks/x`、`/webhooks%2Fx` は当たらない）。
3. クライアントが送った URI のパス（`?` より前）が、次のどれも含まない: `.` か `..` だけの区切り（`..;x` のようにパスパラメーターを付けたものを含む）、`//`、`\`、パーセントエンコードした `.`・`/`・`\`（`%2e`・`%2f`・`%5c`、大文字・小文字を問わない）。

- 1 だけでは足りない。Caddy は整えたパスで照合し、アプリには送られた URI のまま転送するので、`/admin/../webhooks/x` や `/admin/..%2Fwebhooks/x` は Caddy には `/webhooks/x` に見え、アプリには `/admin/` 以下のパスに見えうる。2 と 3 で、Caddy とアプリが別のパスとして読みうる書き方を外す対象から除く。アプリやその前段のサーバーが `..;` や `\` をどう読むかは koroku からは分からないので、読み方が割れうるものはまとめて除く。
- 大文字・小文字を区別するのは、アプリのルーティングが区別するかどうかが分からないため。`/webhooks/` を外したときに `/WEBHOOKS/` まで外れると、アプリが別の処理に割り当てているパスが開く。
- 区切りの中の `.`（`/webhooks/v1.2/x`、`/webhooks/..a`）、ほかのパーセントエンコード（`%20` や UTF-8）、クエリーの中身は見ない。クエリーはルートの照合にもアプリのパスの解釈にも関わらず、Webhook の呼び出し元が URL をクエリーに載せることがあるため。
- 2 と 3 は `expression`（CEL）の照合で書く。`{http.request.orig_uri}` は、クライアントが送ったリクエストの URI（エンコードしたまま、クエリー付き）を返す。`{http.request.orig_uri}.matches(r'^(?:<外すパス>|...)') && !{http.request.orig_uri}.matches(r'<3 の正規表現>')` の形にし、1 の `path` と同じ照合の組に入れて（両方を満たすときだけ当たる）、`not` で `forward_auth` のルートに付ける。外すパスは受け付ける文字が限られている（引用符、`{}`、`\` を含まない）ので、正規表現のメタ文字だけをエスケープして埋め込む。
- `vars_regexp` の照合は使わない。照合する値の中のプレースホルダーを展開するので、クライアントが URI に `{...}` を書くと、照合する文字列を変えられるため。
- 実物の Caddy を前に置く結合テストで、上の書き方がどれも `forward_auth` に回り、登録したとおりの綴りのリクエストだけがアプリに届くことを確かめている。Caddy 2.7 と 2.11 で同じ結果になる。

**ゲートの Cookie を `__Host-` 付きのホスト限定にする理由**

- アプリは互いに、また管理 UI とも兄弟サブドメインで動き、互いを信用できない（10 章「管理 UI の保護の細則」）。`Domain=<base>` の Cookie にすると、すべてのアプリにその Cookie が送られ、あるアプリのサーバーが受け取った値を使って別の保護されたアプリを開けてしまう。ホスト限定にすれば、ブラウザーはそのホストにしか送らない。
- `__Host-` を付けると、ブラウザーは `Domain` 属性のない、`Secure` で `Path=/` の Cookie しかこの名前で受け付けない。兄弟サブドメインのアプリが親ドメイン向けに同じ名前の Cookie を置いて上書きする（cookie tossing）ことができない。
- さらに、グラントはホスト名に結び付けて保存し、問い合わせのホスト名と一致しなければ無効にする。値がどこかに漏れても、ほかのアプリのホストでは使えない。
- 属性は `Secure`、`HttpOnly`（アプリのページの JavaScript から読めない）、`SameSite=Lax`（ほかのサイトからのフォーム送信や埋め込みには付かず、リンクで開いたときには付く）、`Path=/`。
- 管理 UI のセッション Cookie（`__Host-koroku_session`）は管理 UI のホスト限定で、広げない。広げるとすべてのアプリがホストの root 相当の権限を持つセッションを受け取ることになる。

**1 回限りのコードを使う理由**

- ゲートの Cookie はアプリのホストで発行しなければならない（ホスト限定のため）が、ログインの状態を知っているのは管理 UI のホストだけである。2 つのホストの間は、ブラウザーのリダイレクトの URL でしか受け渡せない。
- URL はブラウザーの履歴、アクセスログ（Caddy のアプリ別ログは収集してオブジェクトストレージへ退避する）、`Referer` に残りうる。そこに長く使える値（グラントの Cookie の値やセッションの値）を載せず、乱数 32 バイトで、60 秒で失効し、1 回使うと消え、発行先のホスト名とセッションに結び付いたコードだけを載せる。コードは DB にハッシュだけを置く。
- 消費は「ハッシュとホスト名が一致し、期限内の行を消す」1 つの文で行い、同じコードを 2 回使えない。消費したらすぐにグラントを作り、Cookie を発行して戻り先へ 303 で送る。この応答には `Referrer-Policy: no-referrer` を付ける。

**管理 UI のセッションに結び付ける理由**

- グラントは発行時の管理 UI のセッション（`sessions` の行）を指し、問い合わせのたびにそのセッションが有効で、そのアプリを扱えるユーザー（オーナーか、そのアプリにロールを持つメンバー）のものであることを確かめる。管理 UI でログアウトする、セッション一覧から終了する、ほかのセッションをすべて終了する、`config.toml` のオーナーを変える、メンバーからそのアプリのロールを外す、のいずれでも、そのセッションから作ったグラントは次のリクエストから無効になる。
- グラントの期限は発行元のセッションの期限と同じにし、それを超えない。セッションの行を消すとグラントとコードも外部キーの `ON DELETE CASCADE` で消える。
- 10.3 の再ログインで新しいセッションに入れ替わると、古いセッションのグラントは無効になる。次に `GET` で開いたときにリダイレクトが 1 往復して、新しいセッションのグラントに入れ替わる（画面の操作は要らない）。
- アプリのホストの `/.koroku/auth/logout` はそのホストのグラントと Cookie だけを消す。管理 UI からはログアウトしないので、もう一度開くと自動で通る。完全に閉じるには管理 UI でログアウトする。

**任意のホストへのリダイレクトを防ぐ**

- `/auth/gate` は、ログインしているユーザーがそのアプリを扱えなければ、コードを発行せずに 403 のページ（「このアプリを開く権限がありません」）を出す。
- `/auth/gate` は `host` が「いま保護している、存在するアプリのホスト名」（自動割当のホスト名と、有効な独自ドメイン）のときだけコードを発行し、`https://<host>/.koroku/auth/callback` へリダイレクトする。それ以外は 400 を返し、どこへもリダイレクトしない。管理 UI のホスト名から任意のサイトへ飛ばす踏み台にさせないため。
- 戻り先（`return`）は、管理 UI の `return_to` と同じ規則で、`/` で始まり `//` や `\` を含まないパスだけを受け付け、それ以外は `/` にする。コールバックは同じホストのパスへしか戻さない。長さは 2048 バイトまで。
- 未ログインなら、通常の GitHub ログイン（`/auth/github/login?return_to=/auth/gate?...`）を経て `/auth/gate` に戻る。

**`koroku` 側の入口を絞る**

- アプリのホスト名で `koroku` に届くのは、`/.koroku/auth/callback`、`/.koroku/auth/logout`、`/auth/gate/check` だけにする。Caddy はアプリのホストからは、これらと休止中のアプリを起こす転送（8.4）しか送らないが、`koroku` の側でも、アプリのホスト名（`base_domain` 配下の管理 UI 以外のホスト名と、有効な独自ドメイン）へのリクエストは、これら以外を 404 にする。管理 UI と API は、アプリのホスト名では応答しない。どのアプリのものでもないホスト名（`koroku` が待ち受けるループバックのアドレスなど）では管理 UI が応答する。逆に、これらのパスは呼ばれたホスト名が保護しているアプリのものであることを確かめ、管理 UI のホスト名では使えない。
- 予約パス（`/.koroku` とその配下、`/auth/gate/check`）かどうかは、Caddy の `path` の照合と同じ形に整えたパス（デコードして `.`・`..`・重複した `/` を整え、小文字にそろえたもの）で決める。Caddy が予約パスとして転送したリクエストを、`koroku` がアプリのパスや管理 UI のパスとして扱うことがないようにするため。ゲートの処理が応えるのは、整えた形と同じ綴りで届いたものだけで、`/graphql/../.koroku/auth/logout` や `/.KOROKU/auth/logout` のような別の綴りは、どのホスト名でも 404 にする。
- `/auth/gate/check` は管理 UI のホスト名や未知のホスト名では常に 403 を返す。結果は「通すかどうか」だけで、何も漏らさない。

**検索エンジンに載せない**

- `environment = staging` のアプリは、保護の有無にかかわらず、すべての応答に `X-Robots-Tag: noindex` を付ける（Caddy が応答の送出時に設定する）。保護を外した staging や、外すパスの応答が検索結果に出ないようにするため。本番アプリには付けない。

**開発用モード（`--insecure-no-auth`）**

- ログインがないので、`/auth/gate/check` は常に通す（204）。`/auth/gate` も、ホスト名の確かめだけをしてコードなしでアプリへ戻す。ローカルの Caddy には本番と同じ形の設定（`forward_auth` と `/.koroku/auth/*` の転送）を入れるので、経路そのものは手元で確かめられる。

**限界**

- ブラウザーでログインできない呼び出し元（`curl`、モバイルアプリ、他のサービスからの API 呼び出し）は通らない。外すパスに入れたパスは、アプリ自身が認証する（Webhook の署名の検証など）必要がある。
- `forward_auth` への問い合わせには、元のリクエストのヘッダー（アプリ用の Cookie や `Authorization`）がそのまま付いて `koroku` に届く。`koroku` はゲートの Cookie 以外を読まず、記録もしない。
- 有効なグラントの Cookie を持つブラウザーの中では、アプリの JavaScript がそのホストへのリクエストを自由に送れる（Cookie はブラウザーが付ける）。これはアプリそのものを信用していることと同じで、防がない。

**データ**

| テーブル | 主なカラム |
| --- | --- |
| `apps` に追加 | `access_protection`（0 / 1）、`protection_exempt_paths`（JSON の文字列配列） |
| `gate_codes` | `code_hash`（主キー）、`hostname`、`session_token_hash`（`sessions` への外部キー、`ON DELETE CASCADE`）、`expires_at` |
| `gate_grants` | `token_hash`（主キー）、`hostname`、`session_token_hash`（同上）、`created_at`、`expires_at` |

期限切れのコードとグラントは、新しいコードやグラントを作るときに消す。

**不採用案**

- Basic 認証: パスワードを別に管理することになり、ログアウトや失効ができない。
- 管理 UI のセッション Cookie を `Domain=<base>` に広げる: 上記のとおり、すべてのアプリにホストの root 相当の権限が漏れる。
- アプリのホストで GitHub ログインをやり直す: GitHub App の callback URL はアプリの数だけ登録が要り、独自ドメインでは登録できない。

### 8.4 アクセスがない間の休止

staging のようにたまにしか開かないアプリを、アクセスがない間は止めて、ホストのメモリを空ける。4GB のホストに 3〜5 アプリを置くと、ほとんど使われないアプリも本番と同じだけメモリを持ち続けるため。止めたアプリは次のアクセスで起き、そのリクエストに応える。アプリごとに選び（既定は無効）、無効のアプリの振る舞いは変わらない。

**設定（アプリ単位）**

| 項目 | 内容 |
| --- | --- |
| `idle_sleep` | アクセスがない間は止めるか（真偽値、既定は偽） |
| `idle_sleep_minutes` | 何分アクセスがなければ止めるか。10〜1440、既定 30。画面は 15 / 30 / 60 / 180 から選ぶ |

- 設定はアプリ設定（`PATCH /api/apps/{slug}`）の項目で、変えられるのは管理以上のロール（10.7）。再ログイン（10.3）は求めない。止めても次のアクセスで戻り、データを外へ出さないため。
- 本番アプリ（`environment = production`）で有効にするときは、画面で確認ダイアログを出す。休止のあとの最初の利用者が起動を待たされ、外部の死活監視を当てていると休止しないため。禁止はしない（利用者の少ない本番もありうる）。
- Caddy のアプリ別のアクセスログ（8 章の `access_log_dir`）がないホストでは有効にできない（API が 422 `validation_failed`、`idle_sleep`）。アクセスを数える手段がないため。`GET /api/system` の `idle_sleep.available` で、画面は欄を無効にして理由を出す。
- 無効にしても、休止中のアプリはそのまま休止を続け、次のアクセスで起きる（設定の保存でコンテナを動かさない）。

**休止中という状態**

- 休止は、利用者の操作の「停止」（`desired_state = stopped`）とは別の、実行時の状態として持つ。`apps.sleeping_since` に休止した時刻を入れ、起きていれば NULL。`desired_state` は `running` のまま。停止中のアプリは 503 の固定ページを返してアクセスでは起きず、休止中のアプリはアクセスで起きる。
- DB が唯一の正で、Caddy の設定は `sleeping_since` から作る。`koroku` が再起動しても休止中のアプリは休止中のままで、起動時の投入でも起こす経路になる。ホストを再起動しても、Docker は `docker compose stop` で止めたコンテナを `restart: unless-stopped` では起動しないので、DB とそろう（`restart: always` のサービスは起動するが、次のアクセスで起こす処理が「もう動いている」ことを確かめて休止を解く）。
- 起動と停止（10 章）は、`desired_state` を書くときに `sleeping_since` も消す。休止中のアプリを停止すれば停止中に、起動すれば稼働中になる。

**アクセスの数え方**

- Caddy を通ったリクエストだけを数える。デプロイと起こす処理のヘルスチェックはループバックの割当ポートへ直接行くので数えない。
- アクセスログはログのコレクタ（10.1）が取り込んでいる。コレクタは新しい行を書くたびに休止の見張りへ知らせ、見張りはそのアプリの最後のアクセスの時刻を進める。`koroku` の起動時に取り込む、前回までに書かれていた行は数えない。
- 保護したアプリ（8.3）では、ゲートが返した応答（`/auth/gate` への 302 と 401）は数えない。ログインしていない呼び出し元（クローラーなど）が、保護したアプリを起こしたままにしないため。保護を外すパス（Webhook など）への呼び出しはアプリに届くので数える。見分けには行の `status` と `location`（8 章「アクセスログの内容」）を使う。JSON として読めない行も数えない。Caddy が書く行は必ず 1 行に収まって届くので、読めない行はゲートを通ったリクエストの記録ではないため。保護していないアプリでは、行の中身を読まずに 1 行を 1 回のアクセスとして数える。
- 見張りは 1 分ごとに、休止を有効にした稼働中のアプリについて最後のアクセスからの時間を見て、設定の分を超えたものを休止させる。最後のアクセスの時刻はメモリにだけ持つ。`koroku` の起動時は、すべてのアプリを「いまアクセスがあった」とみなす（再起動のたびに、溜まっていた分で一斉に止めないため）。見張りが、アプリが起きた（休止中でなくなった、起動した、現在のリリースが変わった）ことに気づいたときも同じく、いまアクセスがあったとみなす。
- 取り込みが止まっている（コレクタがファイルを読めないなど）と、アクセスを見落として止めてしまう。そこで見張りは 1 分ごとにアクセスログのファイルの大きさと更新時刻も見て、ファイルが変わったのにその間コレクタから 1 行も届いていなければ、アクセスがあったとみなす（止めない側に倒す）。
- 数えるのはリクエストの有無だけで、中身は見ない。クローラー、外部の死活監視、開いたままのタブのポーリングも 1 回のアクセスになり、それらが設定の分より短い間隔で来るアプリは休止しない。画面にそう書く。

**休止のしかた**

- そのアプリの操作ロック（10 章「起動制御と削除」）を待たずに取れるときだけ休止する。デプロイが `queued` か `running` のとき、コマンドの実行中（9.6）、DB のコピー中（10.6）、ほかの操作の実行中は見送り、次の 1 分で見直す。バックアップの実行中（10.2）も見送る（ダンプフックが動いているコンテナを止めないため）。
- 手順は、`sleeping_since` を書く → Caddy の設定を投入する（アプリのホスト名が起こす経路になる）→ `docker compose -p koroku-<slug> stop`（プロジェクトの全サービス）。経路を先に切り替えるので、止めている最中に来たリクエストはアプリに届かずに起こす処理で待ち、休止が終わってロックが空いてから起こされる。`stop` に失敗したら `sleeping_since` を消して経路を戻す。
- `down` ではなく `stop` にするのは、コンテナを残せば起こすときに作り直しが要らず速いため。named volume、ネットワーク、イメージはそのまま残る。
- 休止させたことはサーバーログに残し、操作の記録（10.7）には残さない（アクセスのたびに起きうる操作で、記録が溢れるため）。

**起こす経路（Caddy）**

- 休止中のアプリのホスト名（自動割当と有効な独自ドメイン）のルートは、アプリの割当ポートの代わりに `koroku`（`listen` のアドレス）へ転送する。この転送では、Caddy がヘッダー `X-Koroku-Wake: <slug>` を設定する（クライアントが同名のヘッダーを送っても置き換わる）。`koroku` へのほかの転送（管理 UI、`/.koroku/auth/*`、ゲートの確認）は、クライアントが送った `X-Koroku-Wake` を取り除く。このヘッダーが `koroku` に届くのは、Caddy が起こす経路で転送したときだけになる（実物の Caddy を前に置く結合テストで確かめている。Caddy 2.7 と 2.11 で同じ結果になる）。そのほかは通常の転送と同じ（`flush_interval: -1`、`stream_close_delay`）。ただし 8 章の再起動中の保留は付けない（`koroku` が自分で待たせるため）。
- ルートの中の順序は起きているアプリと同じで、置き換わるのは最後の転送先だけ。プライマリ以外のホスト名の 308 リダイレクト、`X-Robots-Tag`、アクセスの保護（8.3）の `forward_auth` と Cookie の除去は、その手前でこれまでどおり効く。保護したアプリでは、ログインしていないリクエストはゲートで止まり、アプリを起こさない。
- 停止中と未デプロイのアプリは、休止より 503 の固定ページが優先する。
- 生成する設定は golden file（`testdata/caddy/sleeping.json`）で固定する。

**起こす処理（koroku）**

- `koroku` は、`X-Koroku-Wake` の付いたリクエストを、管理 UI と API とは別の処理に渡す。対象は管理 UI 以外のホスト名で、アクセスの保護の予約パス（`/.koroku/` 以下と `/auth/gate/check`。8.3）以外のもの。予約パスかどうかは、Caddy の `path` の照合と同じ形に整えたパスで決める（8.3「`koroku` 側の入口を絞る」）。予約パスはヘッダーがあってもゲートの処理に渡すので、休止中の保護していないアプリでも、これらのパスへのリクエストはアプリを起こさず 404 になる。`.` や `..` の区切り、重複した `/` を含むパス（デコードしたあとで整えた形と違うもの）は、アプリを起こさず、転送もせずに 400 を返す。Caddy と `koroku` が同じリクエストを別のパスとして読む余地を残さないため。起きているアプリには Caddy がそのまま転送するので、こうしたパスに応えるかどうかはアプリが決める。管理 UI のセキュリティヘッダーとセッションの検査はかけない（アプリの応答をそのまま返すため）。ヘッダーは、リクエストのホスト名のアプリ（自動割当か有効な独自ドメイン）を指すときだけ意味を持ち、それ以外はヘッダーがないものとして通常の処理に渡す（アプリのホスト名なら 404）。
- アプリがすでに起きている（DB の `sleeping_since` が空で、Caddy の設定がまだ追いついていない）なら、そのまま割当ポートへ転送する。停止中か未デプロイなら 503 の固定ページを返す。
- 休止中なら起こす。同じアプリへの同時のリクエストは、1 回の起こす処理を共有する。起こす処理はリクエストから切り離して動き、リクエストが切れても続く。
  1. そのアプリの操作ロックを待つ（デプロイのキューとは別のもので、全体で 1 つのデプロイのワーカーの後ろには並ばない）。取れたら DB を読み直し、もう休止中でなければ（デプロイや起動が起こした）終わる。
  2. `docker compose -p koroku-<slug> start --wait --wait-timeout <up の待ち時間>`。`stop` で止めたコンテナをそのまま起動する。`up` を使わないのは、休止の前に動いていたのと同じコンテナを戻せば足り、Compose ファイルの読み直し、ポリシーの再検査、イメージの確認が要らず速いため。依存の条件（`depends_on` の `service_healthy` など）は `start` でも守られる。コンテナが 1 つもない（ホストで手で消したなど）ときだけ、10 章の起動と同じく稼働中のリリースのファイルで `up -d --wait` する。
  3. デプロイの手順 8 と同じ HTTP の確認（`http://127.0.0.1:<割当ポート><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒）。
  4. `sleeping_since` を消し、Caddy の設定を投入する。以降のリクエストは Caddy から直接アプリへ行く。
- 起こせなかったら（`start` の失敗、HTTP の確認が通らない）、`docker compose stop` で止め直して休止のままにし、理由をサーバーログに残す。それから 1 分の間は、次のリクエストにも同じ失敗を返し、起動を繰り返さない。
- リクエストは最長 60 秒待たせる。その間に起きれば、元のリクエストをそのまま割当ポートへ転送する。本文はまだ読んでいないのでそのまま渡り、WebSocket の Upgrade と SSE（応答を溜めない）も通る。`Host` と、Caddy が付けた `X-Forwarded-For` / `X-Forwarded-Proto` / `X-Forwarded-Host` はそのまま渡し、`X-Koroku-Wake` は外す。
- 60 秒で起きなければ 503 と `Retry-After: 5` を返す（起こす処理は続く）。ブラウザー（`Accept` に `text/html` を含む `GET`）には「起動しています」のページを返し、5 秒後に読み直させる（`<meta http-equiv="refresh">`。スクリプトは使わない）。起こせなかったときは 502 と「起動できませんでした」のページ。どちらも `Cache-Control: no-store`。
- 休止のあとの最初のリクエストは、コンテナの起動とアプリの準備を待つ。Rails なら 5〜15 秒ほどかかることが多い。設定の欄にそう書く。
- 休止と起こす処理は、どちらも Caddy の設定の投入を伴う。他アプリの WebSocket は、8 章の `stream_close_delay` の猶予のあとに 1 回切れる（アプリの起動・停止と同じ）。`koroku` を経由して転送していた接続も、同じ猶予のあとに切れる。
- 管理 UI からも起こせる（`POST /api/apps/{slug}/wake`。ロールは起動と同じデプロイ以上）。起きるまで待って、アプリの JSON を返す。休止中でなければ何もしない。操作の記録に `app.woken` を残す。

**ほかの操作との関係**

- デプロイ（7 章）: 休止中のアプリもデプロイできる（push での自動デプロイ 5.2 も止めない）。手順 6（起動）に入るときに `sleeping_since` を消し、新しいリリースが起きた状態で動く。手順 5 までで失敗したら休止のまま。手順 6 以降で失敗して自動ロールバックしたときは、戻したリリースが起きた状態で動く。
- 起動・停止・再起動（10 章）: 起動と停止は休止を解く（上記）。休止中のアプリの再起動は、起こすのと同じ処理になる。
- コマンドの実行（9.6）: 管理 UI からの実行は、休止中なら、実行の前に、実行が持つ同じ操作ロックの中で起こす（`run --no-deps` は依存先が動いていないと使えないため）。ログにその旨を書く。実行中は休止しない（ロックを持っているため）。人が手で実行するときは、そのアプリをいま触っていることが多く、起こしておくほうが続けて使えるため。
- 定期タスク（9.6）: 休止中のアプリでは、アプリを起こさない。同じ操作ロックの中で、タスクのサービスが `depends_on` でたどる依存先だけを `docker compose start --wait <サービス>...` で起動し、`run --rm --no-deps` でタスクを実行して、終わったら `docker compose stop` で止め直す。公開サービスは起動せず、Caddy の経路も `sleeping_since` も変えない。ダンプフックと同じ仕組み（同じ処理を使う）で、深夜や早朝の定期タスクのたびにアプリ全体が起きてメモリを使わないため。定期タスクの実行は Caddy を通らないので、アクセスとして数えない。
- DB のコピー（10.6）: 受け側と、取り元が `live` のときの送り側が休止中なら、`check` の手順で起こす。
- バックアップ（10.2）: volume は止まったままで取れる。ダンプフックは、アプリが休止中なら、そのアプリの操作ロックの中でフックのサービスだけを `docker compose start --wait <サービス>` で起動してダンプを取り、終わったら `docker compose stop` で止め直す。アプリ全体は起こさず、Caddy の経路も変えない。深夜の定期実行で休止中のアプリが一斉に起き、ホストのメモリを超えないようにするため。ダンプを取らずに済ませないのは、昼に使って夜は休止している staging では、毎晩のダンプが取れなくなるため。フックのサービスには、接続を受け付けられるまで待てるよう、Compose の `healthcheck` を書いてもらう（Skill で案内する。ない場合は起動した直後にダンプが始まる）。
- ログの収集（10.1）: 休止の間、そのアプリの `logs --follow` は終わり、起きると再開する（停止中のアプリと同じ）。
- メモリの履歴（9.5）: 休止中はコンテナが動いていないので記録がない。止めていた時間と同じく、目安の根拠の長さに数えない。
- 概要とリソースのタブ: サービスは止まった状態で出る。画面は休止中であることを出す。

**画面**

- アプリ一覧と詳細のヘッダーの状態は「休止中」。色とアイコンは停止と同じ灰色で、ラベルとツールチップで区別する（画面の状態の色とアイコンの種類は増やさない）。
- 概要タブに休止した時刻を、アプリ詳細のヘッダーに「起こす」ボタン（デプロイ以上。9.1）を出す。
- リソースタブの「アクセスがない間の休止」（9.1）に「アクセスがない間は止める」のスイッチと分の選択を置き、最初のアクセスが起動を待つこと（Rails で 5〜15 秒ほど）と、クローラーや死活監視が来ていると休止しないことを書く。休止はホストのメモリを空けるための設定なので、メモリの上限と予約の隣に置く。Caddy のアプリ別のアクセスログがないホスト（`GET /api/system` の `idle_sleep.available` が `false`）では、有効にできず、無効にすることだけができる。

**データ**: `apps` に `idle_sleep`（0 / 1）、`idle_sleep_minutes`、`sleeping_since`（Unix ミリ秒、NULL 可）。

**不採用案**

- Caddy の負荷分散で、アプリのポートにつながらなければ `koroku` へ回す（`first` の選び方と受動的なヘルスチェック）: 休止と起こすたびの設定の投入が要らないが、経路がどちらを向いているかが Caddy のメモリの状態で決まり、DB から決まらない。デプロイの入れ替えで一瞬つながらないときにも `koroku` へ回ってしまう。
- 休止のたびにダンプを取る: 休止のたびに restic のスナップショットが増え、バックアップの設定がないホストでは使えない。
- アプリ全体を起こしてからダンプする: 上記のとおり、深夜にすべての休止中のアプリが同時に起きる。
- 受け付けたリクエストを常に `koroku` 経由で転送する: すべてのリクエストに `koroku` の転送が加わり、`koroku` の停止や更新でアプリまで止まる。

## 9. データモデル・API・UI

データは SQLite の 1 ファイル（`koroku.db`）にある。マイグレーションは埋め込みの SQL を起動時に適用する。

| テーブル | 内容 |
| --- | --- |
| `apps` | slug、リポジトリ（`repo_id`、`repo_full_name` か `repo_url`）、Compose の置き場所と本文（`compose_source`、`compose_content`、6.3）、Compose ファイルのパス、公開サービスとポート、割当ホストポート、ヘルスチェックパス、`environment`、`desired_state`、`relax_policy`、メモリ上限、メモリの予約（`memory_reservation`、`service_memory_reservations`、6.2）、稼働中のデプロイ、バックアップの設定（除外 volume、ダンプフック、ダンプの間隔 `dump_interval_hours`、デプロイ前取得。10.2、10.8）、時点指定の復旧のサービス（`pitr_service`、10.9）、DB のコピーの設定（ロードフック、コピー後のコマンド。10.6）、必要な変数の読み方（`env_source`、`env_example_path`、9.4）、アクセスの保護（`access_protection`、`protection_exempt_paths`、8.3）、昇格元のアプリ（`promote_from_app_id`、9.7）、自動デプロイ（`auto_deploy`、`auto_deploy_branch`、`auto_deploy_workflow`、5.2）、アクセスがない間の休止（`idle_sleep`、`idle_sleep_minutes`、`sleeping_since`、8.4）、レビューアプリの許可（`review_apps_allowed`、9.9）、無停止デプロイ（`blue_green`、`alt_host_port`、`web_color`、7.2） |
| `review_apps` | レビューアプリの親、pull request の番号・題名・head のブランチ・状態、データのコピーの状態（9.9） |
| `env_vars` | 環境変数。値は暗号化して保存（`app_id` + `key` で一意） |
| `registry_credentials` | アプリのコンテナレジストリの認証（`app_id` で一意。レジストリ、ユーザー名、暗号化したトークン、更新時刻。6.1） |
| `deployments` | ref、コミット、状態、エラー、時刻、環境変数の指紋（9.2）、サービスごとのメモリの予約とその形（`memory_reservations`、`memory_shapes`、6.2）、ロールバックの戻す先（`source_deployment_id`、7.1）、昇格元のデプロイ（`promoted_from_deployment_id`、9.7）、始めたユーザー（`actor_user_id`、`actor_login`。10.7）、何が始めたか（`trigger_kind`、`pusher`、5.2）、稼働中のコンテナを置き換えたか（`containers_replaced`、9.5）、公開サービスの色と切り替え（`web_color`、`switched_at`、7.2） |
| `github_deployments` | デプロイに対応する GitHub のデプロイ ID と、GitHub が最後に受け付けた状態（5.1） |
| `pending_auto_deploys` | コマンドの実行が終わるまで預かっている自動デプロイ（アプリごとに 1 行。5.2） |
| `app_domains` | 独自ドメイン（8.1） |
| `backup_runs` | バックアップの実行記録（10.2。間隔のダンプは 10.8） |
| `db_copies` | DB のコピーと、ダンプからの復旧の実行記録（10.6、10.8） |
| `pitr_bases`、`pitr_wal_batches`、`pitr_gaps`、`pitr_restores` | 時点指定の復旧のベースバックアップ、退避した WAL のまとまり、戻せない区間、復旧の実行記録（10.9） |
| `sessions` | セッション（Cookie のハッシュ、表示用の ID、ユーザー、最終利用、IP、User-Agent。10.3） |
| `login_events` | ログイン履歴（10.3） |
| `api_tokens` | 読み取り専用の API トークン（ハッシュ、名前、ユーザー、期限、最終利用。10.5） |
| `members`、`app_roles` | メンバーと、アプリごとのロール（10.7） |
| `app_activity` | アプリの操作の記録（誰が、いつ、何を。10.7） |
| `memory_usage` | サービスごとのメモリ使用量の 5 分ごとの集計（9.5） |
| `gate_codes`、`gate_grants` | アクセスの保護の 1 回限りのコードとグラント（8.3） |
| `app_tasks`、`task_runs` | 登録したタスク（定期実行の予定と、直近に見送った予定を含む）と、コマンドの実行記録（9.6） |

API はすべて JSON の REST で、ログとデプロイの進行は SSE で配信する。`/auth/*` と、署名で認証する GitHub の Webhook（`POST /webhooks/github`、5.2）を除き、セッション Cookie が要る。エンドポイントごとに、誰が使えるか（全ユーザー / オーナーだけ / アプリのロールの段階）が決まっている（10.7）。エンドポイントの一覧と形は [API.md](API.md) が正。読み取り専用の API トークンで使える `GET` は 10.5。

### 9.1 管理 UI の構成と API

管理 UI は「上部バー + アプリ一覧（ホストの状態を常時表示）+ アプリ詳細（ヘッダーと横タブ: 概要 / デプロイ / ログ / リソース / タスク / 環境変数 / バックアップ / 設定）+ システム（状態 / 定期処理 / 管理）+ セキュリティ」で構成する。上部バーの左に「アプリ」と「システム」、右にテーマの切り替えとユーザーメニュー（「セキュリティ」「ログアウト」）を置く。システムはオーナーだけの画面で、メンバーには入口を出さない。アプリ詳細は、ロールでできない操作のボタンや欄を出さないか無効にし、設定の欄は値だけを読める形で出す（10.7）。画面の要素と、それを支える API の値は次のとおり。

**アプリ詳細のタブと節**

タブは「来る目的」ごとに 1 つにし、同じものを 2 か所で変えられるようにしない。タブが多いと狭い画面で横に流れて見渡せず、同じ設定が 2 つのタブにあると、どちらで変えたものが効くのかが分からなくなるため。使う頻度の低いものは、近い目的のタブの節にする。設定も、それが効く場所を見に来るタブに置く。設定を変えたくなるのは、その結果（デプロイの履歴、照合の結果、メモリの使用量）を見ているときで、結果の隣で変えれば、別のタブへ移って探さずに済むため。

| タブ | 節 | まとめる理由 |
| --- | --- | --- |
| 概要 | 稼働中のリリース、サービス、直近のデプロイ、ドメイン、操作の記録 | 状態を見に来る場所 |
| デプロイ | デプロイのフォーム、履歴、レビューアプリ（親になれるアプリだけ。9.9）、デプロイの設定（自動デプロイ、昇格元（本番だけ）、無停止デプロイ（7.2）、デプロイの安全装置） | レビューアプリは pull request ごとのデプロイ先で、デプロイと続けて扱う。デプロイの設定は「何が、どう積まれるか」を決めるもので、その結果は履歴に出る |
| ログ | — | 全幅・全高で使う |
| リソース | 稼働中のリリース、今の使用量、メモリの上限と予約、アクセスがない間の休止、メモリの履歴、構成（9.10） | 上限と予約は使用量と履歴を見て決めるもので、構成は上限や予約が効いているかを確かめる場所。休止はホストのメモリを空けるための設定で、上限と予約と並べて決める。どれも「メモリとコンテナ」の 1 つの関心 |
| タスク | — | 実行の履歴とライブログを持つ |
| 環境変数 | 環境変数、必要な環境変数の照合（照合の結果と照合元の設定） | 1 件ずつ確定する独自の編集（9.2）。照合元を変えると結果が変わるので、結果の隣で選ぶ |
| バックアップ | — | 実行とリストア |
| 設定 | ソース、公開（ドメインを含む）、環境、アクセスの保護、メモリの上限と予約（読み取りだけ）、コンテナレジストリの認証、危険な操作 | ほかのタブの関心に入らない、変える頻度の低いアプリの設定。ドメインは公開の仕方の一部なので「公開」に置く。レジストリの認証は、ソースと同じく取得するものを決める |

- **ヘッダーの操作**: アプリ詳細のヘッダーには、どのタブでも「デプロイ」と、その隣の「…」（その他の操作）のメニューを置く。メニューには「再起動」と「停止…」（止まっていないアプリ）を置き、停止は確認のダイアログ（本番アプリでは題に「本番アプリ」）を挟む。アプリが止まっているときは「起動」、休止中（8.4）のときは「起こす」を、メニューではなくヘッダーのボタンとして出し、そのボタンを主に、「デプロイ」を並のボタンにする。動かすべきときに、操作がメニューの奥に隠れないようにするため。どれもデプロイ以上のロール（10.7）にだけ出し、一度もデプロイしていないアプリには出さない（起動するリリースがない）。結果（「再起動しました。」など）とエラーは、ヘッダーの下に出す。アプリの削除は、元に戻せない操作として設定タブの「危険な操作」に残す。
- **節への移動**: 節には URL のクエリ `section` で直接入れる（例 `/apps/<slug>/deploy?section=review-apps`、`/apps/<slug>/deploy?section=auto-deploy`、`/apps/<slug>/resources?section=idle-sleep`、`/apps/<slug>/env?section=env-requirements`、`/apps/<slug>/settings?section=domains`）。開くと、その節を画面の上端に寄せ、節にフォーカスを移す。アプリ一覧の「レビューアプリ 3」、概要のドメインとレビューアプリの行、確認待ちのドメインの警告、レビューアプリのヘッダーの「親」、昇格の欄の「昇格元」、照合できなかったときの「照合元の設定」は、この形で節へつなぐ。
- **設定の目次**: 設定タブは縦に長いので、節の目次を置く。広い画面では左に固定した一覧（いま見えている節に印）、狭い画面（64rem 未満）では上端に固定した「項目へ移動」の選択欄にする。選ぶと「節への移動」と同じ動きをし、URL の `section` も書き換える（履歴は増やさない）。
- **設定の保存**: 設定を持つ節は、タブごとに保存のボタンを持ち、そのタブの項目だけを `PATCH /api/apps/{slug}` に送る（変えた項目だけ。ほかのタブの項目は送らない）。検査（ポートの範囲、保護を外すパスの形）と、本番アプリで確認を挟む変更（アクセスの保護、自動デプロイ、休止）は、どのタブでも同じ処理を通す。
  - 設定タブのフォームは節ごとの `<form>` に分かれているが、編集中の値は 1 つで、保存のボタン（ページの末尾）と各欄の Enter は設定タブの全部の節をまとめて保存する。ドメインの追加などは「公開」の節の中にあってもフォームの外で、押したときにすぐ反映される（ドメインの API を呼ぶ。8.1）。節ごとに分けるのは、ドメインの欄（それ自体がフォームとダイアログを持つ）を設定のフォームの中に入れ子にできないため。
  - デプロイタブの「デプロイの設定」、リソースタブの「アクセスがない間の休止」、環境変数タブの照合元は、それぞれの節の中に 1 つのフォームと保存のボタンを持つ。
  - 設定タブの既定のブランチと環境は、ほかのタブの確認に関わる（本番アプリで自動デプロイが既定のブランチを追っているとき、既定のブランチを変えると追うブランチが変わる。staging から本番に変えると、有効な自動デプロイと休止が本番で効き始める）。この確認は、変えた設定タブの保存で尋ねる。
- **保存していない変更**: 保存していない変更があるまま、デプロイが保存済みの設定で始まる操作に進もうとすると、先に尋ねる。デプロイは保存済みの設定で行われ、タブを移ると編集中の内容は消えるため。
  - 設定タブ、リソースタブ、環境変数タブからデプロイのタブ（ヘッダーのボタンを含む）へ移ろうとすると、移る前に「保存して進む / 保存せずに進む / キャンセル」を尋ねる。
  - デプロイタブの「デプロイの設定」に保存していない変更があるまま、デプロイ（フォームのボタン、履歴の「このコミットを再デプロイ」）を押すと、「保存してデプロイ / 保存せずにデプロイ / キャンセル」を尋ねる。保存せずにデプロイしたときは、編集中の値は欄に残る（タブを移らないので消す理由がない）。
- **メモリの上限と予約**: 変えるのはリソースタブだけ（9.5）。設定タブには保存済みの値の一覧と、リソースタブの節への「リソースで変える」だけを置く。

**システムの画面**

システムは 3 つのタブ（子のルート）に分ける。どれもオーナーだけが開ける（メンバーが URL を開くとアプリ一覧へ戻す）。

| タブ | パス | 節 | 来る目的 |
| --- | --- | --- | --- |
| 状態 | `/system` | ホストのリソース、アプリごとのメモリと CPU（9.3）、本番アプリのメモリの予約（6.2）、Docker のディスク使用量と「不要なデータを削除」 | ホストに余裕があるかを確かめる、空きを作る |
| 定期処理 | `/system/jobs` | 定期処理（バックアップ、ログ退避、Caddy への設定投入）、バックアップの実行履歴 | 裏で動くものが止まっていないかを確かめる、バックアップを今すぐ取る |
| 管理 | `/system/admin` | メンバー（10.7）、GitHub App（5 章）、バージョン | 人とつながりの設定。めったに開かない |

- 読み込みはタブごとにする。状態のタブだけが `GET /api/system/containers` と `GET /api/system/docker`（どちらも数秒かかる）を呼び、定期処理のタブは `GET /api/backups` と `GET /api/system` の `log_archive` と `caddy`、管理のタブはメンバーの一覧、`GET /api/system/github`、`GET /api/system` のバージョンだけを読む。ほかのタブを見に来ただけで重い問い合わせを走らせないため。開いている間の読み直しも、そのタブの分だけ行う。
- アプリ一覧の警告バナーは、ディスクの警告を状態のタブへ、バックアップの警告を定期処理のタブへつなぐ。作成画面の「別の org のリポジトリを使うには」は管理のタブの GitHub App の節へつなぐ。

| 画面の要素 | 必要な情報 | API |
| --- | --- | --- |
| アプリ一覧のホスト状態行、警告バナー | ログ退避の最終成功時刻、ディスク警告のしきい値、Caddy への設定投入の状態 | `GET /api/system` の `disk_limit_percent`、`log_archive`（`configured`、`last_success_at`、`last_error`、`next_run_at`）、`caddy`（`enabled`、`last_loaded_at`、`last_error`）、`swap`、`idle_sleep`（`available`。8.4） |
| アプリ一覧の行、詳細ヘッダー | プライマリ URL、検証待ちドメインの有無 | アプリの JSON の `primary_url`（プライマリドメインがあればその URL、なければ自動割当の URL）と `pending_domains`（検証待ちの数）。一覧のためにアプリごとにドメイン API を呼ばずに済む |
| 概要タブのサービス一覧 | メモリ使用量と上限 | `GET /api/apps/{slug}/services` の各サービスの `memory_bytes` と `memory_limit_bytes`（`docker stats --no-stream` の値。取れなければ `null`） |
| リソースタブ | 稼働中のリリース、サービスごとの今の CPU とメモリ、メモリの予約（6.2）と次のデプロイで付く予約、メモリの上限と予約の設定、メモリの履歴と上限・予約の目安、履歴の期間内のデプロイ（9.5） | アプリの JSON の `current_deployment` と `latest_deployment`、`GET /api/apps/{slug}/services` の `cpu_percent`、`memory_bytes`、`memory_limit_bytes`（同じ 1 回の `docker stats --no-stream`）、`memory_reservation_bytes`（稼働中のリリースの記録）、`next_memory_reservation_bytes`（次のデプロイで付く予約、6.2）、アプリの JSON の上限と予約の設定と、`GET /api/apps/{slug}/memory`、`GET /api/apps/{slug}/deployments?finished_since=<期間の始め>` |
| リソースタブの構成 | そのリリースの override、重ねた結果、コンテナと稼働中のリリースとの食い違い（9.10） | `GET /api/apps/{slug}/compose`、`GET /api/apps/{slug}/containers` |
| システムの状態のタブのメモリの予約 | 本番アプリの予約の合計と内訳、次のデプロイ後の合計と内訳、ホストの RAM、警告 | `GET /api/system` の `memory_reservation`（6.2） |
| デプロイタブの手順表示 | 手順の区切りと、手順ごとの所要時間 | デプロイログの手順の見出し行（`==> <RFC 3339 の時刻> <手順のキー> <表題>`）。UI はこの行でログを手順に分け、次の見出し（または終了）までを所要時間とする。手順のキーは `fetch` `prepare` `validate` `build` `pull` `backup` `start` `route` `check` `done` `cleanup`、blue/green の `switch` `services` `drain`（7.2）と、失敗時の `failed` `rollback`。DB に手順の表は持たない（ログファイルが唯一の記録で、SSE の再接続とも整合する）。自動ロールバックの結果も同じ理由でログの行から読み取り、デプロイの JSON にフィールドは持たない（UI が解釈する行は `docs/API.md`） |
| バックアップタブ | 次回の実行予定、スナップショットのサイズ | `GET /api/backups` の `next_run_at`、スナップショットの `size_bytes`（restic が記録していれば。なければ `null`）、`GET /api/apps/{slug}/volumes`（下記） |
| システムの状態のタブ | Docker のディスク内訳 | `GET /api/system/docker`（`docker system df` の結果: イメージ、コンテナ、volume、ビルドキャッシュそれぞれの件数と合計と、「不要なデータを削除」で空く量）。数秒かかりうるので `/api/system` とは分ける |

- `swap`（`total_bytes`、`used_bytes`。swap がない、または読めなければ `null`）は `/proc/meminfo` の `SwapTotal` と `SwapFree` から求める。ビルド時のピークに備えて swap を 2GB 置くので（10 章）、メモリと並べて使われ具合を見せる。
- `log_archive.next_run_at` は次にログ退避が走る時刻（未設定なら `null`）。退避の間隔は設定値で UI からは分からないので、バックアップの `next_run_at` と対にしてシステムの定期処理のタブの「次回」に出す。
- `GET /api/apps/{slug}/volumes` は、そのアプリの named volume の一覧（名前、バックアップから外しているか、Docker に実在するか）。バックアップタブで、一度もスナップショットを取っていない volume（DB のデータディレクトリなど、最初のバックアップより前に外したいもの）も選べるようにするため。
  - 「アプリの volume」の定義はバックアップと同じにする（10.2 の細則）。バックアップが使う列挙の処理（`internal/backup`）をそのまま呼び、別の定義を作らない。
  - 列挙は `docker volume ls` と `docker volume inspect` だけで、restic もオブジェクトストレージも使わない。バックアップが未設定のホストでも応答する。
  - 一度もデプロイしていないアプリは volume がまだないので、空の一覧を返す（エラーにしない）。
  - 除外リストにあるが Docker に実在しない名前（先に外しておいた、あるいは volume を消した）も `exists: false` として一覧に含める。UI に出ないと対象へ戻せないため。
  - volume の大きさは返さない。求めるにはディレクトリを歩く（`docker system df -v` も同じ）必要があり、タブを開くたびに走らせるには重い。スナップショットの `size_bytes` が目安になる。

バックアップの API（一覧、実行、リストア、ダンプのダウンロード）は 10.2 の内容で、形は `docs/API.md` にある。環境変数の画面の「再デプロイ」はデプロイの `POST` を呼ぶだけで、専用の API はない（保存とデプロイは分ける。9.2）。保存していない設定の変更があるときの扱いは上の「保存していない変更」のとおり。状態の自動更新はポーリング（一覧と詳細は数秒間隔、デプロイ中のログは SSE）で行い、状態変化を配信する専用の SSE は作らない（アプリ 3〜5 個の規模では不要）。

### 9.2 環境変数を 1 件ずつ確定する

環境変数は行ごとに編集して確定（保存）する。全件を一括置換する形だと、1 件だけ直したいときにも画面全体の「未保存の変更」を抱え、別のタブで開いた古い一覧で保存すると他の行の変更を消してしまうため。

- API は `PUT /api/apps/{slug}/env/{key}`（追加・変更、本文の `key` で名前の変更）と `DELETE /api/apps/{slug}/env/{key}`。名前の変更は 1 つのトランザクションで行い、「削除して追加」の途中の状態を作らない。一括置換の `PUT /api/apps/{slug}/env` は「.env 形式で編集」に使う。
- 保存はデプロイを伴わない。行ごとに「保存して再デプロイ」を付けると、3 件直せば 3 回デプロイが走るため。代わりに、DB の値と稼働中のリリースの値の差を「未反映の変更」として画面に出し、再デプロイのボタンはそこに 1 つだけ置く。
- 差を知るため、デプロイの手順 3 で env ファイルを作るときに、各変数の指紋（キーと値の HMAC-SHA256 を 16 バイトに切ったもの）を `deployments.env_fingerprints` に JSON で記録する。`GET /api/apps/{slug}/env` は、`current_deployment_id` のデプロイの指紋と今の DB を比べて、変数ごとの `pending`（`added` / `changed`）と、削除したがまだ稼働中の `removed` を返す。
  - 値そのものや素のハッシュは記録しない。短い値は素のハッシュから総当たりで戻せるため。HMAC の鍵はマスターキーから導出する（`HMAC(master_key, "koroku env fingerprint v1")`）。マスターキーを差し替えると比べられなくなるが、それは全件「変更」と出るだけで、再デプロイすれば揃う。
  - 稼働中のリリースがない（未デプロイ）か、指紋を記録していないデプロイが稼働中のときは比べられないので、`compared: false` を返し、UI は差を出さない。
  - 指紋は値を含まないので、再ログインから 10 分を過ぎて値を隠しているときも差は返す。
  - 指紋を取るのはアプリの変数だけで、koroku が足す変数（`env.release` のコミットの変数、`KOROKU_APP_URL` など。6 章）は比べない。コードだけのデプロイの直後や、ロールバックで別のコミットに戻した直後も、それだけで「未反映の変更」は出ない。
- 失うもの: 関連する変数（接続先とパスワードなど）を同時に変えられない。1 件目を保存した時点で、半端な組み合わせが DB に残る。デプロイは手動でしか始まらないので、その間に誰かがデプロイしない限り稼働中のアプリには影響しない。まとめて変えたいときは「.env 形式で編集」（一括置換）を使う。

### 9.3 コンテナのリソースの内訳

システムの状態のタブ（9.1）は、ホスト全体の使用量に加えて、稼働中のコンテナのメモリと CPU をアプリごとにまとめて出す。アプリ 3〜5 個を 4GB のホストに詰める構成では、どのアプリが使っているかが一番知りたいため。

- API は `GET /api/system/containers`。`docker stats --no-stream` は CPU 使用率を 2 回の計測の差から出すので、数秒かかる。`/api/system` に混ぜると一覧画面のホスト状態行まで遅くなるため、`/api/system/docker` と同じく分ける。
- 取り方は 3 つのコマンド。`docker stats --no-stream --no-trunc --format json`（稼働中の全コンテナ）と `docker info`（CPU のコア数と、Docker から見たメモリの総量）を並行に走らせ、終わってから stats に出たコンテナを `docker container inspect` で引く。inspect からは Compose のラベル（`com.docker.compose.project`、`com.docker.compose.service`）とメモリ上限（`HostConfig.Memory`）を取る。stats の出力にはラベルがなく、上限は「なし」でもホストのメモリ量が出て区別できないため。
- アプリへの振り分けは、プロジェクト名が DB にあるアプリの `koroku-<slug>` と一致するかで決める。それ以外（koroku の外で動かしているコンテナ、Compose 以外のコンテナ、手元の開発環境の `koroku-dev` のように `koroku-` で始まっても DB にないもの）は 1 つの「その他」にまとめる。合計が Docker 全体の使用量と一致するように、捨てずに出す。
- 稼働中のコンテナだけを出す（停止中のコンテナは stats に出ず、何も使っていない）。アプリのコンテナが 1 つも動いていなければ、そのアプリの行は出さない。
- アプリの行のアプリ名は、そのアプリのリソースタブ（9.5）へのリンクにする。この表を見て次に知りたいのは、どのサービスが使っているかと、使用量の推移と上限の目安で、それはリソースタブにあるため。
- **CPU は `docker stats` と同じく 1 コアを 100% とする値で返す**（`CPUPerc` そのもの）。2 コアのホストでは 200% まで振れる。`docker stats` や `top` で見慣れた読み方と同じ数字が出るほうが、手元で確かめた値と突き合わせやすいため。コア数は応答に含め、UI は「このホストは N コア、最大 N×100%」と書き添え、メーターは N×100% を満杯として描く（メモリやディスクと同じく 0〜100% の長さで比べられるように）。値は stats の 2 回の計測の間（約 1 秒）の瞬間値で、平均ではない。
- メモリの使用量は stats の値（cgroup の使用量からページキャッシュの非アクティブ分を除いたもの。`docker stats` の表示と同じ）。割合の分母は `docker info` のメモリ総量（VPS ではホストの `MemTotal` と同じ）。上限は `HostConfig.Memory` で、0（上限なし）は `null`。アプリの上限の合計は、そのアプリの稼働中のコンテナすべてに上限があるときだけ出す（koroku は override で全サービスに付けるので、普通は出る）。
- 競合: stats のあとで inspect する間にコンテナが消えると、inspect は `No such container` で失敗するが、残りのコンテナの出力は出す。その場合は出力を使い、消えたコンテナは除く（消えたものは何も使っていない）。stats と inspect が Docker デーモンのエラーで失敗したときは、`docker system df` と同じく数回やり直す。
- 履歴は持たない。画面を開いている間、ホストの状態と同じ 30 秒間隔で取り直す。
- 同じ画面に、本番アプリのメモリの予約の合計と内訳を出す（6.2）。こちらは稼働中のリリースの記録から求めるので Docker に問い合わせず、`/api/system` に入れる。

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
  - `include:`、`extends: { file: }` で参照する別ファイル。読むのは `compose_path`（未指定なら自動検出した 1 ファイル）だけ。Compose を koroku に書いたアプリでは、保存済みの本文だけ（6.3）。
  - `environment: [VAR]` のような値なしの受け渡し、リポジトリ内の `env_file:`、アプリのコードが直接読む変数。`koroku` は env ファイルで全変数を全サービスに注入するので、コードだけが読む変数は Compose に現れない。必要なら Compose に `${VAR:?...}` として書いてもらう。
  - 補間の中の不正な構文は、その参照を読み飛ばす（デプロイ時に `config` が失敗して分かる）。

**照合の結果**

- 照合するのは DB に保存済みの変数（稼働中のリリースではない）。「次にデプロイしたら足りるか」を知りたいため。値は使わず、キーの有無だけを見る。
- 「設定してあるが一覧にない変数」も返す。`dotenv` では「リポジトリから消えた変数」として行に印を付ける。`compose` では、アプリのコードが直接読む変数（Compose に現れない）が正常に多数あるので、画面には出さない。
- 環境変数の画面は、アプリの `default_branch` の最新コミットと照合する。デプロイの画面は、選んだブランチかコミットと照合し、足りない必須の変数があれば選択欄の下と、本番アプリの確認ダイアログに警告を出す。**デプロイは止めない**。`${VAR:?}` の形なら手順 4 の `config` が失敗して旧版が動き続けるし、意図して未設定のまま出すこともあるため。
- 照合元（`env_source`、`env_example_path`）は、環境変数タブの照合の結果の隣で選ぶ（9.1）。保存したら、その場で照合し直す。既定のブランチと Compose ファイルのパス（設定タブ）を変えたときは、次に環境変数タブを開いたときに新しい値で照合する。
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

### 9.5 メモリ使用量の履歴と上限の目安

koroku は全サービスにメモリ上限を付ける（6 章。既定 512m、アプリ全体とサービス別に変えられる）。上限を決める根拠として、使用量を時系列で記録し、上限の目安を出す。概要タブとシステム画面（9.3）はその瞬間の値しか出さないので、それだけでは夜間のバッチや週 1 回の集計で膨らむサービスを見落とし、上限に当たって初めて気づく。逆に余裕を持たせすぎた上限は、4GB のホストに 3〜5 アプリを詰めるときの見積もりを狂わせる。

**記録**

- `koroku serve` の中で 60 秒ごとに `docker stats --no-stream --no-trunc --format json` を 1 回走らせ、出たコンテナを `docker container inspect` で引く（9.3 と同じ 2 コマンドで、解析の処理も共用する。`docker info` は要らないので呼ばない）。Compose のプロジェクト名が DB のアプリの `koroku-<slug>` と一致するコンテナだけを、ラベルのサービス名で振り分ける。stats は CPU の計測のために 2 回測るので 1 回 2〜3 秒かかるが、60 秒に 1 回なら負荷は小さい。
- 5 分ごとのバケットにまとめて `memory_usage` テーブルに書く。列は `app_id`、`service`、`bucket_start`（5 分の区切り、Unix ミリ秒）、`max_bytes`、`sum_bytes` と `samples`（平均を出すため）、`limit_bytes`（そのバケットで最後に見た上限。上限がなければ NULL）、`restarts`。主キーは（`app_id`、`service`、`bucket_start`）で、アプリを消すと行も消える。
  - 書き込みは 1 回の計測につき 1 トランザクションで、サービスの数だけ upsert する（5 アプリ × 3 サービスで 15 行）。行数は 14 日 × 288 バケット × サービス数で、15 サービスでも 6 万行。生の 60 秒ごとの値は持たない（5 分の最大と平均で、上限を決める用途には足りる）。
  - 保持は 14 日。1 時間ごとに古いバケットを消す。ログのローカル保持（10.1）と揃える。
  - 同じサービスのコンテナが複数ある（`deploy.replicas` など）ときは、使用量の一番大きいコンテナの値を使う。上限はコンテナごとに効くため。
  - 使用量は `docker stats` の値（9.3 と同じく、ページキャッシュの非アクティブ分を除いたもの）。
- 再起動の回数: inspect の `RestartCount`（再起動ポリシーによる自動の再起動の回数。`docker compose restart` では増えない）を前回の計測と比べ、増えた分をバケットの `restarts` に足す。上限に当たって OOM killer に止められたコンテナは `restart: unless-stopped`（6 章）で再起動するので、その目印になる。前回の値はプロセス内にだけ持ち、コンテナが新しくなった（デプロイ）ときと `koroku` の起動直後は、比べる相手がないので数えない。
  - OOM kill そのもの（`State.OOMKilled`）は記録しない。コンテナが再起動するとこの値は戻るので、稼働中のコンテナを 60 秒ごとに見る方法では拾えない。拾うには `docker events` を常時購読する必要があり、そこまではしない。画面では「自動の再起動」として出し、OOM と断定しない。
- 失敗: Docker に問い合わせられなければ、その回は何も書かずに次の回を待つ（ログに警告。続けて失敗している間は繰り返さない）。koroku が止まっていた時間はバケットが欠けるだけで、補わない。

**API**

`GET /api/apps/{slug}/memory?range=24h|7d|14d`（既定 `24h`）。形は `docs/API.md`。サービスごとに、表示用の系列、期間内のピーク、上限の目安を返す。

- 系列は期間に応じて間引く: 24 時間は 5 分ごと（288 点）、7 日は 30 分ごと（336 点）、14 日は 1 時間ごと（336 点）。間引いた点は最大の最大、平均の平均（サンプル数で重み付け）、上限は最大。
- サービスの一覧は、期間内にデータがあるサービス。Compose から消えたサービスも、期間内にデータがあれば出る。

**目安の決め方**

目安は表示の期間とは別に、**いまの上限になってからの、直近 14 日のデータ**（以下「根拠」）から出す。期間を切り替えるたびに目安が変わらないように。上限を変えてデプロイすると根拠は数え直しになる。上限が違えば使い方も変わる（PostgreSQL や JVM は上限に合わせて確保する量を変え、上限の近くではカーネルがキャッシュを返させる）ので、古い上限のもとでの値は比べられないため。

- 根拠の長さは、データのあるバケットの数 × 5 分（止めていた時間と、休止していた時間（8.4）は数えない）。
- **目安の値**: 根拠のピーク × 1.5 を 64MiB 単位に切り上げ、128MiB を下限にする。設定欄に入れるときは `m` 単位の文字列（例 `640m`）にする。根拠が 24 時間分に満たなければ出さない（1 日の波を一周見ていないため）。
- **「上限に近い」**: 根拠のピークが上限の 90% 以上。根拠の長さによらず出す（1 回でも当たりかけたのは事実なので）。
- **「余裕が大きい」**: 根拠が 7 日分以上あり、ピークが上限の 30% 未満。週 1 回の処理を見落とさないよう 7 日を待つ。
- ピークのほかに、5 分ごとの最大の 95 パーセンタイルも返す。「たまにだけ跳ねる」のか「いつも高い」のかを画面で見分けるため。目安の値にはピークを使う（上限は超えたら止まる値なので、まれな山にも合わせる）。
- 上限のないバケット（koroku の外で付けた上限を外したなど。通常は起きない）は「上限に近い」「余裕が大きい」を判定しない。

**画面**

アプリ詳細の「リソース」タブに、サービスごとの今の CPU とメモリと、「メモリの履歴」を置く。使用量を確かめるのは設定を変えるときに限らず、閲覧のロールでも見たいものなので、設定タブとは別のタブにする（概要タブはデプロイの状態を見に来る場所で、ここにグラフを置くと静かでなくなる）。メモリの上限と予約（アプリ全体とサービス別）を変えるのもリソースタブだけにする。上限と予約は使用量と履歴を見て決めるもので、設定タブにも同じ欄があると、どちらで変えたものが効いているのか分からなくなるため。設定タブには保存済みの値の一覧と「リソースで変える」のリンクだけを置く（9.1）。

- 今の値は、サービスごとの CPU（1 コアを 100% とする `docker stats` の値）とメモリ（上限に対する割合）。止まっているサービスは値を出さない。開いている間は数秒ごとに更新する。
- 履歴は、サービスごとに、使用量（5 分ごとの最大）と上限の推移の小さな折れ線、期間内のピーク、自動の再起動の回数、目安を出す。期間は 24 時間 / 7 日 / 14 日から選ぶ。グラフにはポインタとキーボードで時刻ごとの値を出す。
- 「メモリの上限と予約」の節に、アプリ全体（全サービス共通）の上限と予約、サービス別の上限と予約の一覧を置く。値ごとに「変更」（ダイアログで 1 つの値を保存）、サービス別の値には「外す」（全体の値か既定に戻す）、「サービス別の上限を追加」「サービス別の予約を追加」（サービス名は稼働中のサービスから選べるが、まだないサービスの名前も入れられる）を置く。どれも `PATCH /api/apps/{slug}` に、変える 1 つの項目だけを送る（サービス別なら、ほかのサービスの値はそのままで 1 つを足すか書き換えるか消す）。書式は `512m` の形（予約は予約しない `0` も）で、画面が書式を確かめてから送り、API の 422 の理由は欄の下に出してダイアログを閉じない。予約の欄は、本番アプリと、予約の設定が残っている staging アプリにだけ出す。変更は次のデプロイから反映される、とダイアログと節の説明に書く。ボタンは設定を変えられるロール（管理以上、10.7）にだけ出し、ほかのロールには値だけを見せる。
- 目安の横の「上限を <値> にする」は、確認のダイアログのあと、そのサービスの上限を保存する（`PATCH /api/apps/{slug}` の `service_memory_limits` に、ほかのサービスの上限はそのままで 1 つを足すか書き換える）。上限の変更は次のデプロイから反映される、とダイアログと保存後の表示に書く。ボタンは設定を変えられるロール（管理以上、10.7）にだけ出す。目安がいまの上限と同じなら「いまの上限で足りています」と出し、ボタンは出さない。
- 本番アプリでは、今の値の行にサービスごとの「予約」の欄を置く（6.2）。稼働中のリリースの予約（`memory_reservation_bytes`）と、その値がどの設定から来るか（サービス別の値 / 全体の値 / 既定の上限の半分 / 予約しない）を出し、保存済みの設定で次のデプロイに付く値が稼働中と違えば、それを「次のデプロイから」として並べる。
  - 次の値は API の `next_memory_reservation_bytes`（稼働中のリリースの形に今の設定を当てた値、6.2）。それが `null`（形を記録する前のデプロイが稼働中）のときだけ、画面が保存済みのアプリ設定から 6.2 と同じ順（サービス別、全体、上限の 50% を MiB に切り捨て）で見積もり、上限までに切り詰め、6MiB 未満は予約なしとする。見積もりに使う上限は、アプリ設定の上限（サービス別、なければ全体）があればそれ、なければ稼働中のコンテナの上限。どちらも分からない（止まっていて上限の設定もない）ときは、値を出さずに「上限の半分」とだけ書く。見積もりはワンショットのサービスを見分けられないので、終了しているサービスには出さない。
  - 「変更」はダイアログを開き、そのサービスの予約を保存する（`PATCH /api/apps/{slug}` の `service_memory_reservations` に、ほかのサービスの値はそのままで 1 つを足すか書き換える）。書式は「メモリの上限と予約」の節と同じ（`256m` の形か、予約しない `0`）で、画面が書式を確かめてから送る。上限を超える値と 6MiB 未満は API の 422 の理由を欄の下に出し、ダイアログを閉じない。サービス別の値があるときは「既定に戻す」でそのキーを消し、全体の値か上限の半分に戻す。ボタンは設定を変えられるロール（管理以上、10.7）にだけ出す。
  - ダイアログには予約の目安を出し、押すと欄に入る。根拠は上限の目安と同じ（いまの上限になってからの直近 14 日、24 時間分に満たなければ出さない）で、5 分ごとの最大の 95 パーセンタイルを 64MiB 単位に切り上げ、次の上限までに切り詰めた値。ピークではなく 95 パーセンタイルにするのは、予約は超えたら止まる値ではなく、ふだんの使用量を回収から守る値で（6.2）、まれな山に合わせると予約の合計が膨らみ、ほかのアプリの保護を削るため。超えた分も使えて、回収の順が後回しでなくなるだけである。目安は開いたときに `GET /api/apps/{slug}/memory` で読み、読めなければ出さない。
  - 予約の変更は次のデプロイから反映される、とダイアログに書く（システム画面の予約の合計がデプロイまで変わらないことは、システム画面の内訳の「次のデプロイから」が示す）。
  - staging アプリには変更の操作を出さず、予約が付かないことを書く。本番から切り替えてまだデプロイしていないアプリは、稼働中のリリースの予約と「次のデプロイで外れます」を出す。
- 記録が 24 時間分に満たない間は「記録中」として、たまった時間と、目安が出るまでの残りを出す。データが 1 件もなければ空の状態（「デプロイして稼働すると、5 分ごとに記録します」）。
- 管理 UI にはグラフを置かないが、この機能は推移を見ることそのものが目的なので、この 1 か所に限ってグラフを置く。色は使用量にアクセント色 1 つ、上限は線の灰色で、凡例を付ける。
- 「今の使用量」の節の頭に「稼働中のリリース」を 1 行置く: 短い SHA（GitHub のリポジトリならコミットのページへのリンク）、コミットのメッセージの 1 行目、ブランチ、デプロイした時刻、デプロイタブのその記録へのリンク。今の値がどのリリースのものかを、概要タブへ移らずに読めるようにするため。最新のデプロイがこのリリースへのロールバック（7.1）なら、時刻はロールバックした時刻にし、「ロールバック」と書く（`current_deployment` は戻す先のデプロイを指し、その時刻は最初にデプロイした時刻のため）。止まっているときと休止中は書き出しをそれぞれ「停止中のリリース」「休止中のリリース」にする。

**履歴の中のデプロイ**

メモリの履歴の各グラフに、期間内にデプロイで動くものが変わった時刻を印で出す。使用量の段差や傾きの変わり目がどのリリースから始まったかを、デプロイタブと見比べずに読めるようにするため。

- **印を置くデプロイ**: 終わったデプロイのうち、稼働中のコンテナを置き換えたもの。
  - 成功したデプロイ。push の自動デプロイ（5.2）、ロールバック（7.1）、昇格（9.7）を含む。どれも稼働中のリリースを入れ替える。
  - 起動の手順（7 章の手順 6）より後で失敗かキャンセルになり、直前のリリースへ戻したデプロイ。新しい版が一度動き、戻すときにもコンテナを作り直すので、使用量の山や谷を作りうる。成功と見分けられるように、印の形と線を変える（成功は塗りつぶした三角と実線、戻したものは白抜きの三角と破線。色も失敗の赤にするが、色だけには頼らない）。
  - 手順 5 までに失敗したデプロイ（ビルドの失敗など）は出さない。コンテナに触れていないので、使用量とは関わらない。
  - 見分けには `deployments.containers_replaced` を使う。デプロイの実行が終わったときに、稼働中のコンテナに触れたか（手順 6 に入ったか）を記録する。デプロイログを読まずに分かるようにするため。実行の途中で `koroku` が止まったもの、実行の前に取り消されたものは `NULL` で、失敗でも印を出さない。
- **出さないもの**: 起動・停止・再起動、休止と起床（8.4）。止めていた時間と休止の間は記録がなく、グラフの線が途切れるので分かる。再起動は同じリリースを動かし直すだけで、動くものは変わらない。休止と、アクセスでの起床は操作の記録（10.7）にも残らず、出しても一部になる。
- **時刻と位置**: 印の時刻はデプロイの終わった時刻（`finished_at`）。コンテナが置き換わるのは確認の手順の前だが、差は 1 分ほどで、5 分ごとのバケットでは見分けられない。位置は期間の始めと終わりに対する時刻の比で決め（タイムゾーンによらない）、期間の外のものは出さない。表示する時刻は JST。
- **形**: 各グラフには縦線だけを引き、印のボタンはサービスの一覧の上の「デプロイ」の列に 1 度だけ置く。列はグラフと同じ横幅と時間軸で、縦線と揃う。デプロイはアプリ全体に効くので、サービスごとに同じボタンを並べてキーボードの移動を増やさないため。
- **詳細**: 印にポインタを載せるか、フォーカスするか、タップすると、短い SHA とコミットのメッセージ、ブランチ（ロールバックは戻す先の番号、昇格は昇格元のアプリ）、何が始めたか（手動ならユーザー、push の自動デプロイなら push した人）、終わった時刻、結果（成功 / 失敗して戻した / キャンセルして戻した）、いま稼働中のリリースか、を出し、デプロイタブのそのデプロイ（`?deployment=<id>`）へのリンクを置く。Escape で閉じる。ボタンの名前に時刻と種類と SHA を入れ、スクリーンリーダーでも一覧として読める。
- **近い印**: 列の上で 24px 未満の間隔の印は 1 つにまとめて件数を出し、開くとまとめたデプロイを新しい順に並べる。14 日の期間や 390px の幅でも、印が重なって押せなくならないように（押せる大きさは 24px 四方）。
- **読み込み**: `GET /api/apps/{slug}/deployments?finished_since=<期間の始め>&limit=200` を、期間を切り替えるたびにメモリの履歴と並べて読む。期間の外のデプロイを読まないため。200 件で切れたら、新しい 200 件だけを出していることを列の下に書く。デプロイの履歴は消さないので、14 日の期間でも記録はそろっている。読めなかったときは印を出さず、グラフはそのまま出す。

**やらないこと**

- CPU の履歴。`docker stats` の同じ出力に CPU もあるので記録は容易だが、上限を付けていない（6 章）ので目安を出す先がない。必要になったら同じテーブルに列を足す（後続の候補）。
- ホスト全体のメモリの履歴。アプリの合計とは別に、swap の推移と合わせて見たくなったら足す。
- 上限の自動調整。上限を変えるのはデプロイで、デプロイは手動でしか始まらない（1 章）。

### 9.6 ワンオフのコマンドと登録タスク

`bin/rails db:migrate:status` や `bin/rails runner '...'` のように、稼働中のアプリと同じイメージ・環境変数で 1 回だけコマンドを走らせたいことがある。管理 UI になければ、ホストに SSH して `docker compose` を手で組み立てることになり、env ファイルやリリースのディレクトリの場所を知っている必要がある。Heroku の `heroku run`、Dokku の `run` に当たるものを、対話端末なしで管理 UI から使えるようにする。対話的に使うもの（`rails console`、`psql`）は、ホストで `koroku run`（下記）を使う。

**実行の仕方**

```text
docker compose -p koroku-<slug> -f <リリースの Compose ファイル> -f <リリースの override> --env-file <apps/<slug>/env> \
  run --rm --no-deps -T --name koroku-<slug>-task-<run_id> <サービス> sh -c '<コマンド>'
```

- 稼働中のリリース（`current_deployment_id`）のファイルをそのまま使う。イメージ（ビルドしたものはリリース固有のタグ）、env ファイル、メモリ上限が稼働中のコンテナと同じになる。リリースのファイルがディスクにない（未デプロイ、または消えた）ときは `409 not_deployed` で断る。
- アプリの起動（10 章）と同じく、使う直前に `config` とポリシー検査をやり直す。通らなければ `422 policy_violation`。サービス名が正規化した結果にない場合は 422（`validation_failed`、`service`）。
- `--no-deps` で依存サービスを起動しない。DB などは稼働中のコンテナにつながる。停止中のアプリでも実行はできる（コンテナは作られる）が、依存先が止まっていればコマンドの側で失敗する。画面にそう書く。休止中のアプリ（8.4）は、実行の前に起こす。
- `compose run` は `--service-ports` を付けない限りポートを publish しないので、稼働中の公開サービスと割当ポートを取り合わない。
- 標準入力は渡さず（`-T`、入力は空）、端末も割り当てない。対話的なコマンド（`rails console`、`psql` の対話モード）は使えない。入力を待つコマンドは入力の終わりを受け取って終わる。
- コマンドは `sh -c` に 1 つの引数として渡す。イメージに `sh` が要る（ディストロレスのイメージでは使えない）。koroku はシェルの文字列を組み立てない（ユーザーの入力をそのまま 1 つの引数にする）。
- コンテナ名を `koroku-<slug>-task-<run_id>` に固定する。キャンセルやタイムアウトで `docker compose run` のプロセスを止めても、コンテナは動き続けるため（CLI が消えるだけ）。止めたあとは `docker rm -f` でその名前のコンテナを消す。
- アプリ同士のリンク（6.1）のネットワークには、この実行のコンテナをつながない。リンク先の名前を引くコマンドは失敗する。

**登録タスク**

- アプリごとに、名前付きのコマンド（名前、サービス、コマンド）を登録しておき、ボタン 1 つで実行できるようにする。テーブル `app_tasks`（`app_id`、`position`、`name`、`service`、`command`）。名前はアプリの中で一意で 60 文字以内、コマンドは改行を含まない 2000 文字以内、1 アプリ 20 件まで。
- 編集は一覧ごとの置き換え（`PUT /api/apps/{slug}/tasks`）。`apps` の列（JSON）にせず別の表にしたのは、アプリの JSON（一覧画面で毎回返す）を太らせないためと、アプリ設定の `PATCH` とは確認の条件（下記）が違うため。
- 登録タスクの実行も、その場で入力したコマンドの実行も、定期実行（下記）も、同じ実行記録になる。記録にはタスク名（その場のコマンドなら空）、サービス、コマンドの文字列をその時点のまま残す。後でタスクを書き換えても過去の記録は変わらない。

**定期実行**

アプリの中のスケジューラー（cron のコンテナ、ジョブキューの定期実行の機能）は、アクセスがない間の休止（8.4）で止まると動かない。登録タスクに予定を書けば、koroku 自身がその時刻にタスクを実行する。休止中のアプリでも、タスクが要るコンテナだけを一時的に起動して実行する。

- 予定は登録タスクごとの任意の項目（`app_tasks.schedule`、空なら定期実行しない）。cron の 5 つの欄（分 時 日 月 曜日）で書く。各欄は `*`、数、範囲（`1-5`）、間隔（`*/15`、`0-30/10`）、それらのカンマ区切りの並び。分は 0〜59、時は 0〜23、日は 1〜31、月は 1〜12、曜日は 0〜7（0 と 7 が日曜）。月や曜日の名前（`JAN`、`MON`）と `@daily` などの略記は受け付けない。日と曜日の両方を `*` 以外にしたときは、どちらかに合えば実行する（cron の慣習どおり）。
- 時刻は `config.toml` の `timezone`（既定 `Asia/Tokyo`）で読む。日次のバックアップ（10.2）とログの退避の時間枠（10.1）と同じ設定で、ホストの時刻の設定には依らない。夏時間のある地域を設定した場合、その日に存在しない時刻の予定は飛ばし、2 回ある時刻は 1 回だけ実行する。
- 保存のときに検査する。書式の誤り、範囲外の数、一度も来ない日付（`0 0 31 2 *`）は `422 validation_failed`（`tasks[N].schedule`）。API はタスクごとに次の実行予定の時刻を返し、画面に出す。
- 予定の変更は登録タスクの変更（`PUT /api/apps/{slug}/tasks`）なので、本番アプリでは直近のログインが要る（下記「本番アプリの保護」）。画面は、本番アプリで予定を加えるか変えて保存するときに確認ダイアログを出す。予定の時刻の実行そのものには確認もログインも求めない（人がいない時刻に動くもののため）。

**定期実行のしかた**

- `koroku serve` の中の 1 つの見張りが、15 秒ごとに時計（テストでは差し替える）を見る。登録タスクの一覧は毎回 DB から読み、タスク（アプリ・名前・予定の組）ごとに次の時刻をメモリに持つ。時刻を過ぎたら待ち行列に入れ、次の時刻を求め直す。`koroku` の起動時と、予定を加えたり変えたりしたときは、その時点より後の最初の時刻から数える。`koroku` が止まっていた間の予定は、あとから実行しない（バックアップと同じ）。
- 待ち行列は全アプリで 1 つで、1 件ずつ順に実行する。複数のアプリの予定が同じ時刻に重なっても、ホストのメモリを超えて同時に動かさないため。管理 UI からの実行はこの行列に並ばない（これまでどおり、そのアプリが空いていればその場で始める）。
- 実行は管理 UI からの実行と同じ手順・同じ実行記録で、記録の `trigger_kind` が `scheduled`（管理 UI からは `manual`）になる。実行した人は記録しない。タイムアウト（`[tasks] timeout_minutes`）、キャンセル、出力のマスク、50 件の保持も同じ。ログの先頭に、どの予定の時刻の実行かを 1 行書く。
- 実行するときは、そのタスクを DB から読み直す。消えていれば何もしない。サービスやコマンドが変わっていれば、いまの内容で実行する。
- 起きているアプリでは、管理 UI からの実行と同じく `run --rm --no-deps -T` で実行する。休止中のアプリ（8.4）では起こさず、タスクのサービスの依存先だけを起動する。依存先は、稼働中のリリースを正規化した Compose の設定（`config` の結果）で、そのサービスの `depends_on` を推移的にたどったサービス（タスクのサービス自身は含めない）。`docker compose start --wait --wait-timeout <up の待ち時間> <依存先>...` で起動し、コンテナが 1 つもない（ホストで手で消したなど）ときだけ、稼働中のリリースのファイルで `up -d --no-deps --wait <依存先>...` にする。タスクのあと（失敗・キャンセル・タイムアウトでも）`docker compose stop` でプロジェクトを止め直す。依存先がなければ何も起動しない。DB につなぐタスクのサービスには、DB のサービスを `depends_on` に書いてもらう（Skill で案内する）。
- 依存先を起動できなかったら、その実行は `failed`（`error` に理由）にする。`koroku` の再起動で中断された実行のアプリが休止中なら、起動時に `docker compose stop` で止め直す（依存先が動いたまま残らないように）。

**見送る場合**

- アプリが停止中（`desired_state = stopped`）なら実行しない。利用者が止めたアプリの中身を、予定の時刻に動かさないため。未デプロイ（稼働中のリリースのファイルがない）のアプリも実行しない。
- 同じタスクの前の回が、まだ待ち行列にあるか実行中のうちに次の時刻が来たら、その回は見送る。長引いた実行が積み重ならないため。
- そのアプリでデプロイが `queued` か `running`、ほかの操作や管理 UI からの実行の最中（`busy`）なら、待ち行列に残したまま、後ろのほかのアプリの予定を先に実行する。予定の時刻から 10 分たっても空かなければ見送る。デプロイや実行は最長 30 分続き、待ち続けるとほかのアプリの予定まで遅れるため。
- 実行の前の検査（ポリシー検査、サービスがリリースにない）に通らなければ見送る。
- 見送ったことは、そのタスクの行に直近の 1 件だけ残す（`app_tasks.last_skipped_at` と `last_skip_reason`。理由は `stopped`、`not_deployed`、`overlap`、`busy`、`error`、`error` のときは `last_skip_detail` に文）。実行記録には残さない。停止中のアプリの予定は時刻のたびに見送るので、記録に残すと新しい 50 件が見送りで埋まり、実際に実行した記録が消えるため。サーバーログには毎回残す。登録タスクを保存し直しても、名前と予定が同じタスクの見送りの記録は残す。
- 操作の記録（10.7）には、定期実行も見送りも残さない。実行記録が記録で、予定の時刻のたびに増えるものを操作の記録に入れると溢れるため。予定の変更は、登録タスクの変更（`tasks.changed`）として残る。

**実行記録とログ**

- テーブル `task_runs`: `id`、`app_id`、`task_name`、`service`、`command`、`deployment_id`（実行したリリース）、`status`、`exit_code`、`error`、`started_at`、`finished_at`、実行した人（`actor_user_id`、`actor_login`）、何が始めたか（`trigger_kind`、`manual` か `scheduled`）。
- 状態は `running → succeeded | failed | canceled`。デプロイと違って `queued` を持たない。管理 UI からの実行は待たせずにその場で始めるか、`409 busy` で断るため（下記）。定期実行の待ち行列はメモリにだけあり、始めたときに記録を作る。終了コード 0 で `succeeded`、それ以外は `failed`（`error` に `exited with code N`）。タイムアウトは `failed`（`timed out after 30m0s`）、キャンセルは `canceled`。`koroku` の再起動で中断されたものは、起動時に `failed`（`interrupted`）にする。
- 出力（stdout と stderr）は `apps/<slug>/tasks/<run_id>.log` に追記する。デプロイログと同じ書き出し（行単位のマスク、1 行 64 KiB、1 回 50 MiB で打ち切り）と、同じ形の SSE（行ごとのメッセージ、`id:` はバイト位置、終わると `event: end`）で配信する。UI はデプロイログの表示部品を使い回す。
- ログの先頭に `$ <コマンド>` と、どのサービス・どのリリースで実行したかを書き、終わりに `Exited with code N.` などの結果を書く。
- マスクするのは DB に保存した環境変数の値と、稼働中の env ファイル（`apps/<slug>/env`）の値。コンテナが受け取るのは後者で、DB から消した値がまだ残っていることがあるため。env ファイルは koroku 自身が書いた形式（6 章の引用規則）なので、その逆変換で読む。
- コマンドの文字列は秘密として扱わない（DB に平文で残し、画面に出す）。値をコマンドに直接書くと、ログの上ではマスクされるが、記録と画面には残る。値は環境変数として渡すよう画面に書く。
- アプリごとに新しい 50 件を残し、古い記録はログファイルごと消す。

**同時実行**

- 実行中は、そのアプリの操作ロック（10 章「起動制御と削除」と同じもの）を持ち続ける。起動・停止・再起動・削除・リストア・別の実行は `409 busy` になる。
- 実行の記録を終わりの状態（`succeeded` / `failed` / `canceled`）にするのは、操作ロックを手放し、実行中に保留した自動デプロイ（5.2）をキューに積んだあと。記録が終わりになった時点で、そのアプリが次の操作（実行、デプロイ、起動制御）を受け付けるようにするため。
- デプロイが `queued` か `running` のアプリでは実行を断る（`409 busy`）。逆に、実行中のアプリへのデプロイの依頼（7.1 のロールバックを含む）も `409 busy` で断る。push での自動デプロイ（5.2）だけは断らずに預かり、実行が終わってから積む（GitHub は断られた配送を自動では送り直さないため）。手動操作の実行中に始まったデプロイは操作の完了を待つ（10 章）が、実行は最長 30 分続き、デプロイのワーカーは全体で 1 つなので、待つと他のアプリのデプロイまで止まるため。判定と記録は 1 つのロックの中で行い、両方が同時にすり抜けないようにする。
- 別のアプリの実行やデプロイとは並行して動く。ビルドと違い CPU を長く使う前提ではなく、メモリはサービスの上限で抑えられるため。
- タイムアウトは既定 30 分（`config.toml` の `[tasks] timeout_minutes`、1〜1440）。キャンセルは `POST /api/task-runs/{id}/cancel`。

**本番アプリの保護**

- 本番アプリ（`environment = production`）では、実行の前に確認ダイアログを出し、実行と登録タスクの変更（定期実行の予定を含む）に直近 10 分以内のログインを求める（`403 reauth_required`、10.3）。マイグレーションや `runner` は本番のデータを書き換えられ、デプロイと同じかそれ以上に影響が大きいため。staging ではどちらも求めない（デプロイと同じ扱い）。

**画面**

- アプリ詳細に「タスク」タブを置く（9.1）。実行の履歴とライブログを持つので、既存のタブ（設定やデプロイ）に入れると縦に長くなりすぎるため、独立したタブにする。
- 上から、登録タスクの一覧（各行に「実行」と、予定があれば予定の式・次の実行予定・直近に見送った予定と理由）、その場のコマンドのフォーム（サービスの選択とコマンド）、実行の履歴（状態・タスク名かコマンド・定期実行の印・終了コード・所要時間・開始）。登録タスクの編集の行に「予定」の欄を置き、書き方の短い説明と、時刻を読むタイムゾーンを添える。履歴の行を開くとログを出し、実行中は SSE で追い、「キャンセル」を置く。登録タスクの編集は同じタブの中で行う。

**API**（形は `docs/API.md`）

| メソッドとパス | 用途 |
| --- | --- |
| `GET / PUT /api/apps/{slug}/tasks` | 登録タスクの一覧、一括置き換え |
| `GET /api/apps/{slug}/task-runs?limit=50` | 実行の履歴（新しい順） |
| `POST /api/apps/{slug}/task-runs` | 実行。`{"task": "<名前>"}` か `{"service": "...", "command": "..."}`。202 で実行記録 |
| `GET /api/task-runs/{id}`、`GET /api/task-runs/{id}/logs`（SSE、`?format=text`）、`POST /api/task-runs/{id}/cancel` | 記録、ログ、キャンセル |

- 不採用案: 稼働中のコンテナで `docker compose exec` する。コンテナが動いていないと使えず、`restart: unless-stopped` の公開サービスの中で長いコマンドを走らせるとデプロイの入れ替えで途中で殺される。ダンプフック（10.2）が `exec` なのは、DB のサービスのデータに触れる必要があるため。
- 不採用案: 管理 UI の対話端末（WebSocket で pty をつなぐ）。端末の中継とその記録・マスクを作り込む必要があり、個人用途の手間に見合わない。端末の出力は入力のエコーと制御文字が混ざり、値が行の途中で分かれるので、行単位のマスクも効かない。管理 UI への WebSocket は Caddy の設定の入れ替え（8 章）で切れる。対話的な用途は、SSH でホストに入って `koroku run` を使う。

**ホストから対話的に実行する（`koroku run`）**

```text
sudo koroku run [-config <path>] <slug> <サービス> [<コマンド> [<引数>...]]
```

- タスクの実行と同じリリースのファイル、env ファイル、`config` とポリシーの再検査を使い、`docker compose ... run --rm --no-deps <サービス> <コマンド>...` を実行する。リリースのファイルの場所を知らなくても、稼働中と同じイメージ・環境変数・メモリ上限のコンテナに入れる。コマンドを省くと `sh`。
- 端末は `docker` の子プロセスにそのまま渡す（`koroku` は中継しない）。標準入力が端末なら Compose が TTY を割り当てる。端末でなければ `-T` を付け、パイプの入力をそのまま渡す（`koroku run app db psql -U postgres app < dump.sql`）。終了コードはコマンドのものを返す。`Ctrl-C` などのシグナルは `koroku` が受けても無視し、`docker` に任せる（`koroku` が先に `docker` を止めると、コンテナが残るため）。
- コマンドは引数の配列として渡し、`sh -c` で包まない。ホストのシェルが一度解釈するので、パイプや `$VAR` をコンテナの中で使うときは `sh -c '...'` を自分で書く。
- 実行するのはホストの root（`sudo`）。リリースのファイル、env ファイル、DB はどれも root だけが読める。DB は読み取り専用で開き、マイグレーションを当てない。DB のスキーマのバージョンがバイナリのものと違えば断る（バイナリを入れ替えて `koroku` を再起動していない間に、古いスキーマを読み違えないため）。
- 管理 UI の実行と違い、操作ロックを取らず、実行記録（`task_runs`）もアクティビティも残さず、出力をマスクも保存もしない。ホストの root はもともとすべてのアプリと秘密を握っていて、記録やマスクで守れる相手ではないため。デプロイ・停止・削除はこのコンテナを待たない。デプロイで入れ替わっても、このコンテナは起動したときのイメージのまま動き続ける。本番アプリでは、実行の前にそのことを標準エラーに 1 行出す（確認は求めない。スクリプトから使えるようにするため）。
- アプリ同士のリンク（6.1）のネットワークにはつながない（タスクの実行と同じ）。
- 管理 UI のタスクタブの「コマンドを実行」に、選んだサービスと入力したコマンドから組み立てた `sudo koroku run ...` の 1 行を、コピーボタン付きでオーナーにだけ出す。ホストに SSH できるのはオーナーだけで、メンバーには使えないため。コマンドにシェルの特殊文字（`$`、`` ` ``、`|`、`&`、`;`、`<`、`>`、`(`、`)`、`*`、`?`、`[`、`]`、`{`、`}`、`~`、`#`、`!`、`\`）がなければ入力のまま後ろに付ける（引用符はホストのシェルが外し、同じ引数になる）。あれば `sh -c '<コマンド>'` に包み、ホストのシェルに解釈させない。

### 9.7 staging のリリースを本番へ昇格する

8.2 のとおり staging と本番は別アプリのまま、Heroku Pipelines の promote に当たる操作を持つ。staging で確かめたコミットを本番に出すのに、本番のデプロイ画面で同じ SHA を探して選び直すと選び間違いの余地があり、`build:` のあるサービスは本番でもう一度ビルドすることになって、ビルドの時間と負荷（7 章: 他アプリの応答が遅くなりうる）が 2 回かかる。昇格は staging の稼働中のリリースをそのまま指して本番へ出し、ビルド済みのイメージを使い回す。

**組み合わせ（アプリ設定）**

- 本番アプリに「昇格元」として staging アプリを 1 つ指定できる（`apps.promote_from_app_id`、API では slug の `promote_from`）。指定できるのは、自分が `production` で、相手が `staging` かつ同じリポジトリ（`repo_full_name` か `repo_url` が一致）のアプリだけ。自分自身は指定できない。
- 選ぶのは本番アプリのデプロイタブの「デプロイの設定」（9.1）。昇格の欄と同じタブにあり、組み合わせがないときは欄からその節へつなぐ。変えられるのはオーナーだけ（10.7）。
- 指定したあとで環境やリポジトリを変えると、組み合わせは残るが昇格は「使えない」と表示され、受け付けない（`PATCH` で一緒に検査すると、設定の順番によっては変えられなくなるため）。staging アプリを削除すると指定は外れる（`ON DELETE SET NULL`）。

**昇格**

- 本番アプリの画面で「staging のリリースを本番へ」を押すと、staging アプリの稼働中のリリース（`current_deployment_id` のデプロイ）の SHA を、本番アプリに通常のデプロイとして積む。キュー、デプロイログ、手順 6 以降の失敗でのロールバック、本番のデプロイに要る再ログイン（10.3）はすべて通常どおり。デプロイには昇格元のデプロイ ID（`deployments.promoted_from_deployment_id`）を記録し、履歴に「staging から昇格」と出す。
- 受け付けるのは、指定したデプロイが昇格元アプリの「今の」稼働中リリースであるときだけ。確認ダイアログを開いている間に staging が新しく出ていたら `409 promotion_stale` にして、画面は内容を読み直す（確かめた差分と違うものが本番に出ないように）。
- 昇格は取得からやり直すので、昇格先のアプリのリポジトリの ID の確かめ（5 章）と、コミットがブランチかタグからたどれるかの確かめ（7 章「デプロイできるコミット」）を受ける。照合するのは昇格元のデプロイの `ref`（staging に出したときのブランチかタグ。組み合わせは同じリポジトリに限るので、本番から見ても同じ ref）で、昇格のデプロイの `ref` にもそれを記録する。staging に出したあとで、そのブランチが force-push されてコミットが外れた、ブランチが消された、というときは昇格が取得の手順で失敗する。staging にそのブランチのいまのコミットを出し直してから昇格する。
- 本番の環境変数・volume・ドメインはそのまま。動かすのはコードとイメージだけ（Heroku と同じ）。
- staging が 7.1 のロールバックで前のリリースに戻っているとき、staging の `current_deployment_id` はロールバックの記録ではなく戻した先のリリースを指す。昇格はそのリリースのコミットを出し、そのリリースのイメージを使い回し、昇格元としてもそのリリースのデプロイ ID を記録する。staging で今動いているものがそのまま本番に出る。
- 昇格で積んだデプロイは、本番の通常のリリースとして保持する（リリースのディレクトリ、本番の名前を付けたイメージ、環境変数のスナップショット。7 章）。7.1 のロールバックの戻す先にもなり、戻すときに staging のイメージには頼らない。ロールバックの記録は昇格の記録を引き継がない（`promoted_from_deployment_id` は空）。

**ビルド済みイメージの再利用**

`build:` を持つサービスは、6 章のとおりリリースごとに `koroku-<slug>-<service>:r<deploy_id>` というイメージを作る。昇格では、staging のリリースのイメージに本番のリリースの名前を `docker tag` で付け、ビルドを省く。タグを付けるだけなので、イメージの実体（レイヤー）は共有され、staging 側のリリースが片付けられても本番の名前は残る（`docker image rm` はタグを外すだけ）。ロールバックや片付けは通常のリリースと同じに扱える。

再利用してよいのは、同じソースと同じビルド定義からなら同じイメージができる、と言えるときだけ。サービスごとに次をすべて満たすときに再利用し、どれかが外れたらそのサービスは通常どおりビルドする。

1. staging のリリースのディレクトリと env のスナップショット（7 章の `release-state/<id>/env`）がまだ残っている。
2. staging のリリースでもそのサービスに `build:` がある。
3. 正規化したビルド定義が一致する。両方のリリースで、リポジトリの Compose ファイルだけ（override なし）を、それぞれのアプリの env ファイルで `docker compose config --format json --no-env-resolution` にかけ、サービスの `build` を比べる。
   - コンテキストの中身は、同じコミットのチェックアウトなので同じ。比べるのは定義のほうで、`context`、`dockerfile`、`target`、`additional_contexts`、`args` など `build` の全項目を見る。リリースディレクトリの絶対パス（アプリと ID で変わる）は、比べる前に相対パスに直す。
   - **`build.args` は補間したあとの値で比べる。** `args: { ASSET_HOST: ${ASSET_HOST} }` のように環境変数から渡す引数は、staging と本番で値が違えば、できるイメージも違う（Rails の `asset_host` を焼き込んだアセットなど）。値そのものは扱わず、SHA-256 にしてから比べる。違ったキーの名前だけを、デプロイログと確認画面に出す（例 `build args differ: ASSET_HOST`）。値はログに出さない（10 章）。
   - Compose ファイルのパス（`compose_path`）や置き場所（6.3）が 2 つのアプリで違っても、正規化したビルド定義が同じなら再利用する。
   - 昇格の確認画面は、デプロイの前に同じ判定を出すため、昇格元のリリースのチェックアウトを昇格先の Compose で読む。昇格先が Compose を koroku に書いているときは、本文を昇格先のアプリのディレクトリの一時ファイルに書き、`docker compose --project-directory <昇格元のチェックアウト>` で相対パスの基準をチェックアウトにして読む。昇格元のチェックアウトには何も書かない。
4. staging のリリースのイメージ（`koroku-<staging の slug>-<service>:r<staging の deploy_id>`）が Docker に残っている（`docker image inspect`）。

- 判定と `docker tag` は手順 5 で行う。新しい手順のキー `reuse`（表題 `Reusing images of <slug> deployment #<id>`）の下に、サービスごとの結果を 1 行ずつ書く（`web: reusing koroku-shop-staging-web:r12`、`web: building (build args differ: ASSET_HOST)` など）。再利用できなかったサービスだけを `docker compose build <service>...` でビルドし、その手順は通常の `build`。全サービスを再利用できたら `build` の手順は出ない。`image:` だけのサービスは通常どおり `pull`。
- `docker tag` に失敗したら（判定のあとで staging のイメージが消えたなど）、そのサービスもビルドに回す。昇格だから失敗、とはしない。

**ビルド時と実行時の設定**

再利用が「同じイメージ」と言えるのは、イメージに環境ごとの値が入っていないときだけ。koroku が見て判定できるのは `build.args` までで、Dockerfile の中でビルドを走らせる処理が環境ごとの値を読む作りは見抜けない（Dockerfile の `RUN` に env ファイルは渡らないので、値が入る道は `build.args` と、リポジトリにコミットしたファイルだけ）。そこで利用者向けの Skill（`plugins/koroku/skills/prepare-app`）に、環境ごとの値はビルド時ではなく実行時に読む書き方を勧める節を足す（Rails の `asset_host` を実行時の `ENV` で決める、`.erb` のアセットや JS のバンドルに `process.env` の値を焼き込まず、ページに実行時に書き出す、など）。`build.args` に環境ごとの値を渡している場合も、koroku は差を見つけてビルドし直すので壊れはしない。

**確認画面に出す差分**

`GET /api/apps/{slug}/promotion` で、本番アプリの画面が確認の前に次を出す。読み取りだけで、何も変えない。

| 項目 | 内容 |
| --- | --- |
| 出すリリース | staging の稼働中リリース（SHA、メッセージ、デプロイ日時）と、本番の稼働中リリース |
| コミットの差 | GitHub App の commits API で、staging の SHA から 30 件をさかのぼり、本番の SHA より新しいものを並べる。30 件以内に本番の SHA がない（遠い、または履歴が分かれている）、GitHub App で読めない、などは件数を出さず、両方の SHA とメッセージだけを出す |
| 環境変数 | 片方のアプリにだけあるキー（値は見ない。値の違いは環境ごとに違って当然なので出さない） |
| イメージ | サービスごとに「再利用」か「ビルド」と、ビルドになる理由（違った引数のキー名、staging のイメージがない、など） |

- イメージの判定は、デプロイ時と同じ比較を、staging のリリースディレクトリ（同じコミット）に本番の env ファイルを当てて行う。本番の env は一時ファイル（0600、アプリのディレクトリの中、終わったら消す）に書く。リリースディレクトリ（ビルドコンテキスト）には置かない。
- 確認画面の判定は目安で、実際の判定はデプロイの手順 5 でやり直す（その間に環境変数を変えればビルドになる）。

**不採用案**

- 本番でも毎回ビルドし、レイヤーキャッシュに任せる: 変わっていないレイヤーは速いが、アセットのコンパイルなど `COPY . .` 以降の重い手順は毎回走る。staging で確かめたものと同じイメージが出る保証もない。
- イメージをレジストリに push して共有する: レジストリの運用が増える。同じホストの Docker の中で名前を付け替えれば足りる。
- `build.args` の値を DB に記録して比べる: 値の指紋を残すことになる（9.2 の指紋は環境変数そのものの比較用）。比較は判定のたびに `config` で求め、何も残さない。

### 9.8 アプリ作成時の環境変数の初期値

アプリの作成画面は、リポジトリのファイルから使っているフレームワークを推し量り、本番で動かすのにたいてい要る環境変数を初期値として並べる。利用者は作成の前に値を直したり、行を消したり足したりでき、アプリと一緒に保存される。`RAILS_ENV` の設定漏れで development のまま動く事故と、`SECRET_KEY_BASE` のように「ないと起動しないが値に決まりがない」変数を手元で生成して貼り付ける手間を減らすため。

**判定の規則**

読むのはリポジトリ直下のファイルの一覧と、下の表に出てくるファイルだけ。

| フレームワーク | 判定 | 初期値 |
| --- | --- | --- |
| Rails | `Gemfile` に `gem "rails"` か `gem "railties"` | `RAILS_ENV=production`、`RACK_ENV=production`、`SECRET_KEY_BASE=<生成>`。`config/environments/production.rb` に `RAILS_LOG_TO_STDOUT`、`RAILS_SERVE_STATIC_FILES` の名前が出てくれば、それぞれ `true` |
| Rack（Sinatra など） | Rails ではなく、`Gemfile` に `sinatra`・`roda`・`rack` の gem があるか、直下に `config.ru` がある | `RACK_ENV=production` |
| Laravel | `composer.json` に `laravel/framework` があり、直下に `artisan` がある | `APP_ENV=production`、`APP_DEBUG=false`、`APP_KEY=base64:<生成>` |
| Django | 直下の `manage.py` に `django` が出てくる | 設定モジュール（`manage.py` が `DJANGO_SETTINGS_MODULE` に入れる既定値）のファイルが `SECRET_KEY` か `DJANGO_SECRET_KEY` を環境変数から読んでいれば、その名前で `<生成>` |
| Phoenix / Elixir | 直下に `mix.exs` がある | `MIX_ENV=prod`。`mix.exs` の依存に `:phoenix` があれば `SECRET_KEY_BASE=<生成>` |
| Node.js | 直下に `package.json` があり、上のどれにも当たらない | `NODE_ENV=production` |

- 入れるのは、フレームワークか、そのひな形が作る設定ファイルが実際に読む変数だけ。`RAILS_LOG_TO_STDOUT` と `RAILS_SERVE_STATIC_FILES` は Rails 7.0 までのひな形の `production.rb` が読むもので、7.1 以降のひな形は読まない（標準出力へのログと静的ファイルの配信が既定）。読まないアプリに入れても意味がないので、`production.rb` に名前が出てくるときだけ入れる。値は `.present?` でも `== "true"` でも真になる `true`。
- Django の `DEBUG` は入れない。Django 自体は環境変数を読まず、設定ファイルの書き方（`== "True"` で比べる、`bool()` で包むなど）によっては文字列の `False` が真になり、かえってデバッグを有効にしてしまうため。`SECRET_KEY` も、設定ファイルが環境変数から読む書き方（`os.environ`、`os.getenv`、`env(...)`）のときだけ入れる。
- Node.js は、ほかのフレームワークと一緒なら入れない。Rails や Laravel のリポジトリの `package.json` はアセットのビルド用で、実行時の `NODE_ENV` は関係しない。
- 同じ名前が複数の規則から出たら、表の上のものを使う。
- 判定は `internal/envpresets` の純粋な関数で、リポジトリのファイルの組と期待する結果を `testdata` に置いて固定する。

**既存の値を上書きしない**

- Compose ファイルのどれかのサービスが `environment:` に同じ名前の値を直接（`$` を含まない値で）書いていれば、その変数は入れず、画面に「Compose ファイルで設定済み」と出す。Compose では `environment:` が `env_file` より優先され、koroku の env ファイル（6 章）の値は効かないため。`RAILS_ENV: ${RAILS_ENV:-development}` のように補間する書き方は koroku の値が効くので、入れる。読み取りは、作成画面の Compose 検出（5 章）と同じ行ベースの簡易読み取りで、読む Compose ファイルも作成画面で選んだもの。
- 画面で利用者が入力・編集した行と、自分で足した行は、ブランチを変えて判定し直しても上書きしない。消した初期値は、判定し直しても戻さない。判定し直すときに入れ替わるのは、手を付けていない初期値の行だけ。

**秘密の値の生成**

- `koroku` が Go の `crypto/rand` で生成し、初期値の API の応答に入れる。ブラウザーの実装に依存せず、koroku のほかの秘密（セッション、API トークン）と同じ生成元にそろうため。形式はフレームワークの生成コマンドに合わせ、テストで固定する。
  - Rails の `SECRET_KEY_BASE`: 64 バイトの 16 進（128 文字。`bin/rails secret` と同じ）
  - Phoenix の `SECRET_KEY_BASE`: 48 バイトの base64（64 文字。`mix phx.gen.secret` と同じ長さ）
  - Laravel の `APP_KEY`: `base64:` と 32 バイトの base64（`php artisan key:generate` と同じ）
  - Django の `SECRET_KEY`: 英数字 50 文字（`get_random_secret_key()` と同じ長さ。記号を除き、env ファイルの引用を単純にする）
- 生成した値は DB にもログにも残さない。初期値の API を呼ぶたびに新しく作り（応答は `Cache-Control: no-store`）、作成のリクエストで送られてきた時点で、ほかの環境変数と同じく暗号化して保存する（10 章）。画面は生成した値を既定で隠し、「表示」で見せる。
- 値はまだ何にも使われていない乱数なので、初期値の API は再ログイン（10.3）を求めない。

**保存**

- `POST /api/apps` の `env`（`[{key, value}]`）で、アプリと同じトランザクションで保存する。値の検査は `PUT /api/apps/{slug}/env` と同じで、1 つでも不正ならアプリも作らずに 422 を返す。アプリだけができて秘密が保存されていない、という半端な状態を作らない。
- `env` が空でなければ、作成は環境変数の保存を含むので、直近 10 分以内のログインを求める（10.3）。検査はアプリを作る前に行う。
- 再ログインすると入力中のフォームは失われる（GitHub を経由してページを読み直すため）。そこで作成画面は、開いた時点で直近のログインがなければ、フォームの前に再ログインを求め、終わったら作成画面に戻す。入力のあとで再ログインを求めて、それまでの入力を消さないため。「再ログインせずに入力する」も選べ、その場合は初期値の欄に再ログインが要ることを示し、変数をすべて外せば再ログインなしで作成できる（変数はあとから「環境変数」タブで足せる）。
- 入力の途中で 10 分を過ぎたときは、「作成」を押しても送らず、初期値の欄に期限が切れたことと再ログインのボタンを出す。送れば `403 reauth_required` で断られ、再ログインのダイアログから進むとフォームが失われるため。

**取得**

- `GET /api/repos/{owner}/{repo}/env-presets?ref=<ブランチ>&path=<Compose ファイル>`。作成画面の Compose 検出と同じく、GitHub の contents API でブランチのファイルを読み、GitHub App で読めないリポジトリは認証なしで読む（9.4）。作成画面は、リポジトリを選んだときと「既定のブランチ」を変えたときに呼ぶ。
- 1 回の判定の問い合わせは、直下の一覧と、表のうち直下にあるファイル（と `config/environments/production.rb`、Django の設定ファイル、Compose ファイル）だけ。ブランチは動くので結果はキャッシュしない。
- 読めなかったときは 200 で `status: "unavailable"` と理由（9.4 と同じ語）を返し、画面は欄を出さない。リポジトリを URL で指定したときは、github.com の URL なら同じ API で（App で読めなければ認証なしで）読み、それ以外の URL では呼ばない。

### 9.9 pull request ごとのレビューアプリ

Heroku の Review Apps に当たるもの。staging アプリ（8.2）を親にして、そのリポジトリの pull request 1 つを選ぶと、その pull request のコードを動かす別のアプリ（レビューアプリ）を作る。レビューの相手に URL を渡して画面で確かめてもらうため。staging は 1 つのブランチしか出せず、複数の pull request を並べて見せられない。

**作るのは手で、pull request ごとに 1 つ**

- 作るのは、親アプリの画面で pull request を 1 つ選んだときだけ。pull request が開かれたら自動で作る、ということはしない。3〜5 アプリを 4GB のホストに置く規模では、開いた pull request すべてにアプリを立てるとメモリとディスクが足りなくなるため。数の上限と期限は koroku では持たない（利用者が要らなくなったものを消す）。
- 作れるのは、オーナーがその親で許可したときだけ（下の「オーナーの許可」）。
- （親アプリ、pull request の番号）ごとに 1 つ。同じ pull request のレビューアプリがあれば `409 review_app_exists`。
- 親にできるのは、`environment = staging` で、GitHub App で取得する（`repo_full_name` を持ち、`repo_url` を明示していない）、それ自身はレビューアプリでないアプリ。それ以外は `422 not_review_parent`（`details.reason` に `not_staging`、`no_github_app`、`review_app`）。本番を親にしないのは、レビューアプリに親の環境変数とデータを写す（下記）ので、本番の秘密（決済の本番キーなど）と利用者のデータが、マージ前のコードを動かすアプリに増えていくのを避けるため。
- 選べるのは、開いている（`open`）、同じリポジトリのブランチからの pull request だけ。fork からの pull request は `422 pull_request_from_fork`。ポリシー検査（6 章）は他人のコードを安全に動かす隔離ではなく（10 章）、fork の作者のコードに親の環境変数とデータを渡すことになるため。判定は pull request の `head.repo.id` と `base.repo.id` が等しいこと（fork が消されて `head.repo` が空のものも fork として断る）。
- 作るのに要るロールは親アプリの管理（`manage`）で、直近 10 分以内のログイン（10.3）が要る。レビューアプリは親の環境変数（値を含む）とデータを写した新しいアプリで、環境変数の保存と DB のコピー（10.6）がどちらも管理と再ログインを求めるのと揃える。アプリの作成そのものはオーナーだけ（10.7）だが、レビューアプリは親の設定から作るので、リポジトリとホストの割り当ての判断はオーナーが親を作ったときに済んでいる。
- 消すのは通常のアプリの削除（10 章「起動制御と削除」、`DELETE /api/apps/{slug}`）で、volume も必ず消す（`?volumes` の指定によらない）。消すのに要るのも親アプリの管理と再ログイン。pull request が閉じられてもマージされても、自動では消さない。画面に pull request の状態を出し、消してよいことが分かるようにする。
- レビューアプリが残っている親アプリは削除できない（`409 has_review_apps`）。先にレビューアプリを消す。親を消すとレビューアプリが親のない半端なアプリとして残るため。

**オーナーの許可**

- 親アプリごとに「レビューアプリを許可する」（`apps.review_apps_allowed`、既定は無効）を持つ。無効の親で作ろうとすると `409 review_apps_not_allowed`（親になれるかの検査 `not_review_parent` のあと、pull request を読む前）。許可すると、pull request のコード（マージ前で、そのリポジトリのブランチに push できる人なら誰でも書ける）が親の環境変数とデータを受け取るようになる。staging の秘密とデータをどこまで渡すかはホストの持ち主の判断なので、切り替えはオーナーだけにする（10.7）。管理のメンバーが作れるのは、オーナーが許可した親でだけ。
- 切り替えは設定の `PATCH /api/apps/{slug}` の `review_apps_allowed` で、オーナー専用の項目（10.7）。専用のルートにしないのは、ほかのオーナー専用の設定（`environment`、`relax_policy`）と同じ断り方（`403 forbidden_role`、`details.fields`）と操作の記録（`app.settings_changed`）をそのまま使えるため。有効にするのは再ログインから 10 分以内に限る（10.3。親の秘密を渡す先を増やす操作のため）。無効にするのには要らない。
- 親になれないアプリ（上の `not_review_parent` の条件）では有効にできない（`422 validation_failed`、`review_apps_allowed`）。有効のまま親になれなくなった（`environment` を本番に変えた、など）ときは値が残るが、作るときの検査で断られる。
- 無効にしても、作ってあるレビューアプリはそのまま残り、デプロイ・起動・停止・削除・データのコピーのやり直しも通常どおりできる。止めるのは新しく作ることだけ。動いているレビューアプリを消すかどうかは、レビューの途中かを知っている利用者が決めるため。
- 許可の項目ができる前からレビューアプリを持っていた親は、有効で始まる（マイグレーションで、`review_apps` に子のある親を有効にする）。作れていたものが黙って作れなくなるのを避けるためで、その親の秘密とデータはすでにレビューアプリに渡っている。それ以外のアプリは無効で始まる。

**名前と経路**

- slug は `<親の slug>-pr-<番号>`。40 文字（slug の上限）を超えるときは親の slug の側を切り詰め、末尾のハイフンを落とす。その slug をほかのアプリが使っていれば `-2` から `-9` を順に付けて空いているものにし、空いていなければ `409 slug_taken`。
- ホスト名は通常のアプリと同じ `<slug>.<base_domain>`。DNS のワイルドカードのレコード（8 章）があればそのまま開ける。作るときに、そのホスト名を独自ドメインの検証（8.1）と同じ関数で名前解決し、`public_ip` だけに向いていなければ、作成の応答に `dns_warning: true` を付けて、画面はホスト名を DNS に登録するよう案内する（作成は止めない。あとから登録すればよいため）。
- アクセスの保護（8.3）は有効、アクセスがない間の休止（8.4）は有効（Caddy のアプリ別のアクセスログがないホストでは無効）、`environment` は `staging` で始まる。作ったあとは通常のアプリと同じに、それぞれの設定のあるタブ（9.1）で変えられる。

**親から写すもの**

レビューアプリは作った時点で親の写しになり、あとは独立したアプリとして扱う。親のあとからの変更（設定、環境変数、ロールを除く）は伝わらない。伝えると、レビューアプリで変えた値をどちらが勝つかの規則が要り、レビューの途中で動きが変わるため。

| 項目 | 扱い |
| --- | --- |
| リポジトリ（名前と ID。ID のない親から作ったレビューアプリは、デプロイのときに親の ID に従う。5 章）、Compose の置き場所と本文（6.3）、Compose ファイルのパス、公開サービスとポート、ヘルスチェックパス、メモリ上限、`relax_policy`、必要な変数の読み方、保護を外すパス、待つワークフロー | 親と同じ |
| 既定のブランチ、自動デプロイ | pull request の head のブランチ。自動デプロイ（5.2）を有効にしてそのブランチを追う |
| 環境変数 | 親の DB の値をすべて写す（同じトランザクションで、新しいアプリの ID で暗号化し直す） |
| コンテナレジストリの認証（6.1） | 写す（同じく暗号化し直す） |
| ダンプフック、ロードフック、コピー後のコマンド（10.2、10.6） | 親と同じ。データのコピー（下記）に使う |
| バックアップから外す volume | 親と同じ。デプロイ前の取得は無効 |
| ドメイン、登録タスク、昇格元 | 写さない |
| メンバーのロール | 持たない。親のロールに従う（下記） |

**koroku が足す環境変数**

- すべてのアプリの env ファイルに、koroku は `KOROKU_APP_URL`（デプロイした時点のプライマリ URL。9.1 の `primary_url` と同じ規則）と `KOROKU_APP_HOST`（その URL のホスト名）を足す（6 章）。レビューアプリのホスト名は作るまで決まらないので、Rails の `config.hosts` やメールの中の URL の土台をリポジトリに書いておけない。アプリはこの変数から組み立てる。
- レビューアプリには、さらに `KOROKU_PR_NUMBER`（pull request の番号）を足す。アプリはこれがあるかでレビューアプリかを見分け、外へ送るもの（メール、Webhook、決済）を止めるなどできる。
- 写した環境変数にも、外へ届く値（メールの送信先、外部サービスの Webhook の URL、OAuth のコールバックの URL）が含まれる。koroku は値を書き換えない（どれが外へ届くかを知らないため）。必要なら作ったあとで環境変数タブで直す。リポジトリ側は `KOROKU_PR_NUMBER` を見て止める書き方を勧める（Skill の案内）。

**作る流れ**

1. API が pull request を GitHub から読み直し（開いているか、同じリポジトリか、head の SHA とブランチ）、アプリ（上の表の設定）、環境変数、レジストリの認証、`review_apps` の行を 1 つのトランザクションで作る。
2. head の SHA を、手動のデプロイと同じ経路（7 章）でキューに積む。ref は head のブランチ名（fork からの pull request を断っているので、リポジトリのブランチ。7 章「デプロイできるコミット」の確かめもこのブランチで受ける）。GitHub Deployments（5.1）への反映も通常どおりで、pull request の画面に環境と URL が出る。レビューアプリの環境には `transient_environment: true` を付ける（消えるものだと GitHub に伝える）。
3. データのコピー（10.6）を、最初のデプロイが成功したあとに始める（下記）。

- デプロイが失敗しても、アプリは消さずに残す。デプロイの失敗は通常どおり履歴とログに出る。pull request に push すれば自動デプロイで出直す。手でも再デプロイできる。
- 作成の応答はキューに積んだ時点で返す（デプロイを待たない）。

**データのコピー**

- 作るときに「親のデータをコピーする」を選べる（既定は選ぶ）。選んだら、`review_apps.data_copy` を `waiting` にして作る。
- `koroku serve` の中の 1 つの見張りが、15 秒ごと（と作成の直後）に `waiting` のレビューアプリを見て、最初のデプロイが成功した（`current_deployment_id` がある）ものについて、親を送り側、レビューアプリを受け側、取り元を `live` として 10.6 のコピーを始める。成功したデプロイを待つのは、ロードフックのサービスが動いていないとコピーできないため。デプロイが何度失敗しても、成功するまで待ち続ける。
- コピーはふつうの DB のコピーと同じ手順・同じ記録（`db_copies`）で、始めた人は koroku 自身（`actor` は `null`）。ただし `backup` の手順は飛ばす（作ったばかりで、残すデータがないため）。コピー後のコマンド（個人情報を消すもの）も親の設定どおりに走る。
- 始められたら `data_copy` を `started` にし、以後の状態はその DB のコピーの記録で見る。親がデプロイ中・操作中（`busy`）なら、次の回に試し直す。そのほかの理由で始められなければ `failed` にし、理由を `data_copy_detail` に残す（例: 親が止まっている）。
- 親にダンプフックかロードフックがなければ、コピーはせずに作り、`data_copy` を `skipped`（`data_copy_detail` に `no_dump_hook` か `no_load_hook`）にする。画面は作成のダイアログで先にそのことを出し、作ったあとも「データはコピーしていません」と理由を出す。アプリは空の DB で動く（シードはリポジトリの側で、Compose のワンショットのサービスなどで行う）。
- コピーのやり直しは、レビューアプリの画面から、親を送り側にした通常の DB のコピー（10.6。確認の slug の入力、再ログイン、管理のロール）で行う。
- 失敗したコピーで、コピー後のコマンドを設定していて流し込みが途中まで進んだときは、10.6 のとおりアプリのサービスは止まったまま。

**ロール**

- メンバーのレビューアプリでのロールは、親アプリでのロールと同じとする。ロールを読むところ（`authorize`、アプリの一覧の絞り込み、アクセスの保護のゲート）は、`review_apps` を通じて親の `app_roles` を引く。レビューアプリ自身には `app_roles` の行を持たず、メンバーの画面でもロールを付けられない（`PUT /api/members/{id}/roles/{slug}` は `422 role_follows_parent`）。親でロールを外せば、レビューアプリにもその場で入れなくなる。作るたびにロールを写す方式だと、親でロールを外したあとも古いレビューアプリに入れる穴が残るため。
- デプロイのロールで、レビューアプリの再デプロイ・起動・停止ができる（通常のアプリと同じ）。

**pull request の状態**

- `review_apps` に pull request の題名、head のブランチ、状態（`open` / `closed` / `merged`）を持つ。同じ見張りが 10 分ごとに、レビューアプリごとに GitHub から読み直す（`GET /repos/{owner}/{repo}/pulls/{number}`）。失敗したら前の値のまま。画面の表示のためだけで、何も決めない。
- head のブランチが消されても（マージ後の削除など）、アプリはそのまま動く。push が来なくなるだけ。

**GitHub App の権限**

- pull request を読むには、Repository permissions の **Pull requests: Read-only** が要る。ない App でも koroku は動き、レビューアプリの作成だけが使えない。pull request の一覧が 403 で断られたら、API は `502 github_permission_missing` を返し、画面は App に権限を足して、インストール先で新しい権限を承認するよう案内する。`koroku check`（10.4）は、この権限がなければ警告する。

**API**（形は [API.md](API.md)）

| メソッドとパス | 用途 | ロール |
| --- | --- | --- |
| `GET /api/apps/{slug}/pull-requests` | 親のリポジトリの開いている pull request（fork か、レビューアプリがあるか） | 親の管理 |
| `GET /api/apps/{slug}/review-apps` | 親のレビューアプリの一覧（アプリの JSON） | 親の閲覧 |
| `POST /api/apps/{slug}/review-apps` | `{"pr_number": 12, "copy_data": true}` で作る。201 でアプリの JSON と `dns_warning` | 親の管理、再ログイン、親の許可 |
| `PATCH /api/apps/{slug}` の `review_apps_allowed` | 許可の切り替え | オーナー、有効にするときは再ログイン |

- アプリの JSON の `review`: レビューアプリなら `{parent, pr_number, pr_title, pr_url, pr_state, head_branch, data_copy, data_copy_detail, data_copy_status}`、そうでなければ `null`。`data_copy_status` は親からの直近の DB のコピー（10.6。やり直しを含む）の状態（`running` / `succeeded` / `failed`、なければ `""`）。`data_copy` が `started` になったあとの結果を、レビューアプリごとに DB のコピーの一覧を読まずに出せるようにするため。
- アプリの JSON の `review_apps_allowed`（真偽値）と `review_apps`: レビューアプリでないアプリなら `{count, failed, deploying}`（レビューアプリの数、そのうち異常なもの、デプロイ中のもの）、レビューアプリなら `null`。アプリ一覧と概要の 1 行は、これでレビューアプリの API を親ごとに呼ばずに済む。
  - 異常に数えるのは、直近のデプロイが失敗した（`failed`。中断を含む）もの、データのコピーを始められなかった（`data_copy = failed`）もの、`data_copy_status` が `failed` のもの。デプロイ中（`queued`、`running`）は異常に数えず `deploying` に数える。
  - コンテナの不調（unhealthy、再起動の繰り返し）は数えない。アプリの JSON は DB から作り、`docker` を呼ばない（一覧を数秒ごとに読み直すため）。コンテナの状態はレビューアプリの概要で見る。
- `GET /api/apps` はレビューアプリも返す（`review` が `null` でないもの）。API トークン（10.5）で状態を読むときには、レビューアプリも 1 つのアプリとして見えるほうが扱いやすいため。行にしないのは画面の側。

**画面**

- 親になれるアプリ（staging で、GitHub App で取得し、それ自身はレビューアプリでないもの）のデプロイタブの末尾（履歴の下）に「レビューアプリ」の節を置く。本番とレビューアプリには出さない（節の URL を直接開いたときは、節のないデプロイタブを出す）。ただし、親になれなくなった（本番に変えた）アプリでも、レビューアプリが残っていれば出す（残りを消せるように）。タブにしないのは、レビューアプリは pull request ごとのデプロイ先で、デプロイと続けて扱うためと、開く頻度がタブ 1 つ分に満たないため（9.1）。
- 節には、レビューアプリの一覧、作成、許可の欄を置く。
  - 許可の欄: オーナーにはスイッチを出す（有効にするときに、pull request のコードが親の環境変数とデータを受け取ることを書いた確認を挟む）。ほかの人には今の状態を出し、無効なら「オーナーが許可すると作れる」と書く。
  - 一覧: レビューアプリごとに pull request の番号と題名（レビューアプリへのリンク）、pull request の状態、アプリの状態、URL、データのコピーの状態。
  - 「レビューアプリを作成」（管理以上）は許可されているときだけ押せ、開いている pull request を選ぶダイアログを開く。fork からのものとレビューアプリのあるものは選べない。ダイアログに「親のデータをコピーする」を置き、親にフックがなければ無効にして理由を出す。
- 親の概要タブには 1 行だけ置く（「レビューアプリ 3 件」と、異常なものがあれば「1 件失敗」。デプロイタブの節へのリンク）。許可されておらず 1 件もないときは出さない。
- アプリ一覧には、レビューアプリの行を出さない。親の行に「レビューアプリ 3」と数を出し、異常なものがあればその数も足す（「レビューアプリ 3（1 件失敗）」）。デプロイ中のものは色を付けずに足す。アプリ 3〜5 個の一覧が pull request の数だけ伸びると、本体のアプリが埋もれるため。数はデプロイタブの節へのリンクにする。一覧の絞り込みは親のロールのままで、メンバーは親のデプロイタブの節からレビューアプリに入る（レビューアプリのロールは親のもの）。
- レビューアプリのヘッダーには、どのタブでも親へのリンク（「親: <slug>」。親のデプロイタブのレビューアプリの節）を出す。一覧に行がないので、レビューアプリから親へ戻る道をタブによらず残すため。
- レビューアプリの概要タブに、親と pull request へのリンク、pull request の状態、データのコピーの状態（待機中、コピー中、失敗、しなかった理由）と「親のデータをコピーし直す」（管理以上）を出す。pull request が閉じられていれば、削除できることを示す。

**データ**: `apps.review_apps_allowed`（既定は偽）と、テーブル `review_apps`（`app_id`（主キー、アプリの削除で消える）、`parent_app_id`（親のアプリ）、`pr_number`、`pr_title`、`head_branch`、`pr_state`、`pr_checked_at`、`data_copy`（`''` / `waiting` / `started` / `skipped` / `failed`）、`data_copy_detail`、`created_at`。（`parent_app_id`、`pr_number`）で一意）。

**採らない方式**

- pull request が開かれたら自動で作り、閉じたら消す: 上記のとおりホストの資源が足りず、消すときに volume も消えるので、レビュー中のデータが利用者の知らないうちに消える。必要になったら、pull request のイベントの購読と数の上限を別に設計する。
- 親の環境変数をレビューアプリから参照し続ける: 親を直すと動いているレビューアプリの動きが変わり、レビューアプリだけの値を持てない。
- レビューアプリのデータをシードだけで作る: 多くのアプリにはシードがなく、staging のデータで確かめたいことが多い。シードを使いたいときは「親のデータをコピーする」を外す。

### 9.10 Compose の構成を見る

アプリが思いどおりに動かないとき（上限が効いていない、OOM で止まった、古いコンテナが残っている）に、koroku が何を重ねたか、重ねた結果どの値になったか、いま何が動いているかを 1 か所で確かめられるようにする。ホストに SSH してリリースのディレクトリを探し、`docker compose config` や `docker inspect` を手で組み立てずに済むようにするため。アプリ詳細のリソースタブの末尾に「構成」の節を置き、読み取り専用で次の 3 つを並べる。上限と予約を決める節（9.5）と同じタブにあるのは、決めた値が実際のコンテナに効いているかを続けて確かめるため。

1. **koroku の override**: そのリリースの `koroku.override.yaml` をそのまま（6 章）。
2. **重ねた結果**: リポジトリの Compose と override を `docker compose config` で正規化したもの。`!override` と `!reset` が効いたかを確かめられる。
3. **コンテナ**: アプリのコンテナを `docker container inspect` で引いた値と、稼働中のリリースとの食い違い。

**リリースの選び方**

- 既定は稼働中のリリース（`current_deployment_id`）。ファイルを保持しているほかのリリース（7.1 の保持。直近 3 件）も選べる。候補は、成功したデプロイを新しい順にたどり、ロールバックの記録は戻す先に読み替えて重複を除いた、ファイルの残っているリリース。
- 稼働中のリリースがない、またはそのファイルが残っていなければ `409 not_deployed`。指定したリリースのファイルが残っていなければ `409 release_unavailable`。
- コンテナ（3）は選んだリリースによらず、いつも稼働中のリリースと比べる。過去のリリースのハッシュ（下記）は、そのリリースの環境変数で計算しないと合わず、稼働中の env ファイルはそれと違うため。

**重ねた結果の出し方**

- デプロイと同じ呼び出し（`-p koroku-<slug>`、`-f <Compose ファイル> -f koroku.override.yaml` の順、`--env-file <アプリの env ファイル>`、リリースのディレクトリで実行）に `config --format json --no-interpolate --no-env-resolution` を付ける。`--no-interpolate` で `${VAR}` は書いたままの文字列で残り、`--no-env-resolution` で `env_file` はパスのまま残る（6 章）。環境変数の値は出力に入らない。
- 補間しないので、`${TAG}` を使った `image:` や `${MEM:-256m}` の `mem_limit` は書いた形で出て、単位の正規化（`256m` からバイト数へ）もかからない。実際に効いた値はコンテナ（3）の側で見る。
- JSON を koroku がキーの順を保ったまま YAML の形に書き直して返す。読みやすさのためで、YAML のライブラリは入れない（出すだけなので、小さな書き出しで足りる）。
- `config` が失敗したら（Compose ファイルが壊れているなど）、その欄にエラーを出し、ほかの欄は出す。

**値を伏せる規則**（10 章: 環境変数の値、トークン、鍵を出さない）

- 重ねた結果の各サービスの `environment` の値、`build.args` の値、トップレベルの `secrets` の `content` は、すべて `[MASKED]` に置き換え、キーだけを出す。リポジトリに直に書いた値も伏せる。koroku にはどれが秘密か見分けられないため。値のない `KEY`（`null`）は伏せる値がないのでそのまま出す。
- `labels`、`command`、`healthcheck` などはそのまま出す。補間しないので、そこに書いた `${VAR}` は参照のまま出る。
- 3 つの出力の全体を最後に、アプリの保存済みの変数と env ファイル（稼働中のものと、選んだリリースのスナップショット）の値のうち 8 文字以上のものでもう一度伏せる。リポジトリに同じ値を直に書いていた場合の保険。koroku が足す `KOROKU_` の変数（6 章）は秘密ではないので数えない。デプロイログのマスク（3 文字以上を単語単位）より緩いのは、`PORT=3000` のような短い値で override のポートが `[MASKED]` になるのを避けるため。補間しないので、短い値が出力に入る経路はリポジトリに直に書いたものだけで、`environment` に書いたものは上の規則で伏せている。
- コンテナの `Config.Env` は返さない。inspect の出力から、下の表の項目だけを取り出す。
- エラーの本文（Compose の標準エラー）は、デプロイログと同じマスクを通す。

**コンテナとリリースの照合**

- コンテナは `docker ps --all` をプロジェクトのラベル（`com.docker.compose.project=koroku-<slug>`）で絞って列挙し、`docker container inspect` で引く。イメージのダイジェストは `docker image inspect` の `RepoDigests` から取る（ビルドしたイメージにはないので `null`）。ワンオフのコマンドのコンテナ（`com.docker.compose.oneoff=True`、9.6）は除く。止まっているコンテナも「停止」として出す。
- 稼働中のリリースの設定で作られたコンテナかどうかは、Compose がコンテナに付けるラベル `com.docker.compose.config-hash` で決める。稼働中のリリースのファイルで `docker compose config --hash '*'` を実行し（補間あり。出力はサービス名とハッシュだけで値を含まない）、サービスごとに比べる。違えば古い設定のコンテナで、次の `up` で作り直される。Compose 自身が `up` で作り直すかどうかを決める値なので、koroku がサービスの定義を比べ直すより確か。override にデプロイの ID のラベルを足す方法は採らない。ラベルが変わるとデプロイのたびに全サービス（DB を含む）が作り直されるため。
- コンテナを作ったデプロイは、`build:` のサービスならイメージのタグ（`:r<deploy_id>`、6 章）で、それ以外は作成時刻（作成時刻より前に始まった最後のデプロイ）で推定し、どちらで決めたかも返す。作成時刻の推定は、起動（10 章）がファイルの変わったリリースを `up` したときなどにずれうるので、画面では「推定」と書く。
- 稼働中のリリースのハッシュが求められない（`config` が失敗した）ときは照合しない（`compared: false`）。

**食い違いの印**

| 印 | 条件 |
| --- | --- |
| `outdated` | `config-hash` が稼働中のリリースのハッシュと違う |
| `orphan` | 稼働中のリリースにないサービスのコンテナ |
| `memory_limit` | `HostConfig.Memory` が稼働中のリリースの上限と違う |
| `memory_reservation` | `HostConfig.MemoryReservation` が稼働中のリリースの予約と違う |
| `oom_score_adj` | `HostConfig.OomScoreAdj` が稼働中のリリースの値と違う |
| `oom_killed` | `State.OOMKilled` が真 |
| `restarting` | 状態が `restarting` |
| `restarted` | `RestartCount` が 1 以上 |
| `unhealthy` | ヘルスチェックが `unhealthy` |
| `exited` | 0 以外の終了コードで止まっている |

- 上限・予約・`oom_score_adj` の期待する値は、稼働中のリリースを補間ありで正規化した結果（デプロイの手順 4 と同じ `config --format json --no-env-resolution`）から読む。単位がバイトに揃うため。この出力は返さず、その 3 つの値だけを使う。上限も予約もないサービスは 0（Docker の値と同じ）として比べる。
- `OOMKilled` は、コンテナが起動し直すと偽に戻る（9.5）。印がなくても OOM がなかったとは言えないので、画面では再起動の回数と並べて出す。

**誰が、いつ**

- 閲覧以上（10.7）。返すのは値を伏せた出力だけで、環境変数タブのキーの一覧と同じ扱い。API トークン（10.5）でも読める。デプロイは成功したのに動かない（OOM、古いコンテナ、上限の違い）ときに、Skill（`deploy-status`）がこれで調べられるようにするため。
- 節は閉じた状態で出し、「表示」で開いたとき（節の URL で直接入ったときは最初から開く）、リリースを選び直したとき、「再読み込み」を押したときにだけ実行する。ポーリングはしない。リソースタブは使用量を見に何度も開くタブで、開くたびに `docker compose config` と inspect を走らせないため。コマンドは 1 つずつ 30 秒で打ち切る。デプロイの操作のロック（10 章）は取らない。読むだけで、コンテナにもファイルにも触れないため。デプロイの途中に開くと、途中の状態が見える。
- API は `GET /api/apps/{slug}/compose?deployment=<id>`（1 と 2）と `GET /api/apps/{slug}/containers`（3）。形は [API.md](API.md)。リリースを選び直すたびに inspect をやり直さずに済むよう分けている。

## 10. セキュリティと運用

`koroku` は Docker ソケットを操作できるためホストの root 相当であり、管理 UI の認証がこのシステムの唯一の関門になる。

**セキュリティ**

- **信頼モデル**: デプロイするのは自分のリポジトリだけ。他人のコードを動かす前提の隔離（gVisor 等）は持たない。GitHub App で取得するアプリは、リポジトリの名前が別のリポジトリに渡っていないことを、取得の前に ID で確かめる（5 章「リポジトリは ID で確かめる」）。デプロイできるコミットも、そのリポジトリのブランチかタグからたどれるものに限る（7 章「デプロイできるコミット」）。fork からの pull request のコミットのように、リポジトリに書けない人が置けるものは、SHA を指定してもデプロイしない。
- **認証**: GitHub ログイン後、ユーザー ID が `config.toml` のオーナーの ID か、ロールを 1 つ以上持つメンバーの ID と一致した場合のみセッションを発行する（10.7）。Cookie は `HttpOnly`、`Secure`、`SameSite=Lax`。変更系 API は `Origin` ヘッダーを検証する。GitHub の 2 段階認証の必須化、危険な操作の前の再ログイン、セッションの一覧とログイン履歴は 10.3。
- **待受**: `koroku` と Caddy admin API は 127.0.0.1 のみ。外部公開は 22、80、443。
- **前提と限界**: ポリシー検査は「自分のリポジトリを誤って危険な構成でデプロイしない」ための検査で、悪意あるリポジトリを安全に動かす隔離ではない。Compose の設定は許可する一覧で絞る（6 章）が、コンテナの中で動くコードとカーネルの間には Docker の既定の隔離しかなく、Dockerfile の `FROM` に名前を書けば、ホストにあるほかのアプリのイメージを読める。ビルドと、`replicas` で増やしたコンテナの資源にも上限を掛けない。「検査を緩める」フラグを立てたアプリには、上の保護の多くが効かない（フラグは 1 つで、項目ごとには選べない）。Caddy の admin API は認証なしの TCP（ループバック）なので、ホストのネットワークに入れたコンテナからは設定を書き換えられる。ログのマスクは環境変数の値そのものとの一致だけで、3 文字未満の値や、base64 などに変換された値は隠せない。
- **ポート公開の抑止**: Docker が publish したポートの通信は ufw の設定を通らずに届く（[Packet filtering and firewalls](https://docs.docker.com/network/packet-filtering-firewalls/)）。ufw に頼らず、6 章の override で全サービスの `ports` を消すか 127.0.0.1 に限定する。
- **シークレット**: 環境変数の値とコンテナレジストリのトークン（6.1）は AES-256-GCM で暗号化して SQLite に保存する。マスターキーは `/etc/koroku/master.key`。UI は環境変数を既定でマスク表示し、レジストリのトークンは表示しない。

**管理 UI の保護の細則**

- **アプリは管理 UI の兄弟サブドメインで動く**（`<slug>.<base>` と `console.<base>`）。ブラウザーから見ると same-site なので、`SameSite=Lax` はアプリからのリクエストを止めない。そのため次を重ねている。
  - Cookie 名に `__Host-` プレフィックスを付ける（HTTPS のとき）。兄弟サブドメインのページは親ドメイン向けの Cookie を置けるが、`__Host-` 付きの Cookie はそのホスト自身からしか設定できないので、アプリがセッションやログイン用 state の Cookie を差し込んだり上書きしたりできない。
  - 変更系リクエストは `Origin` の完全一致を要求する（5 章）。例外は GitHub の Webhook（`POST /webhooks/github`）だけで、署名で認証する（5.2）。加えて、ブラウザーが `Sec-Fetch-Site` で同一オリジン以外と申告した API リクエストは、参照系も含めて拒否する（応答は読めなくても、`docker system df` や restic の一覧を走らせる負荷はかけられるため）。
  - JSON 以外の Content-Type のボディは拒否する（`text/plain` などの POST は CORS のプリフライトなしで送れるため）。
- **レスポンスヘッダー**: `Content-Security-Policy`（スクリプトは自サイトのファイルのみ。ビルド成果物にインラインスクリプトはない）、`Strict-Transport-Security`（HTTPS のとき）、`X-Frame-Options: DENY`、`Cross-Origin-Opener-Policy` / `Cross-Origin-Resource-Policy: same-origin`、`X-Content-Type-Options: nosniff`。
- **待受アドレス**: `listen` はループバックアドレスしか受け付けない（起動時に検査）。
- **`--insecure-no-auth`**: `[caddy] enabled = true` のときは起動を拒否する。このモードでも、Host がループバック名でないリクエスト（DNS リバインディング対策）と、Origin が自分自身でない変更系リクエストは拒否する。ログインがない分、起動した本人のブラウザーが攻撃面になるため。
- **設定ファイル**: 秘密（GitHub の client secret と Webhook の secret、ストレージの secret key）を含む `config.toml` が所有者以外から読める権限だと起動を拒否する。
- **OAuth のコールバック**: GitHub への問い合わせは、クライアントのアドレスごとに 1 分あたり 10 回、全体で 1 分あたり 60 回までしか行わない（誰でも叩けるエンドポイントで、叩くたびに client secret 付きのリクエストが飛ぶため）。GitHub へ問い合わせる直前に数える。
  - state の Cookie がない、Cookie と `state` パラメーターが一致しない、`code` がないリクエストは、数える前に断る。ログインを始めていないクライアントが回数を使い切って、本物のログインを失敗させないようにするため。
  - Cookie と `state` を自分でそろえて送るクライアントは数に入る（state はそのブラウザーの中での照合で、koroku が発行した値かどうかは確かめない）。そのクライアントが 1 つのアドレスから全員の回数を使い切って、本物のログインを失敗させ続けないように、先にアドレスごとに数える。自分の上限を超えたアドレスのリクエストは、全体の回数に入れない。
  - アドレスは 10.3 の「クライアントの IP」（Caddy が接続元で置き換えた値で、偽装できない）。IPv6 は /64 を 1 つのクライアントとして数える（1 台のホストが /64 の中のアドレスを自由に選べるため）。
  - 全体の上限は、client secret 付きで GitHub へ飛ぶリクエストの数を、小さな決まった数に抑えるために残す。60 回は 1 秒に 1 回にあたる。ログインするのはオーナーと数人のメンバーで、1 回のログインは 1 回の問い合わせなので、本物の利用は 1 分に数回もない。1 つのアドレスの上限の 6 倍なので、1 つや 2 つのアドレスからでは使い切れない。多くのアドレスを使えるクライアントは全体の上限を使い切れ、その間のログインは失敗する（1 分たてばまた通る）。これは防がない。
  - アドレスごとの回数はメモリに持ち、1 分たったものは捨てる。覚えるのは 1,024 アドレスまでで、それを超えた新しいアドレスのリクエストは断る（1 分のうちにそれだけのアドレスが来た時点で、全体の上限は使い切られている）。
- **ログの大きさ**: デプロイログは 1 行 64 KiB、1 デプロイ 50 MiB で打ち切る（改行のない出力や終わらない出力で、メモリや全アプリ共有のディスクを使い切らないため）。
- **ファイルの権限**: `koroku.db` は 0600 にする。オブジェクトストレージの endpoint は、ループバック以外では https だけを受け付ける。
- **デプロイログの配信**: 行の途中の CR は取り除く。SSE では CR も行の区切りなので、ログの内容で `event: end` などのフィールドを偽装できてしまうため。
- **コンソールのホスト名と同じ slug・独自ドメイン**は登録できない。
- **staging アプリの閲覧の保護**: アプリのホストには管理 UI のセッション Cookie を広げず、ホスト限定のゲートの Cookie と 1 回限りのコードで受け渡す（8.3）。

**運用**

- **コンテナログ**: Docker の `json-file` ドライバを `max-size=10m`、`max-file=3` でローテートする（`daemon.json`）。長期保管は 10.1 の収集機構が担う。
- **ディスク**: デプロイ成功後に `docker image prune -f`、週 1 回 `docker builder prune --all --filter until=168h`。使用率はシステム画面に出す。
- **メモリ**: ビルド時のピークに備えて swap を 2GB 置く。VPS は 2 vCPU、4GB RAM、40GB ディスクを出発点にする（未計測の目安）。
  - swap の手前に zswap を有効にする（圧縮は `zstd`、カーネル 6.8 以降は `shrinker_enabled` で古い圧縮ページを swap ファイルへ追い出す）。3〜5 アプリのうちアクセスの少ないものは、ヒープの多くに触れないまま常駐している。そのページを RAM の中で圧縮して持てば、ディスクの swap ほど遅くならずに空きが増える。アプリの `docker stats` の値は変わらず、効き目はホストの空きメモリに出る。
  - ホストの Caddy は `GOGC=50` で動かす（systemd の drop-in）。リクエストや TLS のハンドシェイクが集中したあとに膨らんだヒープを抱えたままにせず、常駐を 10 MiB ほど減らす。増える CPU は、小さなアプリを数個載せるホストでは問題にならない。
  - 同じ drop-in で Caddy を `OOMScoreAdjust=-450` にする。ホスト全体でメモリが尽きたとき、本番アプリのコンテナより後に止まるようにするため（6.2）。
- **バックアップ**: 基盤の設定とアプリのデータを日次でオブジェクトストレージに退避する（10.2）。
- **koroku の更新**: バイナリを差し替えて `systemctl restart koroku`。アプリのコンテナは影響を受けない。
- **起動制御と削除**
  - 停止は `docker compose stop`、起動は稼働中リリースのファイルで `up -d --wait`（設定を確実に揃えるため。リリースのファイルが残っていなければ `docker compose start`）、再起動は `docker compose restart`。blue/green のアプリでは、起動と再起動に動かすサービスを名前で並べる（いまの色だけを動かすため。7.2）。どれもコンテナの停止の猶予（`stop_grace_period`）を待ちうるので、タイムアウトは猶予の上限を含めて決める（6 章「停止の猶予」）。停止・起動のたびに Caddy 設定を再投入し、停止中は 503 の固定ページになる。停止と起動はアクセスがない間の休止（8.4）を解き、休止中のアプリの再起動は起こす処理と同じになる。
  - デプロイと手動操作はアプリ単位で直列化する。デプロイが `queued` か `running` のアプリへの操作は 409（`busy`）で断り、操作の実行中に始まったデプロイは操作の完了を待つ。同じアプリに対して `docker compose` を同時に走らせないため。
  - 削除は `docker compose down`（`?volumes=true` なら `--volumes` に加え、プロジェクトのラベルが付いた volume を明示的に消す）→ リリース用イメージの削除（pull したイメージは、ほかのアプリが使っていないものだけ。6 章）→ DB の行とアプリのディレクトリの削除 → Caddy 設定の再投入、の順。`down` に失敗したら DB には触れない。volume を残して削除したあとに同じ slug でアプリを作り直すと、プロジェクト名が同じなので残した volume がそのまま使われる。
  - 削除は、ログとバックアップを消さない。ローカルの実行ログとアクセスログ（`/var/lib/koroku/logs/<slug>`。14 日で消える）、退避済みのログ（バケットの `logs/<slug>/`。ライフサイクルルールが消すまで）、restic のスナップショット（タグ `app:<slug>`。世代管理が消すまで、月次のものは数か月）が残る。消えるのはアプリのディレクトリ（`apps/<slug>`）で、デプロイ・コマンド・DB のコピーのログの手元の分はここにある。どれも slug で引くので、同じ slug で作り直したアプリは、残ったものを自分のものとして見せる。閲覧のロールは前のアプリのログを読め、管理のロールは前のアプリのダンプをダウンロードし、前のアプリの volume のスナップショットをリストアできる（10.1、10.2）。誤って消したアプリを同じ slug で作り直せば、バックアップから戻せる。前のアプリのデータを新しいアプリのメンバーに見せたくないときは、別の slug で作るか、残ったものを先に手で消す（`docs/OPERATIONS.md`）。削除の確認ダイアログは、ログとバックアップがその名前で残り、同じ名前で作ったアプリのものになることを伝える。
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
| 収集 | アプリごとに `docker compose logs --follow --timestamps --no-color` を読み続け、`/var/lib/koroku/logs/<slug>/<service>/YYYY-MM-DD.log` に追記。Compose のプラグインのバイナリを直接起動する（下の細則） |
| 再開 | `koroku` 再起動時は最後に書いた時刻を `--since` に渡す。直近分は Docker 側に残っているので短い停止は埋まる |
| ローカル保持 | 14 日。日付が変わったファイルは gzip 圧縮 |
| 量の上限 | 1 サービス 1 日あたり 100 MiB（`[logs] daily_limit_mb`）。超えた行は日付が変わるまで捨てる。データディスクの使用率が 90% 以上の間は、どの行も書かない（下の「ログの量の上限」） |
| オブジェクトストレージへの退避 | 4 時間ごと（00:10 JST 起点）に、前回以降の差分を gzip して `logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz` へアップロード |
| オブジェクトストレージ側の保持 | プレフィックス `logs/` に 90 日で削除するライフサイクルルールを設定（[S3: Lifecycle configuration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html) と同等の機能。koroku からは設定しない） |
| 閲覧 | ライブ tail（SSE）。過去 14 日は日付・サービス・キーワードで絞り込み。それ以前は presigned URL でダウンロード（[S3: Presigned URLs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html)） |

- API: `GET /api/apps/{slug}/logs?service=&tail=`（ライブ tail）、`GET /api/apps/{slug}/logs/history?service=&from=&to=&q=`（過去ログの検索）、`GET /api/apps/{slug}/logs/archives`、`GET /api/apps/{slug}/logs/archives/url?key=`。

**ログ収集の細則**

- ローカルのファイルは 1 行が `<UTC の時刻（ナノ秒まで）> <本文>`。日付の区切りと退避の時間枠は `config.toml` の `timezone`（既定 `Asia/Tokyo`）で決める。
- 常駐する `logs --follow` は、`docker compose` ではなく Compose のプラグインのバイナリ（`docker-compose`）を直接起動する。`docker compose` は docker CLI がプラグインを子プロセスとして起動するため、アプリごとに 2 つのプロセスが常駐し、docker CLI の側だけで 1 アプリあたり約 7 MB と、その実行ファイルの読み込み分が `koroku` の unit のメモリに加わる。プラグインは単体で起動しても同じ出力を返す。プラグインは docker CLI と同じ順で探す（`$DOCKER_CONFIG/cli-plugins`（既定 `~/.docker/cli-plugins`）、`/usr/local/lib/docker/cli-plugins`、`/usr/local/libexec/docker/cli-plugins`、`/usr/lib/docker/cli-plugins`、`/usr/libexec/docker/cli-plugins`）。見つからなければ `docker compose` で起動する。起動のたびに探すので、プラグインを入れ直しても `koroku` の再起動はいらない。デプロイなどの短い Compose の操作は `docker compose` のまま。
- サービス名は `docker compose logs` の行頭（コンテナ名からプロジェクト名を除いたもの）から末尾の `-<番号>` を落として求める。blue/green のアプリの色の付いたサービス（`web-blue`、`web-green`）は公開サービスの名前に読み替える（7.2）。`container_name` を独自に指定したサービスはその名前で記録される（Compose がそう出力するため）。
- 再開時は、サービスごとに最後に書いた時刻の 30 秒前から `--since` で読み直し、「サービス・時刻・本文」が一致する行を数えて重複を落とす。同じ時刻の別の行は残す。Docker 側に行が残っている限り欠落しない。
- 1 回の `logs --follow` の中で、同じコンテナの「時刻・本文」が一致する行がもう一度出たら捨てる（コンテナごとに直近 4096 行を覚えておく）。`docker compose logs --follow` は、コンテナの起動と重なって開始されると同じコンテナに二重に接続し、以後その行をすべて時刻まで同じ内容で 2 回ずつ出すことがある。停止中のアプリを待っているフォロワーはデプロイのたびにこの状況になるので、対処しないと保存されるログとオブジェクトストレージへの退避が倍になる。Docker は行ごとにナノ秒の時刻を付けるので、同じコンテナで時刻も本文も同じ行はアプリが 2 回出力したものではない。別レプリカ（`web-1` と `web-2`）の同じ行は残す。
- 1 行は 16 KiB で切り、不正な UTF-8 と制御文字は置き換える。ログの本文は `koroku` 自身のログやエラーメッセージに出さない（アプリの出力に何が入るか分からないため）。
- Caddy のアクセスログは、同じアプリの擬似サービス `_access` として取り込む。`koroku` が止まっている間にローテートされて消えた分は読めない。
- 退避は「日ごとのファイルのどこまで送ったか」をバイト位置で記録し、送る範囲を保存してからアップロードする。失敗や再起動のあとは同じ範囲を同じキーへ送り直すので、重複も欠落も起きない。失敗した時間枠は次回（および 10 分後の再試行）にその時間枠のキーで送る。空の時間枠は何も送らない。
- デプロイログは、30 分間更新がなければ完了とみなして `logs/<slug>/_deploy/<id>.log.gz` に 1 回だけ退避する。
- オブジェクトストレージが未設定でもログの収集・検索・ライブ表示は動く。退避と、退避済みログの一覧・ダウンロードだけが無効になる。
- 退避したログは暗号化しない。`logs/<slug>/...` のオブジェクトは gzip しただけの平文で、実行ログ・アクセスログ・デプロイログのどれも同じ。ブラウザーが presigned URL でオブジェクトをそのまま受け取るので、koroku の鍵で暗号化したものは置かない。バケットを読める者（アクセスキーを持つ者、事業者の管理画面に入れる者）は、全アプリのログを読める。バケットは非公開にし、アクセスキーはこのバケットだけに限定し、事業者がバケットに保存時の暗号化を持つなら有効にする（`docs/SETUP.md`）。環境変数の値の伏せ字が掛かっているのはデプロイログだけで、実行ログはアプリが出したままを置く。アプリが秘密をログに出せば、それもバケットに平文で残る。
- ログは slug で分けて置く。ローカルのファイルも、退避したオブジェクトも、アプリを削除しても消えず、同じ slug で作り直したアプリのログタブと退避済みログの一覧に出る（10 章の「起動制御と削除」）。
- 過去ログの検索はライブ tail と別のパス（`/logs/history`、`docs/API.md`）に置く。SSE と JSON を同じパスで出し分けると扱いにくいため。
- 制約: 直近の区間は VPS 上にしかない。ホストを失うと最大 4 時間分のログが消える。間隔は設定値（既定 4 時間、最短 1 時間）。1 回の処理はサービスごとに差分テキストの圧縮と 1 オブジェクトのアップロードだけで、常駐負荷は増えない。

**ログの量の上限**

コレクタが届いた行を際限なくファイルに写すと、出力の多いコンテナや、アプリのホスト名へ大量に送られたリクエスト（アクセスログはログインしていない相手の分も残る。8 章）で、全アプリが共有するディスクが埋まり、SQLite への書き込み、各アプリの DB、デプロイ（使用率 85% 超で拒否。8.2）がまとめて止まる。Docker 自身のログは 1 コンテナ 30 MB で回る（10 章）が、`koroku` の写しにはそれが効かない。そのため、ログに使うディスクに上限を持つ。1 行の大きさ、1 サービス 1 日あたりの量、ディスク全体の使用率の 3 段で、後ろの段ほど、手前の段で防げなかったときの備えになる。

- **1 行の大きさ**: 16 KiB で切る（上記）。アクセスログの行は Caddy の側で 2.5 KB ほどに収める（8 章「アクセスログの内容」）。
- **1 サービス 1 日あたりの量**: 日ごとのファイル（`<slug>/<service>/YYYY-MM-DD.log`。圧縮する前の大きさ）が `[logs] daily_limit_mb`（既定 100、1〜2048）MiB に届いたら、そのサービスのその日の行を、日付が変わるまで捨てる。コンテナのサービスにも `_access` にも同じように効く。入りきらない行が来た時点でその日は閉じ、あとから短い行が来ても足さない（目印の行をその日の最後の行にするため）。
- **ディスクの使用率**: データディスク（`data_dir` のあるファイルシステム）の使用率が 90% 以上の間、コレクタはどのアプリのどのサービスの行も書かずに捨て、90% を下回ったらまた書く。ログ以外（イメージ、volume、バックアップの作業ファイルなど）がディスクを埋めたときや、1 日の上限を大きくした設定でも、ログがディスクの残りを使い切らないための最後の歯止め。使用率は、デプロイの拒否（8.2）と同じ調べ方（`statfs`。root 用の予約分は使用済みに数える）で読み、行ごとには調べず、10 秒に 1 回だけ読んでその間は同じ答えを使う。読めなかったときは書き続ける（分からないことを理由にログを失わない）。
- **90% の根拠**: しきい値は設定の項目にせず、デプロイを拒否する使用率（`[deploy] disk_usage_limit_percent`、既定 85）に 5 を足した値（100 まで）にする。ディスクが減ってきたときは、まずいちばん大きく使うデプロイ（イメージとビルドキャッシュ）を止め、その原因を調べるのに要るログは、もうしばらく残す。85% と 90% の間は 40 GB のディスクで 2 GB あり、1 日の上限いっぱいのサービスが 20 本あっても 1 日はもつ。90% で止めれば 4 GB が残り、SQLite と各アプリの DB は書き続けられる。95% まで待つと、その余裕が半分になる。ログは止めても直近の行が Docker の側に残るが、DB の書き込みの失敗は取り返しがつかないので、早めに止める側に倒す。
- **目印の行**: 捨て始めるところに、コレクタが 1 行だけ書く。本文は、1 日の上限では `[koroku] The log of this service reached 100 MiB for this day. Further lines are dropped until the day changes ([logs] daily_limit_mb).`、ディスクの使用率では `[koroku] The disk of the host is 90% full or more. Log lines are dropped until space is freed.`。時刻は捨てた最初の行のもの。ディスクの使用率で捨てたときは、書けるようになってからもう一度止まれば、また 1 行書く。日付が変わっても止まったままなら、新しい日のファイルにも 1 行書く。1 日の上限に届いた日には、ディスクの目印は足さない。ふつうの行として保存するので、ライブ表示、過去ログの検索、退避したファイルのどれでも、行が欠けていることが分かる。捨てた行はライブ表示にも流さない（保存されたものと表示をそろえる）。
- **捨てた行数**: `koroku` 自身のログに、捨て始めたとき（`dropping log lines`）、続いている間は 10 分ごと（`still dropping log lines`）、そのサービスの行をまた書けたときとアプリの収集を終えるとき（`log lines were dropped`）に、アプリ・サービス・理由（`daily_limit` か `disk_usage`）・日付・行数を出す。ディスクの使用率で止めたときと再開したときは、使用率としきい値も出す（`the disk is almost full; ...`、`the disk has room again; ...`）。行の本文は出さない。
- **再起動をまたぐ扱い**: 上限に届いたかどうかは、ファイルの大きさと最後の行から決める。最後の行がいまの設定の目印の行なら、その日は閉じたまま。最後の行がディスクの目印で、ディスクがまだ埋まっていれば、同じ欠落の続きとして目印を足さない。目印の本文には上限の値が入るので、上限を上げて `koroku` を再起動すれば、その日のうちにまた書き始める。再開時の重複の除去（上記）はファイルにある行だけを数えるので、捨てた行が Docker からもう一度届いても、その日が閉じていればまた捨て、行が重複することはない。ディスクの使用率で捨てた行が、空きが戻ったあとの再起動でもう一度届いたときは、そこで書かれる（目印より後ろに、古い時刻の行として入る）。圧縮済みの日に遅れて届いた行は、圧縮した分に足して数える（その日が上限に届いていれば、遅れた行の代わりに目印の行がもう 1 行入る）。
- **退避**: 退避はファイルのバイト位置を追うだけなので、捨てた行があっても変わらない。目印の行はほかの行と同じように退避される。1 オブジェクトは 1 日の上限より大きくならない。
- **休止の見張り（8.4）**: アクセスログの行は、捨てるものも含めて見張りに渡す。上限に届いても、ディスクの使用率で止まっていても、アクセスの有無は数え続ける。
- **既定値の根拠**: 40 GB のディスクを、Docker のイメージとビルドキャッシュ、各アプリの volume と分け合う。5 アプリにそれぞれコンテナのサービス 3 つと `_access` があるとして日ごとのファイルは 20 本。そのすべてが毎日上限に届く最悪の場合で、当日の分が 20 × 100 MiB = 2 GB、圧縮した過去 14 日分がテキストを 1/10 と見て 14 × 20 × 10 MiB = 2.8 GB、合わせて約 5 GB（ディスクの 12%）に収まる。1 つのサービスだけが暴走した場合は 100 MiB + 14 × 10 MiB = 240 MiB。ふつうの利用では、アクセスログ（1 行 400〜500 バイト）で 1 日 20 万リクエスト強（終日 2〜3 リクエスト/秒）、150 バイトの行を出すコンテナで 1 日 70 万行（終日 8 行/秒）まで欠けない。圧縮率は目安で、同じ行が続く出力ではもっと縮む。
- **上限の範囲**: 2048 MiB まで。日ごとのファイルの大きさを gzip の末尾の値（4 GiB で一巡する）から読むので、1 日分がそこに届かないようにする。

### 10.2 バックアップ

バックアップは restic で S3 互換のオブジェクトストレージに取る。restic は S3 互換ストレージをリポジトリにできる（[restic documentation](https://restic.readthedocs.io/)）。重複排除・暗号化・世代管理を自前で書かずに済む。

| 対象 | 方法 |
| --- | --- |
| 基盤（`koroku.db`、`/etc/koroku`） | SQLite は `VACUUM INTO` で整合したコピーを作ってから `restic backup` |
| アプリの named volume | `docker volume inspect` で得たマウントポイントを稼働中のまま `restic backup`。アップロードファイルなど向け |
| アプリの DB | アプリ設定の「ダンプフック」（サービス名 + コマンド。例: `db` / `pg_dump -U postgres app`）を `docker compose exec -T` で実行し、標準出力を `restic backup --stdin` に流す（[restic manual](https://restic.readthedocs.io/en/stable/manual_rest.html)） |

- レビューアプリ（9.9）は、定期と手動のバックアップの対象にしない。データは親の写しでいつでもコピーし直せ、restic の世代管理でスナップショットがアプリを消したあとも数か月残るため。
- 稼働中の DB のデータディレクトリをファイルコピーしても整合性は保証されない。DB はダンプフックで取り、その volume はアプリ設定で対象から外せるようにする。
- **スケジュール**: 毎日 03:00 JST。UI から手動実行もできる。ダンプは、アプリごとに短い間隔でも取れる（10.8）。「デプロイ前に取得」はアプリ単位のオプション（既定オフ）。
- **世代**: `restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune`（直近 48 時間のダンプはすべて残す。10.8。時点指定の復旧のスナップショットは別に管理する。10.9）。スナップショットには `app:<slug>` と `kind:system|volume|dump` のタグを付ける。
- **監視**: 週 1 回 `restic check`。最終成功時刻を UI に出し、24 時間以上成功がなければ警告表示する。外部通知は後回し。
- **結果の記録はアプリ単位**: 1 回の実行は全アプリをまとめて取るが、結果は対象（アプリの volume・ダンプ、基盤）ごとに `backup_run_targets` に残す（成功 / 警告 / 失敗と、伏せ字にしたメッセージ。ダンプフックの出力は、そのアプリの環境変数の値も伏せる。下の細則）。実行全体のエラー文字列は他のアプリの対象も含むので、アプリの画面には出さない。アプリのバックアップタブと概要タブは、そのアプリの対象を含む直近の実行（デプロイ前の取得を含む）の、そのアプリの結果だけを出す。何も取る前に失敗した実行（リポジトリを開けない、`koroku` の再起動で中断した）は全アプリに関わるので、どのアプリにも「実行全体が失敗した」として出す（メッセージはオーナーにだけ）。
- **目立たせる**: 直近の取得が失敗したアプリ（と、オーナーには基盤）は、アプリ一覧の「対応が必要」の欄に、アプリごとにバックアップタブへのリンク付きで出す。何も取る前に失敗した実行は、アプリごとではなく 1 件にまとめて出す。読めないファイルがあって警告付きで保存したものは、アプリのタブには出すが、一覧の欄には出さない（取れてはいるため）。
- **バケット**: 1 バケットを `restic/` と `logs/` に分ける。ライフサイクルルールは `logs/` だけに掛ける（`restic/` を期限削除するとリポジトリが壊れる）。`restic/` は restic がリポジトリのパスワードで暗号化する（`koroku.db`、鍵を含む `/etc/koroku`、volume、ダンプ）。`logs/` は暗号化しない（10.1）。バケットを読めるだけの者は、バックアップは読めないが、全アプリのログは読める。
- **削除したアプリのスナップショット**: アプリを削除しても、そのアプリのスナップショット（タグ `app:<slug>`）は消さず、世代管理が消すまで残る。一覧・リストア・ダンプのダウンロードはタグの slug で引くので、同じ slug で作り直したアプリのバックアップタブに出て、管理のロールでリストアとダウンロードができる（10 章の「起動制御と削除」）。
- **ストレージの選択**: S3 互換 API（署名 V4、パス形式の URL）だけを使い、事業者固有の API は使わない。事業者の切り替えは `config.toml` の endpoint・region・バケット名・アクセスキーを変えるだけで済む。

**リストア**

1. volume: UI でスナップショットを選ぶ → アプリ停止 → 現在の中身を `/var/lib/koroku/restore-trash/` に退避（7 日保持）→ `restic restore` → アプリ起動。
2. DB ダンプ: ロードフックがあれば、選んだダンプへ koroku の中で戻す（10.8）。なければ UI からダウンロードするところまでで、流し込みは手動（手順を画面に表示）。PostgreSQL の DB は、指定した時刻へも戻せる（10.9）。
3. ホスト全損: 新しい VPS をプロビジョニング → `koroku restore-system` で `koroku.db` と `/etc/koroku` を戻す → 各アプリを再デプロイして volume をリストア → DNS を新 IP に向ける。

**事前準備と秘密情報**

- オブジェクトストレージにバケットを作り、そのバケットに限定したアクセスキー（Access Key ID / Secret Access Key）を発行して `/etc/koroku/config.toml` に置く。
- restic のリポジトリパスワードは `/etc/koroku/restic.key`。これとオブジェクトストレージの認証情報は VPS の外（パスワードマネージャなど）にも必ず控える。失うとホスト全損時に復元できない。

**バックアップの細則**

- restic は 0.17 以上が要る（リポジトリが未作成かどうかを終了コード 10 で判定するため）。初期化はバックアップのときだけ行い、一覧やリストアでは行わない。バケットそのものは `koroku` も restic も作らない。存在しないバケットを指定すると restic が数分リトライしたあと失敗する。
- タグは、基盤が `system` と `kind:system`、アプリが `app:<slug>`・`kind:volume|dump`・`target:<volume 名かダンプフックのサービス名>`。ホスト名は固定の `koroku`。volume と基盤はディレクトリの中から `.` として取るので、ホストやパスが変わってもリストアできる。世代管理は `forget --group-by tags` で、volume・ダンプ・基盤それぞれが自分の世代（日次 7・週次 4・月次 6）を持つ。
- 「アプリの volume」は、そのアプリの Compose プロジェクト（`koroku-<slug>`）のラベル `com.docker.compose.project` が付いた named volume のうち、Docker に実在するもの。名前は Compose ファイルに書いた名前（ラベル `com.docker.compose.volume`。`koroku-blog_pgdata` ではなく `pgdata`）で扱い、除外リストもスナップショットの `target` もこの名前を使う。`external` の volume はラベルを持たないので対象外（他者の持ち物）。Compose ファイルから消えても Docker に残っている volume は対象のまま。`GET /api/apps/{slug}/volumes`（9.1）も同じ列挙を使う。
- ダンプフックは `docker compose exec -T <サービス> sh -c <コマンド>` の標準出力をパイプで restic に渡す。コマンドが失敗したら、restic が入力の終わりを見る前に止めるので、途中までのダンプがスナップショットとして残ることはない。出力が空だった場合も失敗にする。停止中のアプリではダンプフックを実行しない（コンテナがないため）。休止中のアプリでは、フックのサービスだけを一時的に起動して実行する（8.4）。
- volume のリストアは、アプリ停止 → 現在の中身を `/var/lib/koroku/restore-trash/<slug>-<volume>-<時刻>/` へ移動（7 日保持）→ 復元 → アプリ起動。復元に失敗したら途中まで書いた内容を消し、退避した中身を戻してアプリを起動する。もともと停止していたアプリは停止のままにする。退避先は volume と同じファイルシステムにある必要があり、アプリを止める前に確かめる。
- 実行は同時に 1 つ。定期実行のあとに `forget --prune`、日曜はさらに `check` を行い、どれかが失敗したらその回は失敗として記録する（UI に伝える手段がこれだけなので）。`koroku` の停止と重なった定期実行はやり直さない。
- 「デプロイ前に取得」は、ビルドと pull のあと、コンテナを入れ替える直前に、そのアプリだけを対象に行う。失敗したらデプロイを中止し、稼働中のリリースには触れない。別のバックアップが実行中の場合も中止する（待たない）。初回デプロイでは取るものがないので行わない。
- 稼働中のアプリの volume を取るとき、読めないファイルがあっても（restic の終了コード 3）スナップショットは残し、警告として記録する。
- ダンプフックが失敗したときに残すメッセージは、コマンドの標準エラー出力の末尾（5 行、600 バイトまで）を含み、`backup_run_targets` を通じてそのアプリの閲覧のロールにも見える。`pg_dump` などは失敗すると接続文字列を出すので、ストレージのアクセスキーに加えて、そのアプリの環境変数の値を、デプロイログやコマンドのログと同じ伏せ字（`internal/secrets`）で隠す。隠すのは、保存してある値と、稼働中の env ファイルに残っている値（最後のデプロイのあとに消した変数の値を、コンテナはまだ持っている）。DB のコピーの前に取るダンプ（10.6）も同じ経路を通る。`internal/backup` は DB も復号の鍵も知らないので、値は呼び出し側がアプリごとに渡す関数（`backup.App` の `OutputMasker`。デプロイエンジンがコマンドのログに使うものと同じ伏せ字を返す）から、ダンプのたびに受け取る。値を読めなかったとき（復号の失敗など）は、ダンプは実行するが、コマンドの出力は 1 行も残さず、メッセージは終了コードと「出力を残さなかった」ことだけになる。メッセージより、取れるバックアップを優先するため。伏せ字の限界（3 文字未満の値、変換された値）は 10 章の「前提と限界」のとおり。
- restic の認証情報は環境変数で渡し、引数・エラー・ログには出さない。キャッシュは `/var/lib/koroku/restic-cache` に置く（systemd のサービスには HOME の下のキャッシュがないため）。
- `koroku restore-system` は、基盤のスナップショットを取り出し、`koroku.db` と設定ディレクトリのファイルを置く。既にあるファイルは上書きせず、バックアップにあった版を隣に `<名前>.restored-<時刻>` として置く。`koroku.db` が既にあるホストでは `--force` なしでは実行しない。
- 基盤のバックアップで設定ディレクトリをコピーするとき、その下にあるデータディレクトリ（`data_dir`）と、コピー先の作業ディレクトリ自身は除外する。ローカル環境では両方を `data/dev` の下に置くので、除外しないと作業ディレクトリを何重にもコピーし続けてしまう。本番は `/etc/koroku` と `/var/lib/koroku` が別なので関係しない。
- volume のリストアで中身を退避する移動は `rename` で行い、`EXDEV`（マウントをまたぐ移動）で失敗したときだけ、所有者・パーミッション・時刻を保ったコピーと元の削除に切り替える。事前に「同じファイルシステムか」は確かめるが、同じファイルシステムでも別の bind mount を経由すると Linux は `rename` を拒むため（ローカル環境は、`koroku` のコンテナにデータディレクトリと Docker の volume ディレクトリを別々にマウントしている）。コピーではハードリンクと拡張属性は保たれないが、退避は手作業での救出用なので許容する。ソケット・デバイス・名前付きパイプはコピーせず、リストアを失敗させて元に戻す。本番は同じマウントなので常に `rename` になる。

### 10.3 ログインの強化

オーナーとロールを持つメンバー以外はログインできない（5 章、10.7）。そのうえで、「ログインできるユーザーの GitHub アカウントか、koroku のセッション Cookie が盗まれた場合」の被害を小さくし、盗まれたことに気づけるように、次の 3 つを重ねる。管理 UI をネットワークの段階で閉じる仕組み（Tailscale、クライアント証明書など）は持たない。

**GitHub の 2 段階認証は GitHub に任せる**

- オーナーとメンバーの GitHub アカウントで 2 段階認証を有効にしておく（`docs/SETUP.md`）。koroku のログインは GitHub へのサインインを経るので、2 段階認証は GitHub がそこで求める。
- koroku 自身は 2 段階認証の状態を確かめない。`GET /user` の `two_factor_authentication` は、`read:user` / `user` スコープを持つ OAuth App のトークンや classic な個人用アクセストークンへの応答（private user）にだけ含まれ、GitHub App のユーザーアクセストークンには返らないため。
- 古い版の `config.toml` にある `[github] require_two_factor` は読み込めるが使わず、起動時に「消してよい」と警告を出す（知らないキーで起動を止めないため）。

**危険な操作の前に再ログインを求める（step-up）**

- セッションの発行（ログイン）から 10 分以内でなければ、次の操作を `403 reauth_required` で断る。オーナーにもメンバーにも同じに掛かる（ロールの判定のあとで確かめるので、ロールで断られる操作に再ログインを求めることはない）。UI は再ログインの案内を出し、GitHub のログインを経て元の画面に戻す。
  - アプリの削除
  - 本番アプリ（`environment = production`）のデプロイ
  - 環境変数の保存（一括置換も 1 件ずつの追加・変更・削除も、作成時に `env` で一緒に保存するのも。9.8）と、値の表示（下記）
  - volume のリストアと、DB ダンプのダウンロード
  - DB のコピー（10.6）と、レビューアプリの作成（9.9。親の環境変数とデータを写すため）
  - レビューアプリの許可（9.9）を有効にすること。pull request のコードが親の環境変数とデータを受け取れるようになるため。無効にするのは求めない
  - コンテナレジストリの認証の保存と削除（6.1）
  - アプリ設定のうち、`environment`、`relax_policy`、リポジトリ（`repo_full_name`、`repo_url`、`repo_id`）、Compose の置き場所と本文（`compose_source`、`compose_content`。6.3）の変更。本番の確認ダイアログを外す、ポリシー検査を外す、別のコードを動かす、のいずれかにつながるため
  - 本番アプリで自動デプロイ（5.2）を有効にすることと、自動デプロイを有効にしたまま追うブランチ（`auto_deploy_branch`、空なら既定のブランチ）を変えること。push だけで本番が変わるようになるため。無効にするのは求めない
  - アクセスの保護（8.3）を外す変更と、保護を外すパスの一覧の変更。アプリを外へ開くことにつながるため
  - 本番アプリでのコマンドの実行と、登録タスクの変更（9.6）
  - 本番アプリのアプリ設定のうち、ダンプフック（10.2）、ロードフック、コピー後のコマンド（10.6）のサービスとコマンドの変更。ダンプフックは、バックアップのたびに koroku がアプリのコンテナの中で実行するので、保存はコマンドの実行と同じ扱いにする。ロードフックとコピー後のコマンドは、アプリが本番の間は実行されないが（DB のコピーの受け側は staging だけ）、コンテナの中で動くコマンドの設定として同じに扱う。今と同じ値を送るのは求めない
  - メンバーの追加と、ロールを与えるか上げる変更（10.7）。管理 UI を別のアカウントに開くことにつながるため。メンバーの削除とロールを外す・下げる変更は求めない（被害を止める操作を遅らせない）
- 環境変数の `GET` は、10 分を過ぎていても断らず、値を `null` にして `values_hidden: true` を返す。キーの一覧は見られるようにして、タブそのものは使えるようにするため。
- 再ログインは通常のログインと同じ流れで、新しいセッションを発行する。同じブラウザーの古いセッションはそのとき消す。戻り先は `GET /auth/github/login?return_to=<パス>` で渡し、state と同じく短命の Cookie に入れる。`/` で始まり `//` や `\` を含まないパスだけを受け付ける（外部サイトへのリダイレクトに使われないように）。
- 限界: GitHub にログインしたままのブラウザーでは、再ログインはパスワードを聞かれずに終わる。防げるのは「Cookie だけが盗まれて別の場所で使われる」場合で、本人のブラウザーそのものを使われる場合は防げない。GitHub の OAuth には OpenID Connect の `max_age` に当たるものがない。
- 再ログインを求めない操作: 作成（環境変数を含まないもの）、staging のデプロイ、起動制御（休止中のアプリを起こすことと、休止の設定を含む。8.4）、ドメイン、バックアップの実行、prune。どれも取り消せるか、データを外へ出さないため。

**セッションの一覧と失効、ログイン履歴**

- `sessions` は `id`（画面と API で使う識別子。乱数 8 バイトの 16 進。Cookie のハッシュは外に出さない）、`last_seen_at`、`ip`、`user_agent` を持つ。`last_seen_at` は 1 分以上たっていれば更新する（ポーリングのたびに書き込まないため）。
- クライアントの IP: `koroku` はループバックで待ち受け、Caddy を経由して届く。接続元がループバックのときだけ `X-Forwarded-For` の最後の値を使う。Caddy は既定でクライアントから来た `X-Forwarded-For` を信用せず、接続元の IP で置き換えるので、偽装できない。User-Agent は 512 バイトで切る。
- `login_events` テーブル: 時刻、結果（`succeeded`、`not_allowed`。古い版が残した `two_factor_required`、`two_factor_unknown` もそのまま表示する）、GitHub のユーザー ID と login、IP、User-Agent。GitHub のユーザーが分かったものだけを残し、state の不一致や GitHub の失敗は残さない。誰でも起こせて、記録が溢れるだけなので。新しい 200 件を残して古いものを消す。
- セッション、ログイン履歴、API トークンはユーザーごとに持つ。メンバーは自分のものだけを見て終了・失効でき、オーナーは全員のもの（拒否されたログインの試みも含む）を見て、誰のものでも終了・失効できる。一覧にはユーザーの login を出す。
- セッションを終了するか、期限が切れると、そのセッションで開いたままのログの SSE も、次の確認（15 秒ごと）で閉じる（10.7 の「判定の仕組み」）。終了は「盗まれた Cookie を止める」操作なので、開いている接続だけが読み続けられる、ということがないようにする。
- 画面: ユーザーメニューから「セキュリティ」を開き、セッションの一覧（このセッションを示す、個別に終了、ほかをすべて終了）と、ログイン履歴を見る。「ほかをすべて終了」は、オーナーでも自分のほかのセッションだけを終了する。

### 10.4 設定の点検と SSH

外部サービスの設定の誤りを早く見つけるための `koroku check` と、SSH の入口を狭めるプロビジョニングの手順。利用者向けの手順は `docs/SETUP.md`。

**`koroku check`**

- 外部サービスの設定は手作業で、誤りは「ログインできない」「バックアップが夜に失敗する」のように、使ってみるまで表に出ない。`koroku check` は `config.toml` を読み、次を 1 つずつ確かめて、直し方と `docs/SETUP.md` の節を示す。失敗が 1 つでもあれば終了コード 1。
  - 例の値（`example.com`、`203.0.113.x`）が残っていないこと。`public_ip` がホストのインターフェースのアドレスにあること（ないときは警告。NAT の下ならありうる）。
  - DNS: `console_host` と、登録していない名前（`koroku-check-<乱数>.<base_domain>`）が `public_ip` だけに解決されること。後者が解決されなければワイルドカードがないとして警告する（アプリごとの登録でも運用できるため）。判定は独自ドメインの検証（8 章）と同じ関数。
  - Caddy の admin API が応答すること。`console_url` に HTTPS で接続でき、証明書が信頼できること。`acme_ca` が Let's Encrypt の staging なら、信頼できない証明書は警告にとどめ、本番への切り替えを促す。
  - `master.key` があり 0600 であること（ないときは警告。初回起動で作られる）。
  - GitHub App: App の JWT で `GET /app` が通り、`app_id`・`client_id` が一致し、権限が Contents と Metadata の read（足りなければ失敗）と Deployments の write（なければ GitHub への反映が止まる旨の警告）で、それ以外がないこと（余分なら警告。Actions の read と Pull requests の read は余分に数えない）。`GET /app/installations` で 1 つ以上インストールされていること。インストール先は、アカウントの login と ID を並べて表示する（`my-org (ID 2002, all repositories)`）。koroku が使わないアカウントへのインストールがあれば警告する。`installation_accounts`（login での指定。5 章）が書いてあれば警告し、login だけで一致しているアカウントがあるときは、代わりに書く行（`installation_account_ids = [2002]`。設定済みの ID も含める）を示す。`allowed_user_id` が実在するアカウントであること（`GET /user/{id}`、認証なし）。callback URL は API で読めないので、正しい値を表示するだけにする。
  - 自動デプロイ（5.2）: 有効なアプリがあるのに `webhook_secret` がない、App が push のイベントを購読していない（`GET /app` の `events`）、App の Webhook の URL が `<console_url>/webhooks/github` でない（`GET /app/hook/config`）、のいずれかなら警告。有効なアプリは DB を読み取り専用で開いて数える。Webhook が Active かどうかは API で読めないので、表示するだけにする。
  - ワークフローを待つ自動デプロイ（5.2）: 待つワークフローを設定した、自動デプロイが有効なアプリがあるのに、App に Actions の read がない、または workflow_run のイベントを購読していない（`GET /app` の `permissions` と `events`）なら警告。
  - レビューアプリ（9.9）: App に Pull requests の read がなければ警告（レビューアプリを作れない旨）。
  - オブジェクトストレージ: `koroku-check/<乱数>` に書き、読み、一覧し、消す。`logs/` と `restic/` の外なので、ライフサイクルルールにも restic にも触れない。トークンの権限不足をここで見つける。
  - restic: 基盤のスナップショットを一覧できること（パスワードの誤りはここで失敗する）。まだ 1 つもなければ警告（リポジトリは最初のバックアップで作られる）。最新が 26 時間より古ければ警告。
- 外部への書き込みはストレージの確認用オブジェクト 1 つだけで、終わったら消す。DNS、GitHub、Caddy は読むだけ。秘密は表示しない。
- `serve` の起動時には行わない。起動時に外部サービスを待つと、GitHub やオブジェクトストレージの障害で管理 UI まで止まるため。

**SSH の keepalive**

- 途中のルーターやファイアウォールの多くは、通信のない TCP 接続を数分で忘れる。そうなると、放置した SSH のセッションは何も届かなくなって固まる。`provision.sh` は sshd に `ClientAliveInterval 30` と `ClientAliveCountMax 4` を設定し（`/etc/ssh/sshd_config.d/02-koroku-keepalive.conf`）、30 秒ごとにクライアントへ確認を送らせる。この通信で途中の機器は接続を覚えたままになり、本当にいなくなったクライアントのセッションは約 2 分で片付く。鍵の有無に関係なく常に入れる。

**SSH を鍵だけにする**

- VPS の OS イメージは、パスワードでの SSH ログインを受け付けるものが多い。`koroku` を動かすホストの root はすべてのアプリと秘密を握るので、`provision.sh` はパスワードと keyboard-interactive の認証を止め、root のログインを禁じる（スクリプトを root で直接実行した場合は鍵での root ログインだけ残す）。設定は `/etc/ssh/sshd_config.d/01-koroku.conf` に置く。sshd は最初に読んだ値を使うので、`50-cloud-init.conf` より先に読まれる名前にする。
- 締め出しを避けるため、`sudo` を実行したユーザー（`SUDO_USER`、なければ root）の `~/.ssh/authorized_keys` に鍵が 1 つ以上あるときだけ行う。ないときは何もせず、最後のまとめで知らせる。書いたあとに `sshd -t` で検査し、通らなければファイルを消して止まる。反映は reload で、開いているセッションは切れない。
- `--keep-ssh-password` で、この手順だけを飛ばせる。

### 10.5 Claude Code から状態を読む API トークン

デプロイが失敗したとき、原因はたいていアプリのリポジトリの側にある（Compose の書き方、足りない環境変数、起動しないコード）。直すのは手元の Claude Code なので、Claude Code が koroku の状態（デプロイの結果、デプロイログ、実行ログ、必要な変数の照合）を直接読めれば、管理 UI を開いてログを貼り付ける手間がなくなる。そのために、**読み取り専用**の API トークンを管理 UI で発行でき、利用者向けのプラグインには、それを使ってデプロイの状況を調べる Skill（`plugins/koroku/skills/deploy-status`）がある。

**なぜ読み取り専用か**

- トークンは開発者の手元のシェル（環境変数）に置かれ、Claude Code が任意のコマンドの中で使う。ブラウザーの Cookie よりも漏れる経路が多い（シェルの履歴、ほかのツールのログ、誤ってコミットしたファイル、プロンプトインジェクションで外へ送られるなど）。
- koroku はホストの root 相当で（10 章）、変更系の API（デプロイ、アプリ設定、環境変数、リストア）は「別のコードを動かす」「秘密を書き換える」ことに直結する。漏れたトークンでこれらができると、Cookie を盗まれた場合と同じ被害になる。10.3 の Cookie の盗難への対策（再ログインの要求）も、トークンには効かない（トークンには「ログインした時刻」がない）。
- 目的（失敗の原因を読んで、リポジトリを直す）には参照だけで足りる。直したあとのデプロイは、これまでどおり管理 UI から人が行う。

**なぜ環境変数の値を出さないか**

- 環境変数の値はアプリの秘密（DB のパスワード、API キー）で、10.3 で値の表示には直近のログインを求める。トークンからはこの再確認ができないので、値を返す API（`GET /api/apps/{slug}/env`）はトークンでは使えない。
- 必要な変数の照合（9.4）は、キーと「設定済みか」だけを返すのでトークンでも使える。Claude Code は「この変数が足りない」までは分かり、値は人が管理 UI で入れる。
- 同じ理由で、データを外へ出す API（DB ダンプのダウンロード、退避済みログの presigned URL）、セッションとログイン履歴、トークン自身の管理も使えない。

**なぜ `Authorization: Bearer` だけか**

- Cookie として受けると、ブラウザーが自動で付けるので CSRF の対策（`Origin`、`Sec-Fetch-Site`）がトークンにも要るうえ、兄弟サブドメインのアプリから Cookie を差し込まれる問題（10 章の `__Host-`）が戻ってくる。`Authorization` ヘッダーはブラウザーが自動では付けず、別オリジンから付けるには CORS のプリフライトが要る（koroku は応答しないので送られない）。
- クエリパラメーターでは受けない。URL はアクセスログ、ブラウザーの履歴、`Referer` に残る（koroku のリクエストログはクエリを落としているが、前段の Caddy やほかのツールは残しうる）。
- トークンのリクエストでは Cookie を見ない。両方が付いていてもトークンとしてだけ扱い、Cookie の権限が混ざらないようにする。

**仕様**

- トークンはユーザーごとに持ち、発行したユーザーとして、そのユーザーの権限（10.7）で動く。メンバーのトークンは、そのメンバーがロールを持つアプリしか読めない。メンバーを外すか、メンバーがロールをすべて失うと、そのトークンは消え、残っていても `401 invalid_token` になる。メンバーが登場する前に発行したトークンは、`koroku serve` の起動時にオーナーのものにする。
- 管理 UI の「セキュリティ」画面で発行・一覧・失効する。発行には直近 10 分以内のログイン（10.3）が要る。長く使える読み取りの手段を増やす操作なので、Cookie だけを盗まれた場合に発行されないようにする。失効は再ログインなしでできる（被害を止める操作を遅らせない）。
- 項目は名前（用途のメモ。例: 「ノート PC の Claude Code」）、発行日時、有効期限（7・30・90 日から選ぶ。最長 90 日、無期限はない）、最終利用日時と IP。一覧には期限内のものだけを出す。有効なトークンはユーザーごとに同時に 20 個まで。
- 値は `koroku_ro_` に乱数 32 バイトの base64url（43 文字）を続けたもの。接頭辞はシークレットスキャナー（GitHub の push protection など）が正規表現 `koroku_ro_[A-Za-z0-9_-]{43}` で見つけられるようにするため。値は発行時の応答で 1 回だけ返し、DB にはセッションと同じく SHA-256 のハッシュだけを置く（`api_tokens` テーブル）。失効は行を消す。期限切れの行は発行のときに消す。
- 使えるのはトークンの許可リストにある `GET` だけで、それ以外のパスやメソッドは、トークンが有効でも `403 token_not_allowed`。許可リストはパスのパターンで持ち、ハンドラーごとの判断に任せない（新しい API を足したとき、明示的に足さない限りトークンでは使えない）。
  - `GET /api/apps`、`GET /api/apps/{slug}`、`GET /api/apps/{slug}/services`
  - `GET /api/apps/{slug}/deployments`、`GET /api/deployments/{id}`、`GET /api/deployments/{id}/logs`（SSE と `?format=text`）
  - `GET /api/apps/{slug}/logs/history`、`GET /api/apps/{slug}/logs`（ライブの SSE）
  - `GET /api/apps/{slug}/env/requirements`
  - `GET /api/apps/{slug}/compose`、`GET /api/apps/{slug}/containers`（値を伏せた構成とコンテナ。9.10）
  - `GET /api/system`
- 期限切れ・失効・形式違いのトークンは `401 invalid_token`。Cookie のない `401 unauthenticated` と分け、Skill が「トークンを作り直す」と案内できるようにする。トークンで開いたままのログの SSE も、トークンが失効するか期限が切れると、次の確認（15 秒ごと）で閉じる（10.7 の「判定の仕組み」）。漏れたトークンを失効しても、開いている接続だけがログを読み続ける、ということがないようにする。
- `Origin` の検査は変更系だけのものなので、`GET` しか通らないトークンのリクエストには関係しない。`Sec-Fetch-Site` の検査（ブラウザーだけが付ける）、`--insecure-no-auth` のときのループバック名の検査、セキュリティヘッダーは、トークンのリクエストにもそのまま掛ける。
- トークンごとに 1 分あたり 120 回まで。超えたら `429 rate_limited`（`Retry-After` 付き）。Skill の 1 回の調査は 10 回程度で、ライブの SSE を張りっぱなしにしても数えるのは接続の 1 回だけ。
- 最終利用は 1 分以上たっていれば記録する（セッションの `last_seen_at` と同じ。リクエストのたびに書き込まないため）。
- トークンの値はログ・エラーメッセージに出さない。リクエストログはもともとパスだけを記録し、ヘッダーを出さない。トークンを使ったリクエストの記録には、値ではなくトークンの `id` を付ける。
- `--insecure-no-auth` のときも、トークンのリクエストは同じく検査する（開発環境で Skill を試せるように）。

**残る危険**

- 実行ログとデプロイログは読める。デプロイログは環境変数の値でマスクしているが（10 章の制約どおり、3 文字未満や変換された値は隠れない）、アプリが自分で秘密をログに出していれば、それはトークンで読める。
- アプリの JSON には設定（ダンプフックのコマンドなど）が含まれる。ダンプフックのコマンドに秘密を直に書かないよう、画面と Skill で案内する（環境変数で渡せる）。
- トークンが漏れると、期限まで（最長 90 日）アプリの構成とログが読まれる。最終利用の日時と IP を一覧に出し、心当たりのない利用に気づけるようにする。

**採らない方式**

- 変更系も使えるトークン（スコープ付き）: 「直したらそのままデプロイ」まで自動にできるが、上の理由で漏れたときの被害が Cookie の盗難と同じになる。必要になったら、本番を除く・再ログインに相当する確認を挟む、などを別に設計する。
- GitHub の OAuth（Device flow）で Claude Code にもログインさせる: トークンの保存場所が増えるだけで、読み取りに絞る仕組みは結局 koroku 側に要る。

### 10.6 本番の DB を staging にコピーする

staging アプリ（8.2）で、本番に近いデータを使って確かめたいことがある。手作業（ダンプをダウンロードして手元から流し込む）だと、本番のデータが手元のディスクに残り、流し込む先を取り違えると本番を壊す。そこで、koroku の中で本番アプリの DB を staging アプリへ直接流し込む操作を持つ。

**仕組み**

- 同じ仕組みで、アプリ自身のダンプへ戻す（10.8）。
- DB の種類を koroku は知らない。送り側は 10.2 のダンプフック（サービス + 標準出力にダンプを出すコマンド）をそのまま使い、受け側にはアプリ設定の「ロードフック」（サービス + 標準入力からダンプを読むコマンド）を足す。画面は種類ごとの既定値（プリセット）を出すだけで、保存されるのはコマンドの文字列。

| 種類 | ダンプフック（送り側） | ロードフック（受け側） |
| --- | --- | --- |
| PostgreSQL | `pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"` | `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"` |
| MySQL | `mysqldump --single-transaction --routines --triggers -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"` | `mysql -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"` |
| MariaDB | `mariadb-dump --single-transaction --routines --triggers -u root -p"$MARIADB_ROOT_PASSWORD" "$MARIADB_DATABASE"` | `mariadb -u root -p"$MARIADB_ROOT_PASSWORD" "$MARIADB_DATABASE"` |
| SQLite | `sqlite3 /data/app.sqlite3 .dump` | `set -e; rm -f /data/app.sqlite3.load; sqlite3 /data/app.sqlite3.load; mv /data/app.sqlite3.load /data/app.sqlite3` |

- 流れは、送り側の `docker compose -p koroku-<送り側> exec -T <サービス> sh -c <ダンプのコマンド>` の標準出力を、受け側の `docker compose -p koroku-<受け側> exec -T <サービス> sh -c <ロードのコマンド>` の標準入力へつなぐ。ダンプはディスクに書かない。間に `koroku` のプロセスを挟み、流れたバイト数を数えて上限で止める。
- ダンプとロードはそれぞれのコンテナの中のクライアント（`pg_dump` と `pg_restore` など）で動く。**送り側と受け側の DB のメジャーバージョンは揃える**。新しい `pg_dump -Fc` の出力は古い `pg_restore` では読めない。
- 取り元は 2 つから選ぶ。
  - `live`: 稼働中の送り側でダンプフックを実行する。最新のデータが取れるが、本番にダンプの負荷がかかる。送り側が稼働中でなければ断る。
  - `snapshot`: 送り側の最新のダンプのスナップショット（10.2）を `restic dump` で読んで流す。本番に負荷をかけないが、データは最後のバックアップの時点のもの。バックアップが設定されていて、送り側のダンプのスナップショットが 1 つ以上あるときだけ選べる。
- 送り側は受け側以外のどのアプリでもよい（同じリポジトリに限らない）。送り側は読むだけで、リポジトリの名前を変えた staging やフォークにもコピーできるようにするため。取り違えは、受け側の制限（staging のみ）と、受け側の slug の入力と、直前のバックアップで抑える。画面では、受け側と同じリポジトリの本番アプリを先頭に出す。

**手順**（ログの見出しは 9.1 のデプロイと同じ `==> <時刻> <キー> <表題>` の形）

1. `check`: 受け側が `staging` で、デプロイ済みで稼働中で、ロードフックがあり、そのサービスが動いていること。受け側と、`live` のときの送り側が休止中（8.4）なら、ここで起こす。`live` なら送り側にダンプフックがあり稼働中であること、`snapshot` なら送り側のダンプのスナップショットがあること。
2. `backup`: 受け側のダンプフックでダンプを取り、restic に残す（10.2 のダンプと同じスナップショット。`kind:dump`）。失敗したら何も変えずに中止する。バックアップが未設定か、受け側にダンプフックがなければ、その旨をログに書いて飛ばす。この取得は `backup_runs` には記録せず、コピーの記録にスナップショットの ID を残す（`backup_runs` の `trigger` は定期・手動・デプロイ前の 3 つで、最終成功時刻の判定にも使うため）。
3. `stop`: 受け側の稼働中のサービスのうち、ロードフックのサービス以外を `docker compose stop <サービス...>` で止める。アプリが DB への接続を握ったままだと、テーブルの削除が待たされたり、流し込みの途中のデータを返したりするため。
4. `copy`: ダンプをロードへ流す。ダンプのコマンドが失敗したら、ロード側が入力の終わりを見る前に止める（途中までのダンプを完全なものとして読ませない。10.2 のダンプと同じ考え方）。ロード側が先に終わったら、ダンプ側を止めて失敗にする。ダンプが空なら失敗にする。
5. `after`: アプリ設定の「コピー後のコマンド」（サービス + コマンド。例: `web` / `bin/rails db:anonymize`）があれば実行する。個人情報を消すため。そのサービスが動いていれば `exec -T`、止めたサービスなら稼働中のリリースのファイルで `run --rm --no-deps -T`（アプリを公開する前に、1 回限りのコンテナで実行する）。失敗したらコピーを失敗にする。
6. `start`: 3 で止めたサービスを `docker compose start` で起動する。2 や 3 で失敗した場合も起動する（受け側を止めたままにしない）。ただし、コピー後のコマンドを設定していて 4 か 5 で失敗したときは**起動しない**。途中まで流し込んだ本番のデータが、消すべき情報を残したまま公開されうるため。ログにその旨を書き、利用者がコピーをやり直すか、画面から起動する。
7. `done`（失敗時は `failed`。表題に理由）。

**安全策**

- 受け側は `environment = staging` に限る。本番アプリへは API が断る（`422 target_not_staging`）。
- 画面では受け側の slug の入力を求め、API も本文の `confirm` が受け側の slug と一致しなければ断る（`422 confirmation_mismatch`）。
- 再ログインから 10 分以内でなければ断る（10.3）。本番のデータを外へ出す操作のため。
- 受け側の操作のロック（10 章の起動制御と削除）を取る。`live` では送り側のロックも取る（ダンプの途中でコンテナが入れ替わらないように）。どちらかがデプロイ中か操作中なら `409 busy`。コピー中に始めたデプロイは、ほかの操作と同じくコピーの完了を待つ。
- 全体で 2 時間、流すデータは 20 GiB で打ち切る。
- ログには両アプリの環境変数の値をマスクして書く。書くのはコマンドの標準エラーと koroku の行だけで、ダンプの中身は書かない。
- 失敗したとき、受け側の DB は途中まで流し込んだ状態でありうる（`pg_restore --clean` は先に消す）。直前のバックアップ（2）をダウンロードして戻すか、コピーをやり直す。

**記録**

- テーブル `db_copies`: `id`、`target_app_id`（アプリの削除で消える）、`source_app_id`（送り側の削除で `NULL`）、`source_slug`（送り側を消しても名前を残す）、`source_kind`（`live` / `snapshot`）、`status`（`running` / `succeeded` / `failed`）、`error`、`bytes`、`backup_snapshot_id`、`started_at`、`finished_at`。
- ログは `/var/lib/koroku/apps/<受け側>/db-copies/<id>.log`。画面は SSE で追う（デプロイログと同じ配信）。
- `koroku` の再起動で中断したコピーは、起動時に `failed` にする。そのとき 3 で止めたサービスは止まったままなので、画面から受け側を再起動する（ログの末尾にその旨を書く）。
- 実行中のコピーを画面から止める手段は持たない（今後の課題）。

### 10.7 メンバーとアプリごとのロール

koroku はオーナー 1 人が運用する基盤で、オーナーは特定の GitHub ユーザーに特定のアプリだけを任せられる。手伝ってくれる人にデプロイだけ頼む、ログを見てもらう、といった用途で、そのたびにオーナーのアカウントやトークンを渡さずに済むようにするため。

**オーナーとメンバー**

- **オーナー**は `config.toml` の `[github] allowed_user_id` のユーザー。すべての操作ができ、外せない。メンバーを管理できるのはオーナーだけ。
- **メンバー**は、オーナーが管理 UI のシステムの「管理」のタブ（9.1）の「メンバー」で加えた GitHub ユーザー。GitHub の login を入れると、koroku が GitHub の公開 API（`GET /users/{login}`、認証なし）で数値の ID を引き、ID と login を保存する。照合に使うのは ID だけで、login は表示用（メンバーがログインするたびに GitHub の値で書き直す）。login は変えられ、手放した login は別人が取れるので、ID でなければ同じ人と言えないため。organization は加えられない（ログインできないため）。オーナー自身は加えられない。
- ログインできるのは、オーナーと、ロールを 1 つ以上持つメンバー。加えただけでロールのないメンバーはログインできない（`not_allowed`）。
- メンバーを外すと、ロール、セッション、API トークンを消す。ロールを外してメンバーのロールがなくなったとき、アプリの削除でロールがなくなったときも、そのメンバーのセッションと API トークンを消す。消し損ねても、セッションとトークンはリクエストのたびに「オーナーか、ロールを持つメンバーか」を確かめるので通らない。メンバーが開いたままのログの SSE も、ロールを外すかメンバーを外すと、次の確認（15 秒ごと）で閉じる（下の「判定の仕組み」）。

**ロール**

ロールはアプリごとに 1 つで、段階になっている。上のロールは下のロールのできることをすべて含む。

| ロール | できること |
| --- | --- |
| 閲覧（`view`） | アプリを見る: 概要、デプロイの履歴とログ、実行ログ（退避済みログのダウンロードを含む）、タスクの登録と実行の履歴、ドメイン、バックアップの一覧と volume の一覧、DB のコピーの履歴、設定（読み取り。設定タブと、デプロイ・リソース・環境変数のタブの設定の節）、メモリの履歴、Compose の構成とコンテナ（9.10）、必要な環境変数の照合、操作の記録。環境変数はキーだけ（値は返さない）。アプリのリポジトリのブランチとコミットの一覧 |
| デプロイ（`deploy`） | 閲覧 + デプロイ（ブランチかコミット。コミットは、リポジトリのブランチかタグからたどれるものだけ。7 章）、ロールバック、取り消し、そのアプリへの昇格（9.7）と昇格の確認画面、起動・停止・再起動、休止中のアプリを起こす（8.4） |
| 管理（`manage`） | デプロイ + レビューアプリの作成（オーナーが親で許可したときだけ）と削除（9.9）、環境変数（値の表示と編集）、コンテナレジストリの認証の設定・変更・削除（6.1）、アプリ設定（下のオーナー専用を除く）、ドメイン（追加・再検証・プライマリ・削除。強制有効化は下のオーナー専用）、バックアップ（実行、volume のリストア、ダンプのダウンロード、DB のコピー）、タスク（コマンドと登録タスクの実行、登録タスクの編集、実行の取り消し） |

**オーナーだけの操作**

- アプリの作成と削除（レビューアプリの作成と削除は親アプリの管理。9.9）。リポジトリの一覧、作成画面の Compose 検出と環境変数の初期値（作成と、リポジトリの変更のためのもの）
- アプリ設定のうち、`environment`（本番 / staging。本番ではデプロイの前に確認が入るので、これを staging に変えると確認を外せる）、本番アプリの自動デプロイを有効にすることと、追うブランチや待つワークフローを変えること（push だけで本番に出るようになる。5.2）、`relax_policy`（ポリシー検査を緩める）、リポジトリ（`repo_full_name`、`repo_url`、`repo_id`）、Compose の置き場所と本文（`compose_source`、`compose_content`。6.3）、ほかのアプリとの組み合わせ（昇格元 `promote_from`）の変更、レビューアプリの許可（`review_apps_allowed`。9.9）。ホストを守る検査を外す、別のコードを動かす（レビューアプリの許可は、pull request のコードに親の環境変数とデータを渡す）、ほかのアプリに関わる、のいずれかで、1 つのアプリを任せる範囲を超えるため。管理のメンバーが設定の `PATCH` でこれらを今と違う値にすると `403 forbidden_role`（`details.fields` に項目名）。今と同じ値を送るのはかまわない（設定画面は全項目を送るため）
- 独自ドメインの強制有効化と、その解除（`PATCH /api/apps/{slug}/domains/{id}` の `force_active`。8.1）。DNS の検証を飛ばして、このホストを向いている裏付けのないホスト名を有効にするため。管理のメンバーが今と違う値を送ると `403 forbidden_role`（`details.fields` に `force_active`）。今と同じ値を送るのはかまわない
- システムの画面（状態 / 定期処理 / 管理のタブ。ホストのリソース、Docker のディスク、コンテナの内訳、prune、ホスト全体のバックアップの実行履歴、メンバー、GitHub App。9.1）
- メンバーとロールの管理、ほかのユーザーのセッション・ログイン履歴・API トークン

**メンバーにも見せるもの**

- `GET /api/system`（アプリ一覧のホストの状態行）と `GET /api/backups`（最終成功時刻と次回の予定）は、全ユーザーに返す。デプロイがディスク使用率で断られる、バックアップが止まっている、といったことはメンバーのアプリにも関わるため。ただしメンバーには、エラーの本文（ログ退避と Caddy の `last_error`）とバックアップの実行履歴を返さない。ほかのアプリの名前やホスト名が入りうるため。
- バックアップの「今すぐ実行」は、どれかのアプリを管理できるメンバーも使える。アプリ単位のバックアップはなく、実行はホスト全体の 1 回だが、取るだけでデータを外へ出さず、何も壊さないため。
- アプリの JSON の `promote_from`（昇格元の slug）は、昇格元のアプリにロールがなくても見える。昇格の確認画面（デプロイ以上）も昇格元のリリースの SHA とメッセージを出す。昇格は昇格元の稼働中のリリースを指すだけで、昇格元を変えないため。
- DB のコピー（10.6）は、受け側の管理に加えて、送り側の管理も要る。送り側のデータを読み出す操作で、ダンプのダウンロードと同じ権限が要るため。送り側にロールがなければ、送り側は存在しないものとして `404 source_not_found` にする。

**見えないものは存在しないものとして扱う**

- メンバーにはロールのあるアプリだけが見える。アプリの一覧（`GET /api/apps`）はロールのあるアプリだけを返し、ロールのないアプリやそのデプロイ・タスクの実行・DB のコピーを指す API には、存在しないときと同じ `404`（`app_not_found`、`deployment_not_found`、`task_run_not_found`、`db_copy_not_found`）を返す。ステータスだけでなく本文（`code` と `message`）も 1 バイトも違わないように、判定（`authorize`）とハンドラーは、対象の種類ごとに 1 つの同じ応答を使う。本文が違えば、メンバーはアプリの slug や連番の ID を順に試して、ロールのないアプリやそのデプロイがあるかどうかを知れるため。テストは、表の全ルートについて、ロールのない対象と存在しない対象への応答が一致することを確かめる。ロールが足りないときだけ `403 forbidden_role`（`details.required` に要るロール: `view` / `deploy` / `manage` / `owner`）。ロールのないアプリの存在を漏らさないため。
- 設定の `PATCH` でオーナー専用の項目を見るのは、昇格元の slug の検査より前。メンバーが検査のエラーからほかのアプリの有無を知れないようにするため。
- アプリの JSON には、リクエストしたユーザーがそのアプリでできること `role`（`owner` / `manage` / `deploy` / `view`）を入れる。画面はこれでボタンや欄を出し分ける。`GET /api/session` の `user.owner` で、オーナーだけの入口（システム、アプリの作成）を出し分ける。

**判定の仕組み**

- API のルートは 1 つの表（`internal/api` の `apiRoutes`）に、必要なもの（全ユーザー / オーナーだけ / `{slug}` のアプリのロールの段階 / `{id}` のデプロイ・タスクの実行・DB のコピーのアプリのロールの段階 / リポジトリ `{owner}/{repo}` から作ったアプリのロールの段階 / どれかのアプリのロールの段階）と一緒に登録する。表を経ずにルートを足せないので、権限を決め忘れたルートは生まれない。判定はハンドラーより前に 1 か所（`authorize`）で行う。テストは表の全ルートについて、オーナー専用がメンバーを断ること、アプリのルートがロールの段階どおりに断ることを確かめる。
- レビューアプリ（9.9）では、メンバーのロールは親アプリのロールを使う。`app_roles` を引くところ（`authorize`、一覧の絞り込み、ゲート）は、すべて store の同じ関数を通り、そこでレビューアプリを親に読み替える。
- ハンドラーの中で判定するのは、1 つのルートの中で項目ごとに権限が分かれるものだけ: 設定の `PATCH` のオーナー専用の項目、ドメインの `PATCH` の `force_active`、アプリの削除（ルートは管理で登録し、レビューアプリでないアプリはオーナーだけに絞る）、DB のコピーの送り側、環境変数の `GET` の値（管理未満には返さず `values_hidden: true`、`values_forbidden: true`）、アプリの JSON の Compose の本文（管理未満と API トークンには返さない。6.3）、一覧の絞り込み（アプリ、セッション、ログイン履歴、トークン）。
- 開いたままの応答（実行ログのライブの SSE と、デプロイ・コマンド・DB のコピーのログの SSE）は、開いたときの判定だけで読み続けさせない。15 秒ごと（SSE のハートビートの間隔）に、リクエストのときと同じ関数で、セッションか API トークンがまだ有効か（失効、期限切れ、ユーザーがログインできなくなった）と、メンバーなら `authorize` が通したアプリでそのルートの段階をまだ持つかを確かめ直す。どれかが欠けたら、`event: end` を送らずに接続を閉じる（確かめる処理が失敗したときも閉じる）。クライアントの再接続は新しいリクエストとして判定され、`401` か `404` になる。ロールを下げても、そのルートに要る段階（ログは閲覧）が残っていれば続く。ジョブのログの SSE はジョブが終われば閉じるが、コマンドは長く動かせるので、同じに扱う。
- 再ログイン（10.3）の要求は、ロールの判定のあとに、オーナーにもメンバーにも同じに掛かる。
- API トークン（10.5）のリクエストも、トークンのユーザーとして同じ表で判定する。トークンは読み取りの許可リストの `GET` しか使えない点は変わらない。
- `--insecure-no-auth` では、すべてのリクエストをオーナー（login は `local`）として扱う。

**誰がしたかの記録**

- デプロイ（ロールバックと昇格を含む）、コマンドとタスクの実行、DB のコピーは、始めたユーザーの ID と login を記録に残し（`actor_user_id`、`actor_login`）、API の `actor`（`{"id", "login"}`、koroku 自身が始めたものと古い記録は `null`）で返す。デプロイの履歴に表示する。login はその時点のものを残す（あとで login が変わっても記録は変えない）。
- 記録はリクエストの `context` にユーザーを載せて渡し、`internal/store` が記録を作るときに読む。デプロイエンジンなど途中の層の引数を変えずに済み、リクエストを経ずに koroku 自身が作る記録はユーザーなしになる。
- アプリへのそのほかの操作は、アプリごとの「操作の記録」（テーブル `app_activity`）に 1 件ずつ残す。各件は、アプリ、時刻、操作したユーザー（ID と login。koroku 自身なら `NULL`）、操作の種類（`action`）、小さな JSON の詳細（`details`）。記録するのは次の操作で、成功したときだけ（断られた操作や失敗した操作は残さない）。

| `action` | 操作 | `details` |
| --- | --- | --- |
| `app.created` | アプリの作成 | 環境、一緒に保存した環境変数のキー。レビューアプリなら親の slug と pull request の番号 |
| `review_app.created`、`review_app.deleted` | 親アプリに残す、レビューアプリの作成と削除（9.9） | レビューアプリの slug と pull request の番号 |
| `app.settings_changed` | アプリ設定の変更（オーナー専用の項目を含む） | 変わった項目ごとに項目名と前後の値。秘密として扱う項目と、Compose の本文（6.3）は項目名だけ |
| `app.started`、`app.stopped`、`app.restarted` | 起動・停止・再起動 | なし |
| `app.woken` | 休止中のアプリを管理 UI から起こした（8.4。アクセスで起きたときと、休止させたときは残さない） | なし |
| `env.changed` | 環境変数の追加・変更・名前の変更・削除・一括置換 | キーの一覧だけ（`added`、`changed`、`removed`、`renamed`、`replaced`） |
| `domain.added`、`domain.verified`、`domain.primary`、`domain.forced`、`domain.deleted` | ドメインの追加・再検証（ボタン）・プライマリ指定・強制有効化・削除 | ホスト名と結果の状態や指定の有無 |
| `registry.set`、`registry.removed` | コンテナレジストリの認証の保存・削除 | レジストリとユーザー名（変えたときは前の値）、新しいトークンを保存したか。トークンは残さない |
| `backup.restored`、`backup.downloaded` | volume のリストア、DB ダンプのダウンロード | スナップショットの ID |
| `db_copy.started`、`db_copy.sent` | DB のコピーの開始（受け側と送り側の両方に残す） | 受け側には送り側の slug と取り元、送り側には取り元だけ |
| `task_run.started`、`tasks.changed` | コマンドとタスクの実行、登録タスクの変更 | 実行の ID・タスク名・サービス・コマンド、変わったタスクの名前 |
| `member.role_set`、`member.role_removed` | そのアプリでのメンバーのロールの変更（メンバーの削除で外れたロールを含む） | メンバーの ID と login、前後のロール |
| `auto_deploy.skipped` | 待つワークフローが成功しなかったので自動デプロイしなかった（koroku 自身の記録。5.2） | ワークフローのパス、結論、コミット、ブランチ、実行の URL |
| `auto_deploy.dropped` | コマンドの実行中に預かった自動デプロイを、実行のあとで積めなかった（koroku 自身の記録。5.2） | コミット、ブランチ、push した人、待ったワークフロー、理由（`disk_full`、`auto_deploy_disabled`、`stopped`、`branch_changed`、`workflow_changed`） |

  - **環境変数の値は記録しない**。変更かどうかはメモリの上で復号した値を比べて決め、残すのはキーだけ。同じ値で保存し直したものは残さない。アプリ設定は値を残すが、秘密として扱う項目（API の項目の一覧に印を付ける）は「変わった」ことだけを残す。コマンドの文字列は 9.6 と同じく秘密として扱わない。
  - 送り側の `db_copy.sent` は受け側の slug を書かない。送り側のメンバーが受け側にロールを持つとは限らず、見えないアプリの名前を漏らさないため。
  - デプロイ（ロールバックと昇格を含む）は、`deployments` が始めたユーザーを持ちデプロイの履歴に出るので、操作の記録には重ねない。5 分ごとのドメインの自動の再検証も残さない（`pending` のものを数日にわたって試し続け、記録が溢れるため）。
  - 保持: アプリごとに新しい 500 件。記録を足すときに、それより古いものを同じトランザクションで消す。アプリの削除で消える（`ON DELETE CASCADE`）。日数ではなく件数で区切るのは、操作の少ないアプリでも直近の経緯を残し、操作の多いアプリでも大きさが決まるため。
  - 記録の書き込みに失敗しても、操作そのものは済んでいるので、応答はエラーにせずサーバーログに残す。
  - API は `GET /api/apps/{slug}/activity`（新しい順、`limit` と `before` でページ送り）で、閲覧以上のロールで読める。画面は概要タブの横の列に「操作の記録」として最新 5 件を出し、「さらに表示」でさかのぼる。誰が（login、koroku 自身なら「koroku」）、何を（日本語の文）、いつ（相対時刻、ホバーで日時）を出す。設定タブにしないのは、閲覧のメンバーにも見せたいもので、タブを増やさずに済むため。
  - コマンドとタスクの実行と DB のコピーは、それぞれの履歴（タスクタブ、バックアップタブ）にも「実行: <login>」を出す。
- `koroku` のリクエストログ（journald）の各行にも `user`（login）を付ける。

**データ**

| テーブル | 主なカラム |
| --- | --- |
| `members` | `github_user_id`（主キー）、`login`、`avatar_url`、`added_at` |
| `app_roles` | `app_id`（アプリの削除で消える）、`github_user_id`（メンバーの削除で消える）、`role`（`view` / `deploy` / `manage`）。主キーは（`app_id`、`github_user_id`） |
| `api_tokens` に追加 | `github_user_id`、`login`（トークンのユーザー） |
| `deployments`、`task_runs`、`db_copies` に追加 | `actor_user_id`、`actor_login` |
| `app_activity` | `app_id`（アプリの削除で消える）、`created_at`、`actor_user_id`（koroku 自身なら `NULL`）、`actor_login`、`action`、`details`（JSON） |

**採らない方式**

- メンバーにもアプリの作成を許す: アプリの作成はリポジトリの選択（GitHub App のインストール先すべてが見える）とホストのポート・ディスクの割り当てを伴い、ホストの持ち主の判断になるため。
- ロールを操作ごとの権限の組み合わせにする: 3〜5 アプリと数人の規模では、3 段階で足りる。画面と判定が単純なほうが誤りが少ない。
- メンバーのロールを GitHub のリポジトリの権限から決める: リポジトリに書ける人とホストでコードを動かしてよい人は同じではなく、GitHub への問い合わせがリクエストごとに要る。

### 10.8 ダンプの間隔とダンプからの復旧

日次のダンプ（10.2）だけでは、誤った操作や壊れたデータに気づいたときに最大で 1 日分を失い、戻すにもダンプをダウンロードして手で流し込むしかない。そこで、ダンプを取る間隔をアプリごとに短くでき、選んだダンプへ koroku の中で戻せるようにする。DB の種類は知らないまま、ダンプフックとロードフック（10.6）だけで動かす。秒単位で戻す仕組みは PostgreSQL に限って 10.9 に置く。

**ダンプの間隔**

- アプリ設定 `dump_interval_hours` は `24`（既定）、`12`、`8`、`6`、`4`、`3`、`2`、`1` のどれか。24 を割り切る値に限り、取る時刻を日次の実行（03:00 JST）から数えて決める（`1` なら毎時 0 分、`6` なら 03:00、09:00、15:00、21:00）。時刻が揃うので、どの時点のダンプが残るかを利用者が予想できる。
- 03:00 の実行は、10.2 のとおり基盤、volume、全アプリのダンプを取る。それ以外の時刻には、その時刻にあたるアプリのダンプだけを取る（間隔のダンプ）。volume は日次のまま。アップロードファイルのような volume は、DB ほど頻繁に戻したくならず、取るたびにディスク全体を読むため。
- 間隔のダンプも 10.2 と同じ restic の操作のロック（ホスト全体で同時に 1 つ）を取る。時刻にロックが埋まっていれば、空いたときに 1 回だけ取る。待つ間に次の時刻が来ても重ねない（遅れたダンプを何本も続けて取ると、本番の DB に負荷が続くため）。
- 停止中のアプリでは取らない（10.2 と同じ）。休止中（8.4）のアプリでも、間隔のダンプは取らない。休止の間は DB が変わらず、取るたびに DB を起こすと休止の意味が薄れるため。日次のダンプは、10.2 のとおりフックのサービスだけを起こして取る。
- 記録は `backup_runs` に `trigger = interval` として残す。アプリのバックアップの状況（10.2 の「結果の記録はアプリ単位」）は、日次・手動・デプロイ前の直近の実行に、それより新しい間隔のダンプの結果を対象ごとに重ねて出す。毎時のダンプの実行が「直近の実行」になって、volume の結果が見えなくなるのを防ぐため。最終成功時刻と 24 時間の警告は、間隔のダンプの実行を数えない（volume を取っていないため）。システム画面の実行の履歴にも出さない（1 時間に 1 つ増え、日次の実行が押し出されるため。結果はアプリの画面に出る）。リポジトリを開けずに失敗した間隔のダンプの実行は、取るはずだったアプリのダンプの失敗として記録する。全アプリに関わる失敗として扱うと、ダンプを取らないアプリにまで失敗が出るため。加えて、ダンプが間隔の 2 倍の時間成功していなければ、そのアプリのダンプを警告にする。
- ダンプにかかった時間を記録し、間隔の半分を超えたら、そのアプリの設定画面に警告を出す（本番の DB に、ダンプの負荷がかかり続ける状態になっているため）。
- 世代は、直近 48 時間のダンプをすべて残し（`restic forget` に `--keep-within 48h` を足す。`--keep-within-hourly` は 1 時間に 1 つしか残さず、戻す前に取ったダンプが同じ時間の間隔のダンプを押し出すため）、それより前は日次 7・週次 4・月次 6 のまま。誤りに気づくのは多くが 2 日以内で、それより前は 1 日の刻みで足りる。`forget` は日次の実行のときだけ行うので、間に取ったダンプは翌朝まで残る。
- 間隔を 24 時間より短くするアプリの PostgreSQL のダンプフックは、`pg_dump -Fc -Z0`（圧縮しないカスタム形式）を勧める（画面のプリセットの説明に書く）。`pg_dump -Fc` の既定の圧縮をかけた出力は、データが少し変わっただけでもほぼ全体が別のバイト列になり、restic の重複排除が効かない。圧縮しない出力なら、変わらない部分は重複として省かれ、残りは restic のリポジトリ側の圧縮で縮む。ロードフック（`pg_restore`）はどちらの形式も読む。

**ダンプからの復旧**

アプリ自身のダンプのスナップショットを選び、そのアプリの DB へ流し込む。10.6 の DB のコピーの仕組みのうち、送り側と受け側が同じアプリで、取り元がスナップショットのものとして実装する（記録も `db_copies`）。API は `POST /api/apps/{slug}/db-restores`（本文は `snapshot_id` と、本番アプリでは `confirm`）。選べるのは、いまのダンプフックのサービスで取ったダンプだけ（別のサービスのダンプを、いまのロードフックで読ませないため）。

1. `check`: アプリにロードフックがあり、そのサービスが動いていること（休止中なら起こす）。選んだスナップショットが、このアプリの、いまのダンプフックのサービスのダンプであること。
2. `backup`: いまの DB をダンプフックで取り、restic に残す（10.6 の `backup` と同じ）。**取れなければ中止する**。10.6 と違い、バックアップが未設定でもダンプフックがなくても飛ばさない。戻す操作は本番にも使い、いまのデータを失わない手段がこれだけのため。
3. `stop`: ロードフックのサービス以外を止める（10.6 の `stop`）。
4. `load`: 選んだスナップショットを `restic dump` で読み、ロードフックへ流す（10.6 の `copy` の `snapshot`）。コピー後のコマンドは走らせない。自分のデータを戻す操作で、個人情報を消す理由がないため。
5. `start`: 止めたサービスを起動する。
6. `done`（失敗時は `failed`）。

- 4 で失敗したら、2 のダンプを同じロードフックで流し込み直してから 5 に進む（ログに `reload` の見出しを出す）。戻し直しにも失敗したら、サービスは止めたままにして、ログにその旨と 2 のスナップショットの ID を書く。途中まで流し込んだ DB で本番を公開しないため。
- 安全策は 10.6 の受け側の扱いをそのまま使う（`manage`、再ログインから 10 分以内、操作のロック、2 時間・20 GiB の打ち切り、ログの伏せ字）。本番アプリでは、本文の `confirm` に slug を求める（アプリの削除と同じ）。staging だけという受け側の制限は、送り側と受け側が同じアプリのときには掛けない。
- 記録: `db_copies` に `kind`（`copy` / `restore`）と、取り元のスナップショットの ID（`source_snapshot_id`）を足す。`restore` では `source_app_id` は受け側と同じ。操作の記録（10.7）は `db.restored`（スナップショットの ID と時刻）。
- 画面: バックアップタブのダンプのスナップショットの行に「この時点に戻す」を置く（ダウンロードの隣。ロードフックとダンプフックがあり、いまのダンプフックのサービスのダンプのときだけ）。ダイアログには、戻す先の時刻、2 で今のデータを残すこと、止まるサービスを出す。戻した記録とログは、同じタブの「ダンプから戻す」の欄に並べ、DB のコピーの履歴には混ぜない（コピーの欄は staging アプリにしか出ず、本番アプリの復旧の行き場がないため）。本番アプリのロードフックは、この欄で設定する（staging アプリはコピーの設定で設定する）。
- 10.2 の「DB ダンプは UI からダウンロードするところまで」は、ロードフックのないアプリでの扱いとして残る。

**採らない方式**

- ダンプのたびに volume も取る: DB のデータディレクトリの volume は稼働中にコピーしても整合しない（10.2）。それ以外の volume は、DB ほど頻繁に戻す要求がない。
- 間隔を分単位まで細かくする: ダンプは毎回 DB 全体を読む。それ以上の細かさは、変わった分だけを送る仕組み（10.9）でないと、本番の DB の負荷とストレージが釣り合わない。

### 10.9 PostgreSQL の時点指定の復旧

PostgreSQL の DB を、指定した時刻（秒の単位）の状態に戻す。PostgreSQL の WAL アーカイブと、ベースバックアップからの回復（[Continuous Archiving and Point-in-Time Recovery](https://www.postgresql.org/docs/current/continuous-archiving.html)）を使う。koroku が DB の種類を知って動く唯一の機能で、PostgreSQL のサービスにだけ使える。MySQL と MariaDB の binlog は対象にしない。利用の多い構成（Rails + PostgreSQL）に絞り、DB ごとの回復の手順を koroku に重ねて持たないため。それらのアプリは 10.8 の間隔のダンプを使う。

**有効にする条件**

- アプリ設定 `pitr_service` に PostgreSQL のサービス名を入れる（空なら無効）。本番アプリで今と違う値にするには再ログインが要る（ダンプフックと同じ。アプリのコンテナで動くコマンドを決める設定のため）。
- サービスのイメージが、公式の `postgres` イメージと同じ道具（`psql`、`pg_basebackup`、`tar`、`cmp`）を持ち、データディレクトリ（`SHOW data_directory`）がそのアプリの named volume の中にあること。bind mount のデータディレクトリは断る（Compose の volume としてしか入れ替えの手順を持たないため）。データディレクトリは volume の根とは限らない。公式イメージは 17 までは volume の根（`/var/lib/postgresql/data`）がデータディレクトリで、18 からは volume（`/var/lib/postgresql`）の下の `18/docker` がデータディレクトリになる。koroku は準備のときに `SHOW data_directory` を記録し、復旧ではそのディレクトリだけを入れ替える。
- PostgreSQL 14 以上。`recovery.signal` での回復と、目標の時刻に届かずに回復が終わったときにサーバーがエラーで止まる振る舞いを前提にする。
- 接続は、サービスの中から `docker compose exec -T` でローカルのソケットを使い、`POSTGRES_USER` のユーザーで行う。ベースバックアップには `replication` の接続を許す `pg_hba.conf` の行が要る。公式イメージの既定（`initdb` が作る `local replication all trust`）で通ることを、14、17、18 で確かめた。満たさないときは、準備（下の「有効にする流れ」）で理由を出して止める。
- ファイルの操作（WAL の取り出しと受け渡し、データディレクトリの入れ替え、所有者の設定）は、ホストのパスではなく、DB のサービスのイメージのコンテナの中で行う（稼働中は `docker compose exec`、止めているときは稼働中のリリースのファイルで `docker compose run --rm --no-deps -T --entrypoint sh`）。`postgres` ユーザーの uid はイメージで違い（Alpine は 70、Debian は 999）、データディレクトリのパスもコンテナの中のものなので、同じイメージの中で操作すれば koroku がそれを知らずに済む。

**WAL の受け渡し**

- override が、`pitr_service` のサービスに、ホストのディレクトリ `/var/lib/koroku/apps/<slug>/wal/<service>/` を `/koroku/wal` としてマウントする。ディレクトリは 0700 で、所有者はサービスの中の `postgres` ユーザーの uid（`exec` で調べる）。WAL は DB の中身そのものなので、ほかのユーザーから読めないようにする。
- PostgreSQL の設定は、koroku が `ALTER SYSTEM` で書く（`archive_mode = on`、`archive_command`、`archive_timeout = 300`）。値はデータディレクトリの `postgresql.auto.conf` に残るので、リポジトリの `command` を書き換えずに済み、コンテナを作り直しても消えない。`archive_mode` の変更は DB の再起動を要するので、koroku が DB のサービスを 1 回だけ再起動する（数秒、DB への接続が切れる）。
- `archive_command` は、マウントしたディレクトリへの一時ファイルへのコピーと rename だけをする（`test -d /koroku/wal || exit 0; f=/koroku/wal/%f; if [ -f "$f" ]; then cmp -s %p "$f"; else cp %p "$f.tmp" && mv "$f.tmp" "$f"; fi`）。同じ名前のファイルがあるときは、中身が同じなら成功、違えば失敗にする。**マウントがないときは何もせずに成功を返す**。マウントを外したコンテナで WAL を切り替え続けても、アーカイブの失敗は数えられず、`pg_wal` は増えないことを確かめた。ロールバックで有効にする前のリリースに戻ったときのようにマウントが外れていると、失敗を返す `archive_command` では PostgreSQL が WAL を消さずに溜め続け、ディスクが埋まれば DB が止まる。DB を止めるより、時点指定の復旧ができない区間を作るほうを選ぶ。koroku は稼働中のリリースの override にマウントがあるかを知っているので、外れている間を「戻せない区間」として記録する。
- `archive_timeout = 300` で、書き込みが少なくても 5 分ごとに WAL のファイルが切り替わり、アーカイブに出る。

**有効にする流れ**

1. `pitr_service` を保存する。マウントは override で付くので、効くのは次のデプロイから（設定タブに「次のデプロイから」と出す。メモリの上限と同じ）。
2. マウントの付いたリリースのデプロイが成功したら、koroku がそのアプリの操作として「時点指定の復旧の準備」を始める（デプロイの手順には入れない。DB の再起動でデプロイを長くせず、失敗してもデプロイを失敗にしないため）。条件（上記）を確かめ、`ALTER SYSTEM` を書き、`archive_mode` が変わるなら DB のサービスを再起動し、ベースバックアップを取る。
3. 2 が終わるまで、`GET /api/apps/{slug}/pitr` は「準備中」、失敗したら理由（`replication` の接続が拒まれた、データディレクトリが named volume にない、など）を返す。設定は残すので、理由を直してデプロイし直せば、もう一度 2 を行う。
4. 操作の記録（10.7）に、準備の開始と結果を残す。DB の再起動は本番アプリにも起きるので、有効にするときのダイアログで「次のデプロイの後に、DB が数秒止まる」ことを伝える。

**取るもの**

| 対象 | 時機 | 方法 |
| --- | --- | --- |
| ベースバックアップ | 日次の実行（03:00）、有効にした直後、復旧の直後、メジャーバージョンが変わった後 | `docker compose exec -T <service> pg_basebackup -D - -Ft -X none --checkpoint=fast --no-manifest` の出力を `restic backup --stdin`（タグ `kind:pitr-base`、`target:<service>`）。WAL は含めず、回復ではアーカイブから読む。`--no-manifest` は、14 の `pg_basebackup` が標準出力の tar のあとにマニフェストを続けて書き、tar として読めなくなるため |
| WAL | 1 時間ごと（毎時 0 分。10.8 の間隔のダンプと同じロックを取る） | マウントしたディレクトリの、書き終えたファイル（一時ファイルでないもの）の一覧を決め、`docker compose exec -T <service> tar -c` でまとめて `restic backup --stdin`（タグ `kind:wal`、`target:<service>`）で取ってから、そのファイルを消す。ファイルは `postgres` ユーザーのもの（0600）なので、コンテナの中から読む |

- ベースバックアップごとに、開始と終了の時刻、開始の WAL の位置、タイムライン、`server_version_num` を `pitr_bases` に記録する。WAL のスナップショットごとに、含めたファイル名の最初と最後、最後のファイルの更新時刻を `pitr_wal_batches` に記録する。回復でどのスナップショットを読めばよいかを、restic の一覧を毎回たどらずに決めるため。戻せない区間は、開始と終了の時刻と理由（マウントの外れ、ホストの中の上限で捨てた、メジャーバージョンの変更）を `pitr_gaps` に記録する。
- ホストの中に WAL を溜める上限を持つ。退避に失敗し続けてディレクトリが 2 GiB を超えたとき、またはデータディスクの使用率が 90% 以上（10.1 の「ログの量の上限」と同じしきい値）のときは、古いファイルから捨てて「戻せない区間」に記録し、次の日次の実行を待たずにベースバックアップを取り直す。
- 1 時間ごとの退避のときに、`pg_stat_archiver` の `failed_count` と `last_failed_time` を読み、増えていれば警告にする。
- 休止中（8.4）のアプリでは WAL が出ないので、退避するものはない。日次のベースバックアップは、ダンプフックと同じく DB のサービスだけを起こして取る。休止が長く続いても、戻せる期間の起点のベースバックアップを失わないため。
- 1 時間ごとの WAL のスナップショットは、7 日の期間で 1 アプリあたり 200 ほどになる。小さなスナップショット 200 個のリポジトリで、`restic snapshots` と `restic forget --keep-within` は 1 秒かからないことを確かめた。
- 世代は、`kind:pitr-base` と `kind:wal` を、ほかのスナップショットとは別の `restic forget --tag ... --group-by tags --keep-within 8d` で管理する。戻せる期間は 7 日で、その起点より前のベースバックアップが 1 つ残るように 1 日足す。それより前は、10.2 と 10.8 のダンプで戻す。10.2 の `forget` には `--keep-tag kind:pitr-base --keep-tag kind:wal` を足して、この 2 つを消させない。足さないと、1 時間ごとの WAL のスナップショットが日次の世代として 1 日 1 つに間引かれ、WAL がつながらなくなる。
- メジャーバージョンを上げたデプロイ（`server_version_num` の上 2 桁が、直近のベースバックアップと違う）の後は、すぐにベースバックアップを取り、それより前の時刻は「このバージョンでは戻せない」として画面に出す。古いバージョンのベースバックアップは、新しいバージョンのサーバーでは回復できないため。

**戻せる期間の表示**

- `GET /api/apps/{slug}/pitr`（閲覧以上）: 有効かどうか、戻せる最も古い時刻（残っている最も古いベースバックアップの終了時刻）、退避済みの最新の時刻（`pitr_wal_batches` の最後のファイルの更新時刻）、戻せない区間の一覧、警告（アーカイブの失敗、マウントの外れ、ホストの中の WAL の上限）。
- ホストの中に残っている、退避前の WAL と、稼働中の DB の `pg_wal` の分は、現在まで戻せる。ホストを失ったときに失うのは、最大で退避の間隔（1 時間）と `archive_timeout`（5 分）の分。
- 画面: バックアップタブに「時点を指定して戻す」の欄を置き、戻せる期間を帯で出し、戻せない区間を塗り分ける。時刻は JST で入れる。

**復旧の手順**（`POST /api/apps/{slug}/pitr/restores`、本文 `{"target_time": "...", "confirm": "<slug>"}`。ログの見出しは 10.6 と同じ形）

1. `check`: `target_time` が戻せる期間の中で、戻せない区間に入らず、その前の最も新しいベースバックアップから `target_time` まで WAL がつながっていること。ベースバックアップの `server_version_num` のメジャーバージョンが、稼働中のリリースのイメージと同じであること。
2. `stop`: アプリ全体を止める（`docker compose stop`）。
3. `move`: データディレクトリの中身を restore-trash（`/var/lib/koroku/restore-trash/<slug>-pitr-<時刻>/`。7 日保持）へ移す。DB のサービスのイメージのコンテナに、データの volume と restore-trash の下のディレクトリをマウントして行う。別のマウントの間の移動なので、DB の大きさに比例して時間がかかる。
4. `base`: ベースバックアップを `restic dump` で読み、同じ形のコンテナの `tar -x` でデータディレクトリに展開する（所有者は `postgres`、データディレクトリは 0700）。
5. `wal`: 必要な WAL のスナップショットを `restic dump` で読み、マウントしたディレクトリの下の `restore/` に展開する。まだ退避していないホストの中の WAL と、3 で移した古いデータディレクトリの `pg_wal` にあって、アーカイブにないファイルも加える。誤った操作の直前の数分まで戻せるようにするため。
6. `recover`: データディレクトリに `recovery.signal` を置き、`postgresql.auto.conf` に `restore_command`（`restore/` からのコピー）、`recovery_target_time`、`recovery_target_action = 'promote'` を書く。DB のサービスだけを稼働中のリリースのファイルで起動し、`pg_is_in_recovery()` が偽になるまで待つ。目標の時刻に届かずに WAL が尽きると、PostgreSQL がエラーで止まるので、失敗にする。
7. `cleanup`: 回復の設定を `ALTER SYSTEM RESET` で消して設定を読み直させ、`restore/` を消す。
8. `start`: アプリ全体を起動する。
9. `base`: 新しいタイムラインのベースバックアップを取る。失敗しても復旧は成功として扱い、警告を出す（次の日次の実行でも取る）。
10. `done`（失敗時は `failed`）。

- 4 から 7 で失敗したら、DB のサービスを止め、展開したデータを消し、3 で移した中身を戻してから、アプリを起動する（volume のリストアの失敗と同じ扱い）。もともと止まっていたアプリは止めたままにする。
- 安全策: `manage`、再ログインから 10 分以内、本番アプリでは `confirm` の slug、操作のロックと restic の操作のロックの両方を取る（デプロイや別の復旧と重ねない）。全体で 2 時間で打ち切る。
- 記録: `pitr_restores`（`id`、`app_id`、`target_time`、使ったベースバックアップのスナップショットの ID、restore-trash のパス、`status`、`error`、`actor`、開始と終了の時刻）。ログは `/var/lib/koroku/apps/<slug>/pitr-restores/<id>.log`。`koroku` の再起動で中断した復旧は、起動時に `failed` にし、アプリは止めたままにする（ログの末尾に、restore-trash のパスと戻し方を書く）。操作の記録（10.7）は `db.pitr_restored`。

**ほかの機能との関係**

- blue/green（7.2）: DB のサービスは色を持たないので、そのまま使える。環境変数を変えたデプロイで DB のコンテナが作り直されても、マウントは override が付け直し、設定は volume に残る。
- ロールバック（7.1）で、有効にする前のリリースに戻ると、マウントが外れる。上の「マウントがないときは成功を返す」で DB は止まらず、外れている間が戻せない区間になる。
- 無効にする: `pitr_service` を空にすると、koroku がまず `ALTER SYSTEM RESET` で `archive_mode` などを消して DB を再起動し、その後のデプロイからマウントを外す。順を逆にすると、マウントのない間に WAL が捨てられ続けるだけになる。ホストの中のディレクトリは消し、restic のスナップショットは世代管理が消すまで残す。
- レビューアプリ（9.9）は設定を写さない（バックアップの対象でないため。10.2）。昇格（9.7）でも写さない。DB のコピー（10.6）と 10.8 の復旧は、論理的なダンプでそのまま使える。
- アプリの削除で、ホストの中のディレクトリは消える。restic のスナップショットは 10.2 の「削除したアプリのスナップショット」と同じく残る。

**採らない方式**

- WAL-G や pgBackRest をアプリの Compose に入れる: オブジェクトストレージのアクセスキーをアプリに渡すことになり、そのキーで全アプリのログ（10.1。暗号化していない）が読める。koroku が退避を受け持てば、キーはホストの外に出ない。
- koroku が WAL を自分で暗号化してオブジェクトストレージへ置く: 暗号化、一覧、世代管理を restic と重ねて持つことになる。
- 別のアプリ（staging）に、指定した時刻の状態を作る: 物理的なバックアップにはロールとパスワードまで入っており、受け側の環境変数の接続情報と合わない。確かめてから本番に戻したいときは、本番で戻したうえで、restore-trash の中身で元に戻す（`docs/OPERATIONS.md`）か、戻す前に 10.8 のダンプを取っておく。
- `archive_command` を失敗させてアーカイブの欠けを防ぐ: 上の「マウントがないとき」のとおり、DB がディスクを埋めて止まるほうが被害が大きい。

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
