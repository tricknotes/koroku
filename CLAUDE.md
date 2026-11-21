# koroku — Claude Code 作業ガイド

個人用の Heroku ライクなホスティング基盤を実装するリポジトリ。OSS として公開しており、特定のホスティング事業者・DNS サービス・オブジェクトストレージに依存しない。VPS 1 台の上で、GitHub リポジトリの Docker Compose アプリ（3〜5 個）を UI からデプロイする。

## 参照する資料

- `docs/ARCHITECTURE.md` が設計の正。実装前に必ず全体を読むこと。
- マイルストーンと完了条件、受け入れシナリオはこのファイルにだけ書く。
- それ以外で両者が食い違ったら ARCHITECTURE.md に従う。
- 設計を変える必要が出たら、実装より先に ARCHITECTURE.md を新しい設計に書き換える。黙って設計から外れない。
- ARCHITECTURE.md は現在の設計だけを書くストック情報。日付、「当初は」「変更した」のような経緯、マイルストーン、実機確認の記録は書かない。確かめて分かった事実は、該当する章に現在の振る舞いとその理由として書く。変更の理由と経緯はコミットメッセージと PR に書く。
- 利用者向けの説明は `README.md`、外部サービスの設定は `docs/SETUP.md`、ホストの構築と運用は `docs/OPERATIONS.md`、開発の手順は `docs/DEVELOPMENT.md`。

## 確定している前提

