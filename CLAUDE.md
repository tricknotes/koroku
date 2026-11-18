# paasd — Claude Code 作業ガイド

個人用の Heroku ライクなホスティング基盤を実装するリポジトリ。VPS 1 台の上で、GitHub リポジトリの Docker Compose アプリ（3〜5 個）を UI からデプロイする。

## 参照する資料

- `docs/ARCHITECTURE.md` が設計の正。実装前に必ず全体を読むこと。
- マイルストーンと完了条件、受け入れシナリオは **このファイルが優先**（ARCHITECTURE.md の 11 章は初版のままで、後から追加した独自ドメイン・ログ・バックアップが反映されていない）。
- それ以外で両者が食い違ったら ARCHITECTURE.md に従う。
- 設計を変える必要が出たら、実装より先に ARCHITECTURE.md を更新し、変更理由を書く。黙って設計から外れない。

## 確定している前提

| 項目 | 内容 |
| --- | --- |
| 実行基盤 | さくらの VPS 1 台、Ubuntu 24.04、x86_64 |
| ベースドメイン | `dev.tricknotes.io`（アプリは `<slug>.dev.tricknotes.io`、管理 UI は `console.dev.tricknotes.io`） |
| コントロールプレーン | Go 製シングルバイナリ `paasd`（systemd、ホスト直置き）+ SQLite |
| フロントエンド | Ember + TypeScript の SPA。ビルド成果物を `paasd` に embed |
| リバースプロキシ | Caddy（ホスト直置き）。設定は DB から全体を生成して admin API の `POST /load` |
| GitHub 連携 | GitHub App（ログイン + installation token で clone） |
| デプロイ単位 | 1 アプリ = 1 Compose プロジェクト（`-p paas-<slug>`）。staging と本番は別アプリ |
| 外部ストレージ | Cloudflare R2（S3 互換 API のみ使用。S3 に差し替え可能にする） |
| バックアップ | restic → R2。日次 03:00 JST |
| ログ退避 | 4 時間ごとに R2 へ |

## リポジトリ構成

```text
cmd/paasd/            エントリポイント（serve、restore-system などのサブコマンド）
internal/api/         HTTP ハンドラ、セッション、SSE
internal/deploy/      デプロイの状態機械とワーカー
internal/compose/     override 生成、config 正規化、ポリシー検査
internal/caddy/       Caddy JSON 設定の生成と投入
internal/github/      GitHub App 認証、API クライアント
internal/domains/     独自ドメインの検証
internal/logs/        ログコレクタと R2 退避
internal/backup/      restic 呼び出し、スケジュール、リストア
internal/store/       SQLite、マイグレーション
internal/runner/      外部コマンド実行の抽象
web/                  Ember アプリ
deploy/               プロビジョニングスクリプト、systemd unit
docs/                 設計資料
testdata/             サンプル Compose、golden file
```

## 実装規約

- Go は標準ライブラリ中心（`net/http`、`database/sql`）。Web フレームワークや ORM は入れない。SQLite ドライバは cgo 不要の `modernc.org/sqlite`。
- 依存を追加するときは、標準ライブラリで足りない理由をコミットメッセージに書く。
- 外部コマンド（`docker`、`git`、`restic`）は必ず `internal/runner` の `Runner` インターフェース経由で呼ぶ。引数は配列で渡し、シェル文字列を組み立てない。
- GitHub、Caddy、オブジェクトストレージ、時計はインターフェースにして、テストでフェイクに差し替えられるようにする。
- 環境変数の値、トークン、鍵をログ・エラーメッセージ・デプロイログに出さない。出力前にマスクする。
- 外部呼び出しには `context` でタイムアウトを付ける。エラーは文脈を付けて wrap する。
- DB が唯一の正。Caddy 設定や override は常に DB から再生成できること。
- UI の文言は日本語。コード、コメント、コミットメッセージは英語。

## テストと確認コマンド

