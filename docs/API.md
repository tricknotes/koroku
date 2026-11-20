# koroku HTTP API

管理 UI（`web/`）と `koroku` の間の取り決め。ARCHITECTURE 9 章の一覧を、リクエストとレスポンスの形まで具体化したもの。食い違いがあれば実装（`internal/api`）とこのファイルを同時に直す。

各節の見出しにある **M1** などは、その API を実装するマイルストーン。

## 共通

- JSON は `Content-Type: application/json`。時刻は RFC 3339（UTC）。未設定の時刻は `null`。
- 変更系（`POST` / `PUT` / `PATCH` / `DELETE`）のリクエストボディは JSON オブジェクト 1 つ。未知のフィールドは `400 invalid_json`。
- 変更系のリクエストには、管理 UI のオリジンと一致する `Origin` ヘッダーが要る（ブラウザーの `fetch` は自動で付ける）。なければ `403 forbidden_origin`。
- ボディのある変更系リクエストは `Content-Type: application/json` でなければ `415 unsupported_media_type`。
- ブラウザーが `Sec-Fetch-Site` で同一オリジン以外（`same-site`、`cross-site`）と申告した API リクエストは、参照系も含めて `403 forbidden_origin`。
- `--insecure-no-auth`（開発用）のときは、Host がループバック名（`localhost`、`127.0.0.1` など）でなければ `403 forbidden_host`。
- エラーは次の形。`code` は安定した識別子で、UI はこれを日本語の文言に対応づける。`message` は英語の補足。

```json
{ "error": { "code": "validation_failed", "message": "some fields are invalid", "details": { "web_port": "must be between 1 and 65535" } } }
```

| ステータス | `code` | 意味 |
| --- | --- | --- |
| 400 | `invalid_json`、`invalid_limit` など | リクエストの形が不正 |
| 401 | `unauthenticated` | 未ログイン（M3 以降、`/auth/*` と `GET /api/session` 以外のすべて） |
| 401 | `invalid_token` | `Authorization: Bearer` の API トークンが期限切れ・失効・形式違い（API トークンの節） |
| 403 | `forbidden_origin` | 変更系リクエストの `Origin` が管理 UI のものではない（M3） |
| 403 | `token_not_allowed` | API トークンでは使えない API（API トークンの節） |
| 403 | `reauth_required` | 直近 10 分以内のログインが要る操作（ARCHITECTURE 10.3）。UI は再ログインを案内する |
| 404 | `not_found`、`app_not_found`、`deployment_not_found`、`domain_not_found` | 対象がない |
| 409 | `slug_taken`、`no_free_port`、`not_cancelable`、`hostname_taken`、`busy`、`too_many_tokens` | 状態の衝突 |
| 422 | `validation_failed`（`details` はフィールド名 → 理由）、`ref_unresolvable` | 入力値が不正 |
| 429 | `rate_limited` | API トークンの利用回数の上限（1 分あたり 120 回）を超えた。`Retry-After` あり |
| 507 | `disk_full`（`details.used_percent`、`details.limit_percent`） | ディスク使用率が上限超過でデプロイ拒否 |
| 500 | `internal` | サーバー側の失敗（詳細はサーバーログ） |
| 503 | `not_configured` | その機能に必要な設定（GitHub App、オブジェクトストレージなど）がない |

