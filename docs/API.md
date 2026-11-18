# paasd HTTP API

管理 UI（`web/`）と `paasd` の間の取り決め。ARCHITECTURE 9 章の一覧を、リクエストとレスポンスの形まで具体化したもの。食い違いがあれば実装（`internal/api`）とこのファイルを同時に直す。

各節の見出しにある **M1** などは、その API を実装するマイルストーン。

## 共通

- JSON は `Content-Type: application/json`。時刻は RFC 3339（UTC）。未設定の時刻は `null`。
- 変更系（`POST` / `PUT` / `PATCH` / `DELETE`）のリクエストボディは JSON オブジェクト 1 つ。未知のフィールドは `400 invalid_json`。
- 変更系のリクエストには、管理 UI のオリジンと一致する `Origin` ヘッダーが要る（ブラウザーの `fetch` は自動で付ける）。なければ `403 forbidden_origin`。
- エラーは次の形。`code` は安定した識別子で、UI はこれを日本語の文言に対応づける。`message` は英語の補足。

```json
{ "error": { "code": "validation_failed", "message": "some fields are invalid", "details": { "web_port": "must be between 1 and 65535" } } }
```

| ステータス | `code` | 意味 |
| --- | --- | --- |
| 400 | `invalid_json`、`invalid_limit` など | リクエストの形が不正 |
| 401 | `unauthenticated` | 未ログイン（M3 以降、`/auth/*` と `GET /api/session` 以外のすべて） |
| 403 | `forbidden_origin` | 変更系リクエストの `Origin` が管理 UI のものではない（M3） |
| 404 | `not_found`、`app_not_found`、`deployment_not_found`、`domain_not_found` | 対象がない |
| 409 | `slug_taken`、`no_free_port`、`not_cancelable`、`hostname_taken`、`busy` | 状態の衝突 |
| 422 | `validation_failed`（`details` はフィールド名 → 理由）、`ref_unresolvable` | 入力値が不正 |
| 507 | `disk_full`（`details.used_percent`、`details.limit_percent`） | ディスク使用率が上限超過でデプロイ拒否 |
| 500 | `internal` | サーバー側の失敗（詳細はサーバーログ） |
| 503 | `not_configured` | その機能に必要な設定（GitHub App、オブジェクトストレージなど）がない |

## セッション（M3）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/session` | 常に 200。`{"authenticated": true, "user": {"id": 1, "login": "octo", "avatar_url": "https://..."}}` か `{"authenticated": false}` |
| `GET /auth/github/login` | GitHub の認可画面へリダイレクト（ページ遷移で開く） |
| `GET /auth/github/callback` | GitHub からの戻り先。成功すると Cookie を発行して `/` へ、拒否なら `/?login_error=<code>` へリダイレクト。`code` は `not_allowed`（許可ユーザーではない）、`state_mismatch`、`github_error`、`not_configured`（GitHub App が未設定） |
| `POST /auth/logout` | 204。セッションを破棄 |

## システム（M0、prune は M5）

`GET /api/system`

```json
{
  "version": "v0.1.0",
  "base_domain": "dev.example.com",
  "disk": { "total_bytes": 42949672960, "used_bytes": 8589934592, "used_percent": 21.5 },
  "memory": { "total_bytes": 4294967296, "available_bytes": 2147483648 },
  "docker_version": "29.8.0"
}
```

`disk`、`memory` は取得できなければ `null`、`docker_version` は空文字。

`POST /api/system/prune`（M5）: ボディ `{"images": true, "builder": true}`（省略時は両方）。200 で `{"output": "..."}`（コマンド出力の要約）。

## アプリ（M1、起動制御と削除は M5）

アプリの形:

```json
{
  "slug": "blog",
  "repo_full_name": "octo/blog",
  "repo_url": "",
  "default_branch": "main",
  "compose_path": "",
  "web_service": "web",
  "web_port": 3000,
  "host_port": 20000,
  "healthcheck_path": "/",
  "desired_state": "running",
  "environment": "staging",
  "relax_policy": false,
  "memory_limit": "",
  "service_memory_limits": { "db": "1g" },
  "url": "https://blog.dev.example.com",
  "current_deployment": null,
  "latest_deployment": null,
  "created_at": "2026-11-18T03:00:00Z",
  "updated_at": "2026-11-18T03:00:00Z"
}
```