| 項目 | 内容 |
| --- | --- |
| 実行基盤 | VPS 1 台（事業者は問わない）、Ubuntu 24.04 か 26.04、x86_64 |
| ベースドメイン | 利用者が `config.toml` の `base_domain` で決める。文書と例では `dev.example.com`（アプリは `<slug>.dev.example.com`、管理 UI は `console.dev.example.com`） |
| コントロールプレーン | Go 製シングルバイナリ `koroku`（systemd、ホスト直置き）+ SQLite |
| フロントエンド | Ember + TypeScript の SPA。ビルド成果物を `koroku` に embed |
| リバースプロキシ | Caddy（ホスト直置き）。設定は DB から全体を生成して admin API の `POST /load` |
| GitHub 連携 | GitHub App（ログイン + installation token で clone） |
| 利用者 | オーナー 1 人（`[github] allowed_user_id`）と、オーナーが管理 UI で加えたメンバー。メンバーはアプリごとに閲覧 / デプロイ / 管理のロールを 1 つ持ち、ロールのあるアプリだけを扱う。アプリの作成・削除、システム画面、メンバーの管理はオーナーだけ（ARCHITECTURE 10.7） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p koroku-<slug>`）。staging と本番は別アプリ |
| 外部ストレージ | S3 互換のオブジェクトストレージ（S3 互換 API のみ、パス形式の URL で使う。事業者固有の API は使わない） |
| バックアップ | restic → オブジェクトストレージ。日次 03:00 JST |
| ログ退避 | 4 時間ごとにオブジェクトストレージへ |

## リポジトリ構成

```text
cmd/koroku/           エントリポイント（serve、restore-system などのサブコマンド）
internal/api/         HTTP ハンドラ、セッション、SSE
internal/deploy/      デプロイの状態機械とワーカー
internal/compose/     override 生成、config 正規化、ポリシー検査
internal/caddy/       Caddy JSON 設定の生成と投入
internal/github/      GitHub App 認証、API クライアント
internal/domains/     独自ドメインの検証
internal/logs/        ログコレクタとオブジェクトストレージへの退避
internal/backup/      restic 呼び出し、スケジュール、リストア
internal/store/       SQLite、マイグレーション
internal/runner/      外部コマンド実行の抽象
web/                  Ember アプリ
deploy/               プロビジョニングスクリプト、systemd unit
docs/                 設計資料
testdata/             サンプル Compose、golden file
.claude-plugin/       Claude Code のマーケットプレイス定義
plugins/koroku/       アプリのリポジトリを koroku 向けに整える Skill（利用者に配布）
```

## 実装規約

- Go は標準ライブラリ中心（`net/http`、`database/sql`）。Web フレームワークや ORM は入れない。SQLite ドライバは cgo 不要の `modernc.org/sqlite`。
- 依存を追加するときは、標準ライブラリで足りない理由をコミットメッセージに書く。
- 外部コマンド（`docker`、`git`、`restic`）は必ず `internal/runner` の `Runner` インターフェース経由で呼ぶ。引数は配列で渡し、シェル文字列を組み立てない。
- GitHub、Caddy、オブジェクトストレージ、時計はインターフェースにして、テストでフェイクに差し替えられるようにする。
- 環境変数の値、トークン、鍵をログ・エラーメッセージ・デプロイログに出さない。出力前にマスクする。
- 外部呼び出しには `context` でタイムアウトを付ける。エラーは文脈を付けて wrap する。
- DB が唯一の正。Caddy 設定や override は常に DB から再生成できること。
- API のエンドポイントは `internal/api/server.go` の `apiRoutes` の表に、必要な権限（全ユーザー / オーナーだけ / アプリのロールの段階）と一緒に登録する。ハンドラーの中で権限を判定するのは、1 つのルートの中で項目ごとに権限が分かれるもの（アプリ設定のオーナー専用の項目、DB のコピー元など）だけにする（ARCHITECTURE 10.7）。
- UI の文言は日本語。コード、コメント、コミットメッセージは英語。
- 文書、コード、コメント、UI に特定の事業者（VPS、DNS、オブジェクトストレージ）の名前や、運用者個人のドメイン・IP を書かない。条件で書き、例には `example.com` と `203.0.113.x` を使う。
- デプロイされる側に見える振る舞い（ポリシー検査、override の生成、デプロイ手順、ヘルスチェック、env ファイル、バックアップ）を変えたら、`plugins/koroku/skills/prepare-app/`（`references/requirements.md` と `scripts/rehearse.sh`）も同じ PR で直し、`plugin.json` の `version` を上げる。利用者のリポジトリはこの Skill を頼りに koroku へ合わせるため。

## テストと確認コマンド

```sh
bin/dev go vet ./...
bin/dev go test ./...                       # ユニット
bin/dev go test -tags integration ./...     # 実 Docker・restic を使う結合テスト
bin/dev sh -c 'cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm test'    # フロントエンド
```

- 開発の道具（Go、Node.js、pnpm、Chromium、restic など）は `dev/Dockerfile` のイメージに入っている。ホストには何も入れず、コマンドは `bin/dev` 経由で実行する。
- override 生成、Caddy 設定生成、ポリシー検査は golden file で固定する。
- マイルストーンを終えるときは、下の完了条件を実際に実行して確かめ、結果（コマンドと出力の要点）を報告する。確認していないものを完了と書かない。

## 画面を変える PR

管理 UI の見た目や画面構成を変える PR には、変更後のスクリーンショットを添付する。

- 撮り方: `bin/dev sh -c 'cd web && pnpm build && node mock/screenshots.mjs <名前の接頭辞>'`。`web/tmp/shots/`（git の管理外）に出力される。新しい画面や状態を足したら、`web/mock/screenshots.mjs` とモックのシナリオにも足す。
- 添付するもの: 変えた画面のライトとダーク、390px 幅、変えた状態（空、エラー、警告、ダイアログなど）。自分で開いて崩れがないことを確かめてから添付する。
- 添付の方法: `gh` の `--attach` を使う。本文のファイルに `![説明](./list-healthy-light.png)` のように相対パスで書き、同じファイルを `--attach` で渡すと、アップロード先の URL に書き換わる（本文のファイルは画像と同じディレクトリに置く）。

  ```sh
  gh pr create --title "..." --body-file web/tmp/shots/body.md \
    --attach web/tmp/shots/list-healthy-light.png \
    --attach web/tmp/shots/list-healthy-dark.png
  ```

  既存の PR には `gh pr edit <番号> --body-file ... --attach ...`（本文ごと置き換える）か、`gh pr comment <番号> --body-file ... --attach ...` を使う。画像は 1 枚 10MB まで、1 コマンド 50 枚まで。
- public リポジトリなので、添付した画像は誰でも見られる。モック API（`web/mock/`）のデータで撮ったものだけを添付し、手元の環境や実データが写ったものは添付しない。

## マイルストーン

M0 から順に進める。各マイルストーンの着手時に作業計画を示し、完了時に完了条件の確認結果を報告する。

### M0 骨格

- [x] リポジトリ構成、`config.toml` 読込、SQLite マイグレーション（埋め込み SQL を起動時に適用）
- [x] Ember の空アプリを embed して配信
- [x] CI（vet、ユニットテスト、フロントの lint とテスト）
- 完了条件: `koroku serve` が起動し、`GET /api/system` が応答する

### M1 デプロイエンジン

- [x] public リポジトリの URL と SHA を API で受け、取得 → env ファイルと override 生成 → `config` で検証 → build / pull → `up --wait` → HTTP 確認
- [x] ポリシー検査（ARCHITECTURE 6 章）と、アプリ単位の「検査を緩める」フラグ
- [x] サービスごとのメモリ上限を override で付与（既定値あり、アプリ設定で変更可）
- [x] ディスク使用率 85% 超でデプロイ拒否
- [x] 状態機械（queued / running / succeeded / failed / canceled）、同時実行 1、起動時の中断ジョブ処理
- [x] 手順 6 以降の失敗で直前の成功リリースへ自動ロールバック
- [x] デプロイログのファイル出力とマスク
- [x] 実機確認: `--env-file` 指定時にリポジトリ内の `.env` がどう扱われるか。結果を ARCHITECTURE の 6 章に反映
- 完了条件: サンプル 2 種（`build:` あり / `image:` のみ）が API 経由で起動し、起動に失敗するコミットでは旧版が応答し続ける

### M2 ルーティングとドメイン

- [x] ホストポート割当（20000 番台、DB で一意）
- [x] Caddy JSON 設定の生成と `POST /load`。`koroku` 起動時にも実行
- [x] 自動割当ドメインの TLS（開発中は Let's Encrypt staging）
- [x] `app_domains` テーブル、独自ドメインの追加・DNS 検証（`public_ip` と照合、5 分間隔で再試行）・強制有効化
- [x] プライマリドメインへの 308 リダイレクト
- [x] 停止中アプリは 503 の固定ページ
- [x] Caddy のアクセスログをアプリ別に出力（M5 のコレクタが拾う）
- [x] 実機確認: 設定再投入時に他アプリの接続（WebSocket、SSE）が切れないか。結果を ARCHITECTURE の 8 章に反映
- 完了条件: `https://<slug>.dev.example.com` が応答し、独自ドメインを追加して DNS を向けると HTTPS で応答する