## セッション（M3、強化は 2026-11-19）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/session` | 常に 200。`{"authenticated": true, "user": {"id": 1, "login": "octo", "avatar_url": "https://..."}, "reauth_until": "2026-11-19T03:10:00Z"}` か `{"authenticated": false}`。`reauth_until` は再ログインなしで危険な操作ができる期限（ログインから 10 分。`--insecure-no-auth` では `null`） |
| `GET /auth/github/login?return_to=/apps/blog/env` | GitHub の認可画面へリダイレクト（ページ遷移で開く）。`return_to` はログイン後に戻るパス（省略時は `/`）。`/` で始まらないもの、`//` や `\` を含むものは無視して `/` に戻す |
| `GET /auth/github/callback` | GitHub からの戻り先。成功すると Cookie を発行して `return_to` へ、拒否なら `/?login_error=<code>` へリダイレクト。`code` は `not_allowed`（許可ユーザーではない）、`two_factor_required`（GitHub の 2 段階認証が無効）、`two_factor_unknown`（GitHub が 2 段階認証の状態を返さなかった）、`state_mismatch`、`github_error`、`not_configured`（GitHub App が未設定） |
| `POST /auth/logout` | 204。セッションを破棄 |
| `GET /api/sessions` | 有効なセッションの一覧（新しい順）。`{"sessions": [{"id": "9f2c41d07a1b3e55", "created_at": "...", "last_seen_at": "...", "expires_at": "...", "ip": "203.0.113.5", "user_agent": "Mozilla/5.0 ...", "current": true}]}`。`last_seen_at` は 1 分単位の目安 |
| `DELETE /api/sessions/{id}` | 204。そのセッションを終了する（自分のセッションを指定するとログアウトと同じ）。なければ `404 session_not_found` |
| `POST /api/sessions/revoke-others` | 自分以外のセッションをすべて終了する。200 で `{"revoked": 2}` |
| `GET /api/login-events` | ログイン履歴（新しい順、最大 50 件）。`{"events": [{"at": "...", "outcome": "succeeded", "github_user_id": 1, "login": "octo", "ip": "203.0.113.5", "user_agent": "Mozilla/5.0 ..."}]}`。`outcome` は `succeeded` / `not_allowed` / `two_factor_required` / `two_factor_unknown` |

`--insecure-no-auth` のとき、`/api/sessions` は空の一覧、`/api/login-events` は空の履歴を返し、`reauth_required` は起きない。

再ログイン（`403 reauth_required`）が要る操作: アプリの削除、本番アプリのデプロイ作成、`PUT /api/apps/{slug}/env`、`PUT`・`DELETE /api/apps/{slug}/env/{key}`、volume のリストア、DB ダンプのダウンロード、DB のコピー（`POST /api/apps/{slug}/db-copies`）、`PATCH /api/apps/{slug}` で `environment`・`relax_policy`・`repo_full_name`・`repo_url`・`repo_id` のいずれかを今と違う値にするもの。`GET /api/apps/{slug}/env` は断らず、値を隠して返す（環境変数の節）。

## API トークン（2026-11-21）

手元の Claude Code などが koroku の状態を読むための、読み取り専用のトークン（ARCHITECTURE 10.5）。管理 UI の「セキュリティ」画面で発行する。

**管理（セッション Cookie でだけ使える）**

トークンの形: `{"id": "3fa9c2d17b40e815", "name": "ノート PC の Claude Code", "created_at": "...", "expires_at": "...", "last_used_at": null, "last_used_ip": ""}`。`last_used_at` は 1 分単位の目安で、まだ使われていなければ `null`。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/tokens` | 期限内のトークンの一覧（新しい順）。`{"tokens": [トークン, ...]}` |
| `POST /api/tokens` | 発行。ボディ `{"name": "...", "expires_in_days": 30}`。`name` は 1〜100 文字、`expires_in_days` は `7` / `30` / `90` のいずれか（それ以外は 422）。直近 10 分以内のログインでなければ `403 reauth_required`。有効なトークンが 20 個あれば `409 too_many_tokens`。201 で `{"token": トークン, "value": "koroku_ro_..."}`。**`value` はこの応答でしか返らない** |
| `DELETE /api/tokens/{id}` | 204。失効させる（再ログインは要らない）。なければ `404 token_not_found` |

**使い方**: `Authorization: Bearer koroku_ro_...` を付けて `GET` する。クエリパラメーターや Cookie では受けない。

```sh
curl -fsS -H "Authorization: Bearer $KOROKU_TOKEN" "$KOROKU_URL/api/apps"
```

トークンで使えるのは次の `GET` だけ。

| パス | 内容 |
| --- | --- |
| `/api/apps`、`/api/apps/{slug}` | アプリ |
| `/api/apps/{slug}/services` | サービスの状態 |
| `/api/apps/{slug}/deployments`、`/api/deployments/{id}` | デプロイ |
| `/api/deployments/{id}/logs` | デプロイログ（SSE、`?format=text` で全文） |
| `/api/apps/{slug}/logs/history`、`/api/apps/{slug}/logs` | 実行ログ（過去分の検索と、ライブの SSE） |
| `/api/apps/{slug}/env/requirements` | 必要な環境変数との照合（キーと設定の有無だけ） |
| `/api/system` | ホストの状態 |

- それ以外（環境変数の値 `GET /api/apps/{slug}/env`、DB ダンプのダウンロード、退避済みログの URL、セッション、トークンの管理、すべての変更系）は、トークンが有効でも `403 token_not_allowed`。
- 期限切れ・失効・形式違い（`Bearer ` の後ろが空など）は `401 invalid_token`。
- トークンごとに 1 分あたり 120 回まで。超えたら `429 rate_limited`（`Retry-After` は秒数）。
- トークンのリクエストでは Cookie を見ない（両方を付けてもトークンとしてだけ扱う）。`Origin` は要らない（`GET` だけのため）。`Sec-Fetch-Site` の検査はそのまま掛かる。

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