- `desired_state` は `running` / `stopped`、`environment` は `production` / `staging`。
- `current_deployment` は稼働中の（最後に成功した）デプロイ、`latest_deployment` は最新のデプロイ。どちらも「デプロイ」の形か `null`。
- `compose_path` が空なら自動検出（`compose.yaml`、`compose.yml`、`docker-compose.yml`、`docker-compose.yaml` の順）。
- `memory_limit` が空なら基盤の既定値。書式は数字 + 単位 `b` `k` `m` `g`（例 `512m`）。
- M5 で次のフィールドが加わる: `backup_exclude_volumes`（文字列の配列）、`dump_hook_service`、`dump_hook_command`、`backup_before_deploy`（真偽値）。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps` | `{"apps": [アプリ, ...]}`（slug 順） |
| `POST /api/apps` | 作成。必須は `slug`、`web_port`、リポジトリ（`repo_full_name` か `repo_url`）。ほかは既定値あり。201 でアプリ。`slug` は英小文字で始まる英小文字・数字・ハイフン 40 文字以内で、作成後は変えられない |
| `GET /api/apps/{slug}` | アプリ |
| `PATCH /api/apps/{slug}` | 送ったフィールドだけ更新。200 でアプリ |
| `DELETE /api/apps/{slug}?volumes=true`（M5） | コンテナを止めて削除。`volumes=true` なら named volume も消す。204 |
| `POST /api/apps/{slug}/start`、`/stop`、`/restart`（M5） | 起動制御。完了まで待って 200 でアプリ。一度もデプロイしていなければ `409 not_deployed` |
| `GET /api/apps/{slug}/services`（M5） | `{"services": [{"name": "web", "state": "running", "status": "Up 2 hours", "health": "healthy"}]}`。`health` はヘルスチェックがなければ空文字 |

## 環境変数（M1）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/env` | `{"vars": [{"key": "RAILS_ENV", "value": "production"}]}`（key 順。値は平文で返るので、UI は既定でマスク表示する） |
| `PUT /api/apps/{slug}/env` | 同じ形で全件を置き換える。200 で保存後の一覧。反映は次のデプロイから |

key は英字かアンダースコアで始まる英数字とアンダースコア。重複は 422。

## デプロイ（M1）

デプロイの形:

```json
{
  "id": 42,
  "app_slug": "blog",
  "ref": "main",
  "commit_sha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
  "commit_message": "Fix login",
  "status": "succeeded",
  "error": "",
  "queued_at": "2026-11-18T03:00:00Z",
  "started_at": "2026-11-18T03:00:01Z",
  "finished_at": "2026-11-18T03:01:10Z"
}
```

`status` は `queued` / `running` / `succeeded` / `failed` / `canceled`。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/deployments?limit=50` | `{"deployments": [デプロイ, ...]}`（新しい順、`limit` は 1〜200） |
| `POST /api/apps/{slug}/deployments` | ボディ `{"ref": "main"}` か `{"sha": "<40 桁>"}`（両方あれば `sha` をデプロイし `ref` は表示用）。202 でデプロイ。ロールバックは過去の `sha` を指定する |
| `GET /api/deployments/{id}` | デプロイ |
| `POST /api/deployments/{id}/cancel` | 202 でその時点のデプロイ。実行中のものは非同期に止まる。終了済みは `409 not_cancelable` |
| `GET /api/deployments/{id}/logs` | デプロイログの SSE（下記）。`?format=text` を付けると、その時点までの全文を `text/plain` で返す |

**デプロイログの SSE**: ログ 1 行が 1 メッセージ（`data:` に行の文字列）。`id:` はその行までのバイト位置で、再接続時に `Last-Event-ID` として送ると続きから再開する。デプロイが終わりログを出し切ると `event: end`（`data:` は最終 `status`）を送って接続を閉じる。`EventSource` は自動で再接続するので、UI は `end` を受けたら `close()` する。

## GitHub リポジトリ（M3）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/repos` | `{"repos": [{"id": 1, "full_name": "octo/blog", "private": true, "default_branch": "main"}]}`（GitHub App をインストールしたリポジトリ） |
| `GET /api/repos/{owner}/{repo}/branches` | `{"branches": [{"name": "main", "sha": "9fceb02..."}]}` |
| `GET /api/repos/{owner}/{repo}/commits?ref=main` | `{"commits": [{"sha": "9fceb02...", "message": "Fix login", "author": "octo", "date": "2026-11-18T03:00:00Z"}]}`（新しい順、30 件） |
| `GET /api/repos/{owner}/{repo}/compose?ref=main&path=` | Compose ファイルの検出。`{"files": ["compose.yaml"], "path": "compose.yaml", "services": [{"name": "web", "ports": [3000]}, {"name": "db", "ports": [5432]}]}`。`files` はリポジトリ直下で見つかった候補、`path` は読んだファイル（`path` 指定がなければ最初の候補）、`services` はそこから読み取れたサービス名とコンテナ側ポート（読み取れなければ空配列。UI は手入力にフォールバックする） |