### M3 GitHub App

- [x] Web application flow でのログイン、許可ユーザー ID との照合、セッション Cookie
- [x] 変更系 API の `Origin` 検証
- [x] リポジトリ / ブランチ / コミット一覧
- [x] installation token による private リポジトリの取得（トークンを `.git/config` とログに残さない）
- 完了条件: 許可 ID 以外はログインできない。private リポジトリをデプロイできる

### M4 UI

- [x] アプリ一覧（状態、URL、最終デプロイ、`environment` ラベル）
- [x] アプリ作成（リポジトリ選択 → Compose ファイル検出 → 公開サービスとポート）。同じリポジトリを複数アプリに紐づけられる
- [x] アプリ詳細のタブ: 概要 / デプロイ（ブランチ・コミット選択、履歴、SSE ライブログ）/ ログ / 環境変数 / ドメイン / 設定
- [x] 本番アプリ（`environment = production`）のデプロイと削除に確認ダイアログ
- 完了条件: ブラウザ操作だけで新規アプリを作成・デプロイし、独自ドメインを設定できる

### M5 運用

- [x] ログコレクタ（`docker compose logs --follow` を追記保存、再起動時は `--since` で再開、ローカル 14 日保持）
- [x] 4 時間ごとのオブジェクトストレージへの退避（`logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz`）、過去ログの絞り込み表示、presigned URL でのダウンロード
- [x] バックアップ: 基盤（`koroku.db` は `VACUUM INTO` 後）、named volume、ダンプフック（`docker compose exec -T` の出力を `restic backup --stdin`）
- [x] バックアップのスケジュール（日次 03:00 JST）、手動実行、`forget --prune`、週次 `check`、最終成功時刻と警告の表示
- [x] リストア: volume は UI から、DB ダンプはダウンロードまで。`koroku restore-system` でホスト全損から復旧
- [x] start / stop / restart、アプリ削除（volume も消すか選択）、image / builder prune、システム画面
- [x] プロビジョニングスクリプト（Docker、Caddy、restic、`koroku` の unit、swap 2GB、Docker のログローテーション設定、カーネル更新）
- 完了条件: 新規 VPS がスクリプト 1 本で立ち上がり、下の受け入れシナリオがすべて通る