UI 再設計で次のフィールドが加わる（ARCHITECTURE 9.1）:

```json
{
  "disk_limit_percent": 85,
  "log_archive": { "configured": true, "last_success_at": "2026-11-18T03:10:05Z", "last_error": "" },
  "caddy": { "enabled": true, "last_loaded_at": "2026-11-18T03:00:02Z", "last_error": "" }
}
```

- `disk_limit_percent` を超えるとデプロイが拒否される。UI はこの値で警告を出す。
- `log_archive.last_success_at` はログ退避が最後に成功した時刻（まだなければ `null`）。オブジェクトストレージが未設定なら `configured: false`。
- `caddy.last_loaded_at` は Caddy へ設定を最後に投入できた時刻、`last_error` は直近の投入が失敗していればその理由（成功していれば空文字）。`[caddy] enabled = false` なら `enabled: false`。

システム画面のために、さらに次が加わる（ARCHITECTURE 9.1）:

```json
{
  "swap": { "total_bytes": 2147483648, "used_bytes": 104857600 },
  "log_archive": { "configured": true, "last_success_at": "2026-11-18T03:10:05Z", "last_error": "", "next_run_at": "2026-11-18T07:10:00Z" }
}
```

- `swap` はホストの swap の大きさと使用量。swap がない、または取得できなければ `null`。
- `log_archive.next_run_at` は次にログ退避が走る予定の時刻。オブジェクトストレージが未設定なら `null`。

`GET /api/system/docker`: Docker のディスク使用量。

```json
{
  "items": [
    { "type": "images", "total_bytes": 5368709120, "reclaimable_bytes": 1073741824, "count": 12 },
    { "type": "containers", "total_bytes": 1048576, "reclaimable_bytes": 0, "count": 6 },
    { "type": "volumes", "total_bytes": 2147483648, "reclaimable_bytes": 0, "count": 4 },
    { "type": "build_cache", "total_bytes": 3221225472, "reclaimable_bytes": 3221225472, "count": 58 }
  ]
}
```

- `reclaimable_bytes` は `POST /api/system/prune` で空く見込みで、Docker の回収可能（Reclaimable）ではない。イメージはどのコンテナにも使われていないタグのないイメージ（Compose がビルドしたものは層がビルドキャッシュに残るので除く）、ビルドキャッシュは 1 週間使われておらず、その先の記録も同じであるもの。volume とコンテナは消さないので常に 0。見込みは少なめに出る（ARCHITECTURE 10 章）。

`GET /api/system/containers`: 稼働中のコンテナのメモリと CPU を、アプリごとにまとめたもの（ARCHITECTURE 9.3）。`docker stats` が数秒かかるので `/api/system` とは分けている。

```json
{
  "cpu_count": 2,
  "memory_total_bytes": 4294967296,
  "groups": [
    {
      "app": "blog",
      "memory_bytes": 412090368,
      "memory_limit_bytes": 1073741824,
      "cpu_percent": 6.3,
      "containers": [
        { "name": "koroku-blog-web-1", "project": "koroku-blog", "service": "web", "memory_bytes": 314572800, "memory_limit_bytes": 536870912, "cpu_percent": 6.04 },
        { "name": "koroku-blog-db-1", "project": "koroku-blog", "service": "db", "memory_bytes": 97517568, "memory_limit_bytes": 536870912, "cpu_percent": 0.26 }
      ]
    },
    {
      "app": null,
      "memory_bytes": 20971520,
      "memory_limit_bytes": null,
      "cpu_percent": 0.02,
      "containers": [
        { "name": "registry", "project": "", "service": "", "memory_bytes": 20971520, "memory_limit_bytes": null, "cpu_percent": 0.02 }
      ]
    }
  ]
}
```

- `cpu_count` は Docker から見た CPU のコア数、`memory_total_bytes` はメモリの総量（`docker info`）。
- `groups` はアプリごと（`app` は slug）にメモリの多い順。koroku のアプリでないコンテナは `app: null` の 1 グループにまとめて最後に置く。稼働中のコンテナがなければ空の配列。
- `cpu_percent` は `docker stats` の `CPUPerc` と同じく **1 コアを 100% とする値**で、0〜`cpu_count` × 100。計測の瞬間値。
- `memory_bytes` は `docker stats` の使用量。`memory_limit_bytes` はコンテナのメモリ上限で、上限がなければ `null`。グループの `memory_limit_bytes` は、全コンテナに上限があるときだけその合計、1 つでもなければ `null`。
- `project`、`service` は Compose のラベル。Compose で作られていなければ空文字。
- Docker に問い合わせられなければ `500 internal`。