```sh
go vet ./...
go test ./...                       # ユニット（Docker 不要）
go test -tags integration ./...     # 実 Docker を使う結合テスト
cd web && pnpm lint && pnpm test    # フロントエンド
```

- override 生成、Caddy 設定生成、ポリシー検査は golden file で固定する。
- マイルストーンを終えるときは、下の完了条件を実際に実行して確かめ、結果（コマンドと出力の要点）を報告する。確認していないものを完了と書かない。

## マイルストーン

M0 から順に進める。各マイルストーンの着手時に作業計画を示し、完了時に完了条件の確認結果を報告する。

### M0 骨格

- [ ] リポジトリ構成、`config.toml` 読込、SQLite マイグレーション（埋め込み SQL を起動時に適用）
- [ ] Ember の空アプリを embed して配信
- [ ] CI（vet、ユニットテスト、フロントの lint とテスト）
- 完了条件: `paasd serve` が起動し、`GET /api/system` が応答する

### M1 デプロイエンジン

- [ ] public リポジトリの URL と SHA を API で受け、取得 → env ファイルと override 生成 → `config` で検証 → build / pull → `up --wait` → HTTP 確認
- [ ] ポリシー検査（ARCHITECTURE 6 章）と、アプリ単位の「検査を緩める」フラグ
- [ ] サービスごとのメモリ上限を override で付与（既定値あり、アプリ設定で変更可）
- [ ] ディスク使用率 85% 超でデプロイ拒否
- [ ] 状態機械（queued / running / succeeded / failed / canceled）、同時実行 1、起動時の中断ジョブ処理
- [ ] 手順 6 以降の失敗で直前の成功リリースへ自動ロールバック
- [ ] デプロイログのファイル出力とマスク
- [ ] 実機確認: `--env-file` 指定時にリポジトリ内の `.env` がどう扱われるか。結果を ARCHITECTURE 12 章に追記
- 完了条件: サンプル 2 種（`build:` あり / `image:` のみ）が API 経由で起動し、起動に失敗するコミットでは旧版が応答し続ける

### M2 ルーティングとドメイン

- [ ] ホストポート割当（20000 番台、DB で一意）
- [ ] Caddy JSON 設定の生成と `POST /load`。`paasd` 起動時にも実行
- [ ] 自動割当ドメインの TLS（開発中は Let's Encrypt staging）
- [ ] `app_domains` テーブル、独自ドメインの追加・DNS 検証（`public_ip` と照合、5 分間隔で再試行）・強制有効化
- [ ] プライマリドメインへの 308 リダイレクト
- [ ] 停止中アプリは 503 の固定ページ
- [ ] Caddy のアクセスログをアプリ別に出力（M5 のコレクタが拾う）
- [ ] 実機確認: 設定再投入時に他アプリの接続（WebSocket、SSE）が切れないか。結果を ARCHITECTURE 12 章に追記
- 完了条件: `https://<slug>.dev.tricknotes.io` が応答し、独自ドメインを追加して DNS を向けると HTTPS で応答する

### M3 GitHub App

- [ ] Web application flow でのログイン、許可ユーザー ID との照合、セッション Cookie
- [ ] 変更系 API の `Origin` 検証
- [ ] リポジトリ / ブランチ / コミット一覧
- [ ] installation token による private リポジトリの取得（トークンを `.git/config` とログに残さない）
- 完了条件: 許可 ID 以外はログインできない。private リポジトリをデプロイできる

### M4 UI

- [ ] アプリ一覧（状態、URL、最終デプロイ、`environment` ラベル）
- [ ] アプリ作成（リポジトリ選択 → Compose ファイル検出 → 公開サービスとポート）。同じリポジトリを複数アプリに紐づけられる
- [ ] アプリ詳細のタブ: 概要 / デプロイ（ブランチ・コミット選択、履歴、SSE ライブログ）/ ログ / 環境変数 / ドメイン / 設定
- [ ] 本番アプリ（`environment = production`）のデプロイと削除に確認ダイアログ
- 完了条件: ブラウザ操作だけで新規アプリを作成・デプロイし、独自ドメインを設定できる