## 受け入れシナリオ

1. private リポジトリの Rails + PostgreSQL の Compose を UI からデプロイし、`https://<slug>.dev.example.com` が応答する。
2. 環境変数を変更して再デプロイすると反映され、DB のデータは残る。
3. 起動に失敗するコミットをデプロイすると `failed` になり、旧版が応答し続ける。
4. アプリを 2 つ並べても、互いの DB ポートがホスト外から到達できない。
5. あるアプリのデプロイ中、別アプリは応答し続ける。
6. 独自ドメインを追加し、DNS 設定後に HTTPS で応答する。プライマリ以外のホスト名はリダイレクトされる。
7. バックアップ取得 → volume を空にする → リストアでデータが戻る。
8. 4 時間以上前のログがオブジェクトストレージにあり、UI からダウンロードできる。

## 人が用意するもの

Claude Code は以下を代行しない。必要になった時点で依頼し、値を受け取ってから進める。M1 まではローカルの Docker だけで進められる。

| 必要になる時点 | 用意するもの |
| --- | --- |
| M2 | VPS（Ubuntu 24.04 か 26.04、amd64、メモリ 4GB 程度）と、その IP に向けた DNS レコード（ワイルドカードが登録できなければ `console.<base_domain>` と各 `<slug>.<base_domain>` を個別に） |
| M3 | GitHub App の登録（権限は Contents: Read-only、Metadata: Read-only、Deployments: Read and write、callback は `https://<console_host>/auth/github/callback`）、App ID、秘密鍵、許可するユーザー ID。push での自動デプロイを使うなら、Webhook を Active にして URL `https://<console_host>/webhooks/github` と secret を設定し、Push のイベントを購読する（secret は `[github] webhook_secret`）。自動デプロイでワークフローの成功を待つなら、Actions: Read-only を足して Workflow run のイベントも購読する |
| M5 | オブジェクトストレージのバケット、バケット限定のアクセスキー、`logs/` プレフィックスに 90 日削除のライフサイクルルール。restic のリポジトリパスワード（VPS 外にも控える） |

## やらないこと

- `git push` デプロイ、Addons、buildpack
- マルチノード、オートスケール
- マルチテナント（利用者ごとに隔離した基盤）。メンバーはオーナーのホストを共有し、アプリ単位のロールで分けるだけ。組織やチーム、メンバー同士での権限の委譲は持たない
- 無停止デプロイ（デプロイ対象アプリの数秒の断は許容）
- ホスト冗長化（ホスト障害時の全停止は許容）
- Loki などのログ基盤、外部への障害通知

## 既定案で進める未決事項

判断を求めずに既定案で実装してよい。変えたくなったら ARCHITECTURE の該当する章を書き換える。

- バックアップの世代: 日次 7、週次 4、月次 6
- ログの保持: ローカル 14 日、オブジェクトストレージに 90 日
- 起動失敗時の自動ロールバック: 既定で有効