`POST /api/system/prune`（M5）: ボディ `{"images": true, "builder": true}`（省略時は両方）。200 で `{"output": "...", "reclaimed_bytes": 1073741824}`（コマンド出力の要約と、Docker が空けたと報告した量の合計）。コマンドが失敗したら `500 prune_failed`（`details.output` にそこまでの出力）。

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
  "env_source": "compose",
  "env_example_path": "",
  "url": "https://blog.dev.example.com",
  "primary_url": "https://www.example.org",
  "pending_domains": 0,
  "current_deployment": null,
  "latest_deployment": null,
  "created_at": "2026-11-18T03:00:00Z",
  "updated_at": "2026-11-18T03:00:00Z"
}
```

- `primary_url` はプライマリドメインがあればその URL、なければ `url` と同じ。`pending_domains` は検証待ちの独自ドメインの数（どちらも UI 再設計で追加）。
- `desired_state` は `running` / `stopped`、`environment` は `production` / `staging`。
- `current_deployment` は稼働中の（最後に成功した）デプロイ、`latest_deployment` は最新のデプロイ。どちらも「デプロイ」の形か `null`。
- `compose_path` が空なら自動検出（`compose.yaml`、`compose.yml`、`docker-compose.yml`、`docker-compose.yaml` の順）。
- `memory_limit` が空なら基盤の既定値。書式は数字 + 単位 `b` `k` `m` `g`（例 `512m`）。
- 必要な環境変数の読み取り方（ARCHITECTURE 9.4）: `env_source` は `compose`（既定。Compose ファイルの `${VAR}`）/ `dotenv`（`.env.example`）/ `off`。`env_example_path` は `dotenv` で読むファイルのパスで、空なら `.env.example`。
- バックアップの設定: `backup_exclude_volumes`（バックアップから外す volume 名の配列。Compose ファイルに書いた名前）、`dump_hook_service` と `dump_hook_command`（ダンプフック。両方を指定するか、両方を空にする）、`backup_before_deploy`（真偽値）。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps` | `{"apps": [アプリ, ...]}`（slug 順） |
| `POST /api/apps` | 作成。必須は `slug`、`web_port`、リポジトリ（`repo_full_name` か `repo_url`）。ほかは既定値あり。201 でアプリ。`slug` は英小文字で始まる英小文字・数字・ハイフン 40 文字以内で、作成後は変えられない |
| `GET /api/apps/{slug}` | アプリ |
| `PATCH /api/apps/{slug}` | 送ったフィールドだけ更新。200 でアプリ |
| `DELETE /api/apps/{slug}?volumes=true`（M5） | コンテナを止めて削除。`volumes=true` なら named volume も消す。204 |
| `POST /api/apps/{slug}/start`、`/stop`、`/restart`（M5） | 起動制御。完了まで待って 200 でアプリ。一度もデプロイしていなければ `409 not_deployed`、デプロイ中やほかの操作の実行中なら `409 busy`（削除も同じ）。起動時にリリースのファイルがポリシー検査を通らなくなっていたら `422 policy_violation`（`details` は違反の一覧。再デプロイで直る） |
| `GET /api/apps/{slug}/services`（M5） | `{"services": [{"name": "web", "state": "running", "status": "Up 2 hours", "health": "healthy"}]}`。`health` はヘルスチェックがなければ空文字。各サービスに `memory_bytes`（現在の使用量）と `memory_limit_bytes`（上限。無制限なら 0）も付く。取得できなければどちらも `null` |

### メモリ使用量の履歴（2026-11-21）

`GET /api/apps/{slug}/memory?range=24h`: サービスごとのメモリ使用量の推移と、上限の目安（ARCHITECTURE 9.5）。`range` は `24h`（既定）/ `7d` / `14d`。ほかの値は `400 invalid_range`。

```json
{
  "range": "24h",
  "step_seconds": 300,
  "from": "2026-11-20T03:00:00Z",
  "to": "2026-11-21T03:00:00Z",
  "services": [
    {
      "name": "web",
      "limit_bytes": 536870912,
      "peak_bytes": 501219328,
      "restarts": 1,
      "points": [
        { "at": "2026-11-20T03:00:00Z", "max_bytes": 402653184, "avg_bytes": 380633088, "limit_bytes": 536870912 }
      ],
      "suggestion": {
        "basis_since": "2026-11-17T09:20:00Z",
        "basis_hours": 89.5,
        "peak_bytes": 501219328,
        "p95_bytes": 452984832,
        "ready": true,
        "limit_bytes": 805306368,
        "limit": "768m",
        "flag": "near_limit"
      }
    }
  ]
}
```