GitHub 側の失敗は `404 repo_not_found`（リポジトリ・ref・ファイルがない、または GitHub App が未インストール）、`503 github_rate_limited`、`502 github_error`、GitHub App が未設定なら `503 not_configured`。

## 独自ドメイン（M2）

ドメインの形:

```json
{
  "id": 3,
  "hostname": "www.example.org",
  "status": "pending",
  "is_primary": false,
  "force_active": false,
  "last_checked_at": null,
  "last_error": "",
  "records": [
    { "type": "CNAME", "name": "www.example.org", "value": "blog.dev.example.com" },
    { "type": "A", "name": "www.example.org", "value": "203.0.113.10" }
  ]
}
```

`status` は `pending` / `active`。`records` は利用者が設定する DNS レコードの候補（サブドメインなら CNAME、apex なら A）。`last_error` は直近の検証で分かった理由（英語の短文）。 `config.toml` に `public_ip` がなければ `records` に A レコードは含まれない。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/domains` | `{"default_hostname": "blog.dev.example.com", "domains": [ドメイン, ...]}` |
| `POST /api/apps/{slug}/domains` | ボディ `{"hostname": "www.example.org"}`。201 でドメイン（直後に 1 回検証する）。重複は `409 hostname_taken`、ベースドメイン配下や不正な名前は 422 |
| `POST /api/apps/{slug}/domains/{id}/verify` | 再検証。200 でドメイン |
| `PATCH /api/apps/{slug}/domains/{id}` | ボディ `{"is_primary": true}` や `{"force_active": true}`。200 でドメイン。プライマリはアプリに 1 つで、`active` のドメインだけがなれる |
| `DELETE /api/apps/{slug}/domains/{id}` | 204 |

## 実行ログ（M5）

ログ行の形: `{"service": "web", "time": "2026-11-18T03:00:00.123Z", "line": "Listening on 0.0.0.0:3000"}`

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/logs?service=&tail=200` | ライブ tail の SSE。1 メッセージの `data:` がログ行の JSON。`service` 省略で全サービス。接続はクライアントが閉じるまで続く |
| `GET /api/apps/{slug}/logs/history?service=&from=&to=&q=&limit=1000` | ローカルに残る過去 14 日分の検索。`{"lines": [ログ行, ...], "truncated": false}`（古い順）。`from` / `to` は RFC 3339、`q` は部分一致 |
| `GET /api/apps/{slug}/logs/archives?service=` | 退避済みログの一覧。`{"archives": [{"key": "logs/blog/web/2026/11/18/04.log.gz", "service": "web", "hour": "2026-11-18T04:00:00+09:00", "size": 12345}]}`（新しい順） |
| `GET /api/apps/{slug}/logs/archives/url?key=` | ダウンロード用の presigned URL。`{"url": "https://..."}`（有効 15 分） |

ARCHITECTURE 10.1 では過去ログの検索も `/logs` に同居させていたが、SSE と JSON を同じパスで出し分けると扱いにくいので `/logs/history` に分けた。

## バックアップ（M5）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/backups` | 全体の状況。`{"configured": true, "running": false, "last_success_at": "2026-11-18T18:00:40Z", "warning": false, "runs": [{"id": 12, "trigger": "scheduled", "status": "succeeded", "started_at": "...", "finished_at": "...", "error": ""}]}`。`warning` は 24 時間以上成功がないとき true。`trigger` は `scheduled` / `manual` / `pre_deploy`、`status` は `running` / `succeeded` / `failed` |
| `POST /api/backups` | 手動実行。202 で実行（`runs` の 1 件と同じ形）。実行中なら `409 busy` |
| `GET /api/apps/{slug}/backups` | そのアプリのスナップショット。`{"snapshots": [{"id": "4bba301e", "time": "2026-11-18T18:00:12Z", "kind": "volume", "target": "pgdata"}]}`。`kind` は `volume` / `dump`、`target` は volume 名かダンプフックのサービス名 |
| `POST /api/apps/{slug}/backups/restore` | ボディ `{"snapshot_id": "4bba301e"}`。volume のスナップショットをリストアする（アプリ停止 → 現在の中身を退避 → 復元 → 起動）。完了まで待って 200 で `{"restored": true}` |
| `GET /api/apps/{slug}/backups/{snapshot_id}/download` | ダンプのスナップショットをファイルとしてダウンロード（`Content-Disposition: attachment`） |