### M5 運用

- [ ] ログコレクタ（`docker compose logs --follow` を追記保存、再起動時は `--since` で再開、ローカル 14 日保持）
- [ ] 4 時間ごとの R2 退避（`logs/<slug>/<service>/YYYY/MM/DD/HH.log.gz`）、過去ログの絞り込み表示、presigned URL でのダウンロード
- [ ] バックアップ: 基盤（`paas.db` は `VACUUM INTO` 後）、named volume、ダンプフック（`docker compose exec -T` の出力を `restic backup --stdin`）
- [ ] バックアップのスケジュール（日次 03:00 JST）、手動実行、`forget --prune`、週次 `check`、最終成功時刻と警告の表示
- [ ] リストア: volume は UI から、DB ダンプはダウンロードまで。`paasd restore-system` でホスト全損から復旧
- [ ] start / stop / restart、アプリ削除（volume も消すか選択）、image / builder prune、システム画面
- [ ] プロビジョニングスクリプト（Docker、Caddy、restic、`paasd` の unit、swap 2GB、Docker のログローテーション設定、カーネル更新）
- 完了条件: 新規 VPS がスクリプト 1 本で立ち上がり、下の受け入れシナリオがすべて通る

## 受け入れシナリオ

1. private リポジトリの Rails + PostgreSQL の Compose を UI からデプロイし、`https://<slug>.dev.tricknotes.io` が応答する。
2. 環境変数を変更して再デプロイすると反映され、DB のデータは残る。
3. 起動に失敗するコミットをデプロイすると `failed` になり、旧版が応答し続ける。
4. アプリを 2 つ並べても、互いの DB ポートがホスト外から到達できない。
5. あるアプリのデプロイ中、別アプリは応答し続ける。
6. 独自ドメインを追加し、DNS 設定後に HTTPS で応答する。プライマリ以外のホスト名はリダイレクトされる。
7. バックアップ取得 → volume を空にする → リストアでデータが戻る。
8. 4 時間以上前のログが R2 にあり、UI からダウンロードできる。

## 人が用意するもの

Claude Code は以下を代行しない。必要になった時点で依頼し、値を受け取ってから進める。M1 まではローカルの Docker だけで進められる。

| 必要になる時点 | 用意するもの |
| --- | --- |
| M2 | VPS（さくらの VPS、4GB プラン、東京か石狩）と、その IP に向けた DNS レコード（Squarespace Domains。`*.dev` が登録できなければ `console.dev` と各 `<slug>.dev` を個別に） |
| M3 | GitHub App の登録（権限は Contents: Read-only と Metadata: Read-only、callback は `https://console.dev.tricknotes.io/auth/github/callback`）、App ID、秘密鍵、許可するユーザー ID |
| M5 | R2 のバケット、バケット限定の API トークン、`logs/` プレフィックスに 90 日削除のライフサイクルルール。restic のリポジトリパスワード（VPS 外にも控える） |

## やらないこと

- `git push` デプロイ、Addons、buildpack
- マルチノード、マルチユーザー、オートスケール
- 無停止デプロイ（デプロイ対象アプリの数秒の断は許容）
- ホスト冗長化（ホスト障害時の全停止は許容）
- push 時の自動デプロイ（将来追加の可能性はあるが、今は Webhook を受けない）
- Loki などのログ基盤、外部への障害通知

## 既定案で進める未決事項

判断を求めずに既定案で実装してよい。変えたくなったら ARCHITECTURE 12 章を更新する。

- バックアップの世代: 日次 7、週次 4、月次 6
- ログの保持: ローカル 14 日、R2 に 90 日
- 起動失敗時の自動ロールバック: 既定で有効