- `services` は期間内にデータがあるサービス（名前順）。記録がまだなければ空の配列。
- `points` は期間内の点を古い順に並べたもの。`step_seconds`（24h は 300、7d は 1800、14d は 3600）ごとにまとめ、`at` はその区切りの始まり。データのない区切り（停止中、koroku が止まっていた）は含めないので、UI は `step_seconds` より離れた点の間を線で結ばない。`max_bytes` は区切り内の最大、`avg_bytes` は平均、`limit_bytes` はその区切りの上限（上限がなければ `null`）。
- `limit_bytes`（サービス直下）は最後に記録した上限。`peak_bytes` と `restarts`（自動の再起動の回数）は表示期間内の値。
- `suggestion` は表示期間によらず、いまの上限になってからの直近 14 日（根拠）から求める。
  - `basis_since` は根拠の最初の区切り、`basis_hours` はデータのある時間の合計（5 分単位）。`peak_bytes` と `p95_bytes` は根拠の中の最大と、5 分ごとの最大の 95 パーセンタイル。
  - `ready` は根拠が 24 時間分以上あるか。`false` の間、`limit_bytes` と `limit` は `null`。
  - `limit_bytes` は根拠のピーク × 1.5 を 64MiB 単位に切り上げた値（128MiB 以上）。`limit` は同じ値を設定に書ける形にしたもの（`m` 単位）。
  - `flag` は `near_limit`（根拠のピークが上限の 90% 以上。`ready` によらず出す）、`generous`（根拠が 7 日分以上あり、ピークが上限の 30% 未満）、どちらでもなければ `null`。上限がなければ常に `null`。

## 環境変数（M1）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/env` | 下の形（`vars` は key 順。値は平文で返るので、UI は既定でマスク表示する）。直近 10 分以内のログインでなければ、値をすべて `null` にして `values_hidden: true` を返す |
| `PUT /api/apps/{slug}/env` | `{"vars": [{"key": "...", "value": "..."}]}` で全件を置き換える。200 で保存後の一覧（`GET` と同じ形） |
| `PUT /api/apps/{slug}/env/{key}` | 1 件を追加または変更する。名前が `KOROKU_` で始まる変数は koroku が書くので保存できない（422、`KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` を koroku が env ファイルに足す。ARCHITECTURE 6 章）。本文は `{"value": "..."}`。名前を変えるときは `{"key": "NEW_KEY"}` を足す（`value` を省くと値はそのまま）。名前の変更は 1 つのトランザクションで行う。200 で保存後の一覧。名前を変える元がなければ `404 env_var_not_found`、変えた先がすでにあれば `409 env_var_exists`。追加・変更で `value` がなければ 422 |
| `DELETE /api/apps/{slug}/env/{key}` | 1 件を削除する。200 で保存後の一覧。なければ `404 env_var_not_found` |

```json
{
  "vars": [
    {"key": "DATABASE_URL", "value": "postgres://...", "pending": null},
    {"key": "MAIL_FROM", "value": "noreply@example.com", "pending": "added"},
    {"key": "SECRET_KEY_BASE", "value": "...", "pending": "changed"}
  ],
  "removed": ["OLD_FLAG"],
  "compared": true,
  "values_hidden": false
}
```

- 保存しただけでは反映されず、次のデプロイから効く。`pending` は稼働中のリリース（`current_deployment`）と比べた差で、`added` は稼働中にないキー、`changed` は値が違うキー、`null` は同じ。`removed` は削除したがまだ稼働中のリリースにあるキー（key 順）。
- 稼働中のリリースがないか、比べるための記録がない古いリリースが稼働中なら `compared: false` で、`pending` はすべて `null`、`removed` は空（ARCHITECTURE 9.2）。
- 変更系はすべて、直近 10 分以内のログインでなければ `403 reauth_required`。

key は英字かアンダースコアで始まる英数字とアンダースコア。重複は 422。

### 必要な環境変数との照合（2026-11-20）

`GET /api/apps/{slug}/env/requirements?ref=main` か `?sha=<40 桁>`: リポジトリがそのコミットで必要とする変数と、保存済みの変数との照合（ARCHITECTURE 9.4）。どちらも省くとアプリの `default_branch`。値は扱わないので、再ログインから 10 分を過ぎていても応答する。

```json
{
  "source": "compose",
  "status": "ok",
  "reason": "",
  "ref": "main",
  "commit_sha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
  "path": "compose.yaml",
  "variables": [
    {"key": "SECRET_KEY_BASE", "required": true, "default": null, "description": "Rails secret", "set": true},
    {"key": "SMTP_HOST", "required": true, "default": null, "description": "", "set": false},
    {"key": "RAILS_LOG_LEVEL", "required": false, "default": "info", "description": "", "set": false}
  ],
  "unlisted": ["OLD_FLAG"]
}
```

- `source` はアプリの `env_source`。`off` なら `status: "off"` で、ほかは空。
- `status` は `ok` / `off` / `unavailable`。`unavailable` のとき `reason` は `file_not_found`、`repo_unavailable`（GitHub App からも認証なしでも読めない。private で App が未インストールなど）、`unsupported_repo`（`repo_url` が github.com 以外）、`ref_not_found`、`too_large`、`rate_limited`、`github_error` のいずれかで、`variables` と `unlisted` は空。読めなくても 200 で返す（UI は照合なしで画面を使えるようにする）。 GitHub App で読めないリポジトリは、認証なしの contents API（1 時間 60 回まで）で読む（ARCHITECTURE 9.4）。`not_configured` はもう返さない。
- `commit_sha` は照合したコミット（解決できなければ空文字）。`path` は読んだファイル（`compose` で自動検出したときは見つけた名前。見つからなければ空文字）。
- `variables` は key 順。`required` は必須か（`compose` では `${VAR:?}`・`${VAR?}`・既定値のない `${VAR}`、`dotenv` ではすべて）、`default` は Compose の既定値（`:-` / `-` の右辺。なければ `null`）、`description` は `:?` のメッセージか `.env.example` の直前のコメント（なければ空文字）、`set` は同じ名前の変数が保存されているか。
- `unlisted` は保存済みだが一覧にない key（key 順）。`compose` ではアプリのコードが直接読む変数も入るので、UI は `dotenv` のときだけ表示する。
- `ref` と `sha` の検証に失敗したら 422。アプリがなければ `404 app_not_found`。

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
  "finished_at": "2026-11-18T03:01:10Z",
  "source_deployment_id": null,
  "rollback_available": true
}
```

`status` は `queued` / `running` / `succeeded` / `failed` / `canceled`。`source_deployment_id` は、ロールバック（ARCHITECTURE 7.1）の記録なら戻す先のデプロイの ID（`ref` は `rollback`）、通常のデプロイなら `null`。`rollback_available` は、そのデプロイのリリースへ再ビルドなしで戻せるか（成功していて、ファイルが残っている。ロールバックの記録なら戻す先について）。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/deployments?limit=50` | `{"deployments": [デプロイ, ...]}`（新しい順、`limit` は 1〜200） |
| `POST /api/apps/{slug}/deployments` | ボディ `{"ref": "main"}` か `{"sha": "<40 桁>"}`（両方あれば `sha` をデプロイし `ref` は表示用）。202 でデプロイ。保持しているリリースへ再ビルドなしで戻すときは `{"rollback_to": <デプロイの ID>}`（ほかのフィールドと同時には指定できない）。戻せなければ `422 release_unavailable`、別のアプリのデプロイなら `422 validation_failed`（`details.rollback_to`）。保持していない古い版へは、過去の `sha` を指定して再デプロイする |
| `GET /api/deployments/{id}` | デプロイ |
| `POST /api/deployments/{id}/cancel` | 202 でその時点のデプロイ。実行中のものは非同期に止まる。終了済みは `409 not_cancelable` |
| `GET /api/deployments/{id}/logs` | デプロイログの SSE（下記）。`?format=text` を付けると、その時点までの全文を `text/plain` で返す |

**ロールバックのログ**: `rollback_to` で作ったデプロイは `Queued rollback to deployment #<id> (<sha>).` で始まり、手順は `prepare` `validate` `start` `route` `check` `done`（取得、ビルド、pull はない）。

**手順の見出し行**: デプロイログのうち `==> ` で始まる行は手順の区切りで、`==> <RFC 3339 の時刻> <キー> <表題>` の形をしている（例: `==> 2026-11-18T03:00:01Z build Building images`）。キーは `fetch` `prepare` `validate` `build` `pull` `backup` `start` `route` `check` `done`、成功後の後片付けの `cleanup`、失敗時は `failed`（表題に最終状態と理由）と `rollback`。UI はこの行でログを手順に分け、次の見出しまでをその手順の所要時間とする。ビルドも pull も不要なアプリではその手順の行は出ない。

**失敗とロールバックの行**: 専用の API は持たず、UI は同じログから「旧版に戻ったか」を読み取る。次の行は UI が解釈するので、変えるときは `web/app/utils/deploy-steps.ts` と同時に直す。

| 行 | 意味 |
| --- | --- |
| `==> <時刻> failed Deployment failed: <理由>` / `Deployment canceled: <理由>` | 直前の手順で失敗（またはキャンセル）した。`start` より前なら稼働中のコンテナには触れていない |
| `Last output of the containers:` | `start` か `check` の手順で失敗したとき、その手順の末尾に入る。続く行は `docker compose logs --tail 30` の出力（`web-1  | ...`）。読めなかったときは `Reading the container output failed: ...` |
| `==> <時刻> rollback Rolling back to deployment #<id>` | 直前の成功リリースへ戻し始めた |
| `Rolled back: deployment #<id> is running again.` | 戻し終えた |
| `Rollback failed: ...` / `Rollback is not possible: ...` | 戻せなかった |
| `==> <時刻> rollback No previous release; removing the failed containers` | 初回デプロイで戻す先がなく、起動しかけたコンテナを消した（失敗すると `Removing the containers failed: ...`） |
| `koroku is shutting down; the previous release is not restored automatically.` | `koroku` の停止と重なり、ロールバックを実行しなかった |

最初の見出しより前の行（`Queued deployment of <sha>.` など）は受付の記録。`koroku` の再起動で中断されたデプロイには `failed` の見出しがなく、末尾に `Failed: ...` の行だけが付く。

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

`tail` は 1〜2000（既定 200）、`limit` は 1〜5000（既定 1000）。オブジェクトストレージが未設定なら、退避済みログの 2 つの API は `503 not_configured`。`key` がそのアプリのログでなければ 422。

ARCHITECTURE 10.1 では過去ログの検索も `/logs` に同居させていたが、SSE と JSON を同じパスで出し分けると扱いにくいので `/logs/history` に分けた。

## バックアップ（M5）

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/backups` | 全体の状況。`{"configured": true, "running": false, "last_success_at": "2026-11-18T18:00:40Z", "warning": false, "runs": [{"id": 12, "trigger": "scheduled", "status": "succeeded", "started_at": "...", "finished_at": "...", "error": ""}]}`。`warning` は 24 時間以上成功がないとき true。`next_run_at` は次回の定期実行の予定時刻（未設定なら `null`）。`trigger` は `scheduled` / `manual` / `pre_deploy`、`status` は `running` / `succeeded` / `failed` |
| `POST /api/backups` | 手動実行。202 で実行（`runs` の 1 件と同じ形）。実行中なら `409 busy` |
| `GET /api/apps/{slug}/backups` | そのアプリのスナップショット。`{"snapshots": [{"id": "4bba301e", "time": "2026-11-18T18:00:12Z", "kind": "volume", "target": "pgdata"}]}`。`kind` は `volume` / `dump`、`target` は volume 名かダンプフックのサービス名。`size_bytes` は restic が記録していればその処理量、なければ `null` |
| `POST /api/apps/{slug}/backups/restore` | ボディ `{"snapshot_id": "4bba301e"}`。volume のスナップショットをリストアする（アプリ停止 → 現在の中身を退避 → 復元 → 起動）。完了まで待って 200 で `{"restored": true}` |
| `GET /api/apps/{slug}/backups/{snapshot_id}/download` | ダンプのスナップショットをファイルとしてダウンロード（`Content-Disposition: attachment`） |

`GET /api/apps/{slug}/volumes`: そのアプリの named volume の一覧（ARCHITECTURE 9.1）。バックアップの対象を選ぶために使う。

```json
{
  "volumes": [
    { "name": "pgdata", "excluded": true, "exists": true },
    { "name": "uploads", "excluded": false, "exists": true }
  ]
}
```

- `name` は Compose ファイルに書いた volume 名（`koroku-blog_pgdata` ではなく `pgdata`）。`backup_exclude_volumes` とスナップショットの `target` と同じ文字列。名前順。
- 一覧は、アプリの Compose プロジェクトの named volume のうち Docker に実在するもの（バックアップが取る volume と同じ列挙。`external` の volume は含まない）。`excluded` は `backup_exclude_volumes` に入っていれば true。
- `backup_exclude_volumes` にあるが実在しない名前も `exists: false`、`excluded: true` で含める（UI から対象へ戻せるように）。
- 一度もデプロイしていないアプリは volume がないので `{"volumes": []}`（除外リストに名前があれば、それだけが `exists: false` で並ぶ）。
- volume の大きさは返さない（求めるのが重いため）。
- バックアップが未設定でも応答する。アプリがなければ `404 app_not_found`、Docker に問い合わせられなければ `500 internal`。

バックアップが未設定（`[storage]` か restic のパスワードファイルがない）のとき、`GET /api/backups` は `configured: false` で応答し、`GET /api/apps/{slug}/volumes` は通常どおり応答し、それ以外は `503 not_configured`。そのアプリのものでないスナップショットや、種別が合わないスナップショット（volume のリストアにダンプを指定するなど）は `404 snapshot_not_found`。リストアはアプリを停止・起動するので、デプロイ中は `409 busy`。

## DB のコピー（2026-11-21）

本番アプリの DB を staging アプリへ流し込む（ARCHITECTURE 10.6）。パスの `{slug}` は受け側（staging）のアプリ。

アプリの形に次の設定が加わる（`PATCH /api/apps/{slug}` で変える。ダンプフックと同じく、サービスとコマンドは両方を指定するか両方を空にする。コマンドは 1 行 1000 文字まで）。

- `load_hook_service`、`load_hook_command`: ロードフック。コマンドは `sh -c` でそのサービスのコンテナの中で動き、標準入力からダンプを読む。
- `post_load_service`、`post_load_command`: コピー後のコマンド（個人情報を消すなど）。ロードのあと、止めたサービスを起動する前に実行する。

コピーの形:

```json
{
  "id": 7,
  "target_app": "blog-staging",
  "source_app": "blog",
  "source_kind": "live",
  "status": "succeeded",
  "error": "",
  "bytes": 18350080,
  "backup_snapshot_id": "4bba301e5d2c7a90",
  "started_at": "2026-11-21T03:00:00Z",
  "finished_at": "2026-11-21T03:01:12Z"
}
```

- `source_app` は送り側の slug（送り側を削除したあとも残る）。`source_kind` は `live`（稼働中の送り側でダンプフックを実行）/ `snapshot`（送り側の最新のダンプのスナップショット）。
- `status` は `running` / `succeeded` / `failed`。`bytes` は流したダンプの大きさ（実行中は 0）。`backup_snapshot_id` は流し込む前に取った受け側のダンプ（取らなかったら空文字）。

| メソッドとパス | 内容 |
| --- | --- |
| `GET /api/apps/{slug}/db-copies?limit=20` | そのアプリへのコピーの履歴。`{"copies": [コピー, ...]}`（新しい順、`limit` は 1〜100） |
| `POST /api/apps/{slug}/db-copies` | ボディ `{"source": "blog", "source_kind": "live", "confirm": "blog-staging"}`。202 でコピー（バックグラウンドで進む）。`source_kind` は省略時 `live` |
| `GET /api/db-copies/{id}` | コピー。なければ `404 copy_not_found` |
| `GET /api/db-copies/{id}/logs` | コピーのログの SSE。形はデプロイログと同じ（`id:` はバイト位置、終わると `event: end` で最終 `status`）。`?format=text` で全文 |

`POST` の断り方:

| ステータス | `code` | 意味 |
| --- | --- | --- |
| 403 | `reauth_required` | 直近 10 分以内のログインが要る |
| 404 | `app_not_found`、`source_not_found` | 受け側か送り側のアプリがない |
| 409 | `busy` | 受け側か（`live` では）送り側がデプロイ中・操作中・コピー中 |
| 409 | `not_deployed` | 受け側が一度もデプロイされていない |
| 409 | `target_not_running` | 受け側が停止中 |
| 409 | `source_not_running` | `live` で送り側が稼働中でない |
| 409 | `no_dump_snapshot` | `snapshot` で送り側のダンプのスナップショットがない |
| 422 | `target_not_staging` | 受け側が本番アプリ |
| 422 | `confirmation_mismatch` | `confirm` が受け側の slug と違う |
| 422 | `source_dump_hook_missing` | 送り側にダンプフックがない |
| 422 | `target_load_hook_missing` | 受け側にロードフックがない |
| 422 | `validation_failed` | `source` が受け側と同じ、`source_kind` が不正など |
| 503 | `not_configured` | `snapshot` でバックアップが未設定 |

**ログの見出し行**: `==> <時刻> <キー> <表題>` の形で、キーは `check` `backup` `stop` `copy` `after` `start` `done`、失敗時は `failed`。UI はデプロイログと同じ方法で手順に分ける。
