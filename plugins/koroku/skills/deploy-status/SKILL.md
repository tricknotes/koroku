---
name: deploy-status
description: Reads the state of the koroku apps deployed from the current repository (latest deployment, deployment log, services, runtime logs, missing environment variables) with a read-only API token, and fixes the repository when a deployment failed. Use when the user asks why a koroku deploy failed, what is running on koroku, or to check or fix a deployment. koroku のデプロイが失敗した、デプロイの状況を見て、ログを確認して、などの依頼でも使う。
---

# koroku のデプロイの状況を調べて、リポジトリを直す

koroku は GitHub リポジトリの Docker Compose アプリをデプロイする個人用の PaaS。この Skill は、手元のシェルにある読み取り専用の API トークンで koroku の状態を読み、デプロイが失敗していれば原因をログから突き止めて、リポジトリの側を直す。

できるのは読むことだけ。トークンではデプロイ、設定の変更、環境変数の値の表示はできない（koroku がそう制限している）。直したあとのデプロイと、環境変数の値の入力は、利用者が koroku の管理画面で行う。

## 準備（初回だけ。利用者が行う）

シェルに次の 2 つの環境変数が要る。なければ、[references/token-setup.md](references/token-setup.md) の手順を利用者に示して、設定してもらう。値を会話に貼ってもらったり、Claude がファイルに書いたりしない。

- `KOROKU_URL`: koroku の管理画面の URL（例 `https://console.dev.example.com`）
- `KOROKU_TOKEN`: 管理画面の「セキュリティ」→「API トークン」で作った `koroku_ro_` で始まる値

確かめるときは値を表示せず、設定されているかだけを見る。

```sh
[ -n "$KOROKU_URL" ] && [ -n "$KOROKU_TOKEN" ] && echo set || echo missing
```

## 進め方

### 1. 状況を読む

[scripts/koroku-status.sh](scripts/koroku-status.sh) をリポジトリのルートで実行する。curl が要る。jq があれば要約して表示し、なければ API の JSON をそのまま出す（その場合は JSON を読んで同じことをする）。

```sh
<skill-dir>/scripts/koroku-status.sh
```

- `git remote get-url origin` と、各アプリの `repo_full_name` / `repo_url` を突き合わせて、このリポジトリからデプロイしているアプリ（staging と本番など、複数あることがある）を見つける。見つからなければアプリの一覧を出すので、`--app <slug>` で選び直す（jq がないときも `--app` が要る）。
- アプリごとに、環境（`production` / `staging`）、稼働中のリリース、最新のデプロイを出す。無停止デプロイ（blue/green）を有効にしたか色を持つアプリは、いまの色（`web_color`）と、稼働中のリリースの Compose が無停止デプロイの条件を満たさない理由（`blue_green_problems`）も出す。最新のデプロイが `failed` か `canceled` なら、続けてデプロイログの末尾、サービスの状態、実行ログの最後の行、そのコミットで必要な環境変数のうち未設定のもの（キーだけ）を出す。

個別に読むときのサブコマンド:

| コマンド | 内容 |
| --- | --- |
| `koroku-status.sh apps` | すべてのアプリ |
| `koroku-status.sh deployments <slug>` | 直近 10 件のデプロイ（無停止デプロイは起動した色と切り替えの時刻も） |
| `koroku-status.sh log <id> [--lines N]` | デプロイログ（既定は全文） |
| `koroku-status.sh runtime <slug> [--service web] [--lines N]` | 実行ログの最後の N 行（既定 100） |
| `koroku-status.sh env <slug> [--ref <branch>\|--sha <sha>]` | 必要な環境変数と設定済みかどうか（値は出ない） |
| `koroku-status.sh containers <slug>` | コンテナの状態、メモリ上限、`oom_score_adj` と、稼働中のリリースとの違いの印 |
| `koroku-status.sh compose <slug> [--deployment <id>]` | koroku の override と、重ねた結果の Compose（`${VAR}` は補間しない。環境変数の値は伏せてある） |

エラーの意味: `the token was refused` はトークンの期限切れ・失効・打ち間違い（作り直してもらう）。`not available with a read-only token` はトークンで読めない API。`rate limited` は 1 分あたりの回数の上限（1 分待つ）。

### 2. 原因を見分ける

デプロイログは `==> <時刻> <手順> <表題>` の行で手順に分かれている。最後の `failed` の行と、その直前の手順の出力を読む。

| 失敗した手順 | よくある原因 | 直す場所 |
| --- | --- | --- |
| `fetch` | コミットが push されていない、submodule や LFS に頼っている | push する。submodule / LFS をやめる |
| `prepare` / `validate` | Compose の構文、ポリシー検査の違反（特権、ホストの名前空間、bind mount、`container_name` など）、`${VAR:?}` の必須変数が未設定。無停止デプロイのアプリでは、ルール `blue_green` の違反（公開サービスの `container_name`、`replicas`、ほかのサービスからの `depends_on`、`include:` した別ファイルの公開サービスなど。`cannot extend service "web-blue"` も同じ） | Compose ファイル。未設定の変数は利用者に管理画面で入れてもらう。`blue_green` は prepare-app の requirements.md の「無停止デプロイ」を見て直す（「検査を緩める」では許可されない） |
| `build` | Dockerfile、依存の取得、`.dockerignore` の漏れ | Dockerfile とその周辺 |
| `pull` | イメージ名やタグの誤り、private イメージ。`The registry has no image with that tag (manifest unknown)` は、CI がコミットごとにビルドするイメージがまだ push されていない（ワークフローの終了前にデプロイした） | Compose の `image:`。CI のイメージなら、ワークフローが終わってからデプロイし直す。自動デプロイなら koroku の設定タブの「先に待つワークフロー」にイメージを作るワークフローを入れる |
| `start` | コンテナがすぐ終了する、ヘルスチェックが通らない、ワンショットのサービスが誰にも待たれていない | ログの `Last output of the containers:` に続く行を読む |
| `check` | 公開サービスが `0.0.0.0:<port>` で待ち受けていない、ヘルスチェックパスが 5xx を返す | アプリの設定、ポート、ヘルスチェックパス |
| `switch` | 無停止デプロイで、Caddy への切り替えの投入に失敗した（koroku 側の問題。旧色に戻している） | リポジトリでは直せない。利用者にデプロイし直してもらい、続くなら koroku の運用者に伝える |
| `services` | 無停止デプロイで、切り替えのあとに作り直したワーカーなどが起動しない | `Last output of the containers:` を読む。そのサービスの Compose と起動コマンド |
| `drain` | 無停止デプロイで、旧色が止まらない。デプロイは `succeeded` のまま | 直さなくてよい（次のデプロイの前に消える）。続くなら旧色の `stop_grace_period` と `SIGTERM` の扱い |

無停止デプロイ（blue/green）を有効にしたアプリのデプロイは、`start`（`Starting <新色> on port <ポート>`。ワンショットのサービスと新色だけを起動する）→ `check`（新色のポート）→ `switch`（`Switching traffic from <旧色> to <新色>`）→ `services`（`Recreating the other services`）→ `drain`（`Stopping <旧色>`。待ち時間のあとに旧色を止めて消す）→ `done` と進み、`route` はない。有効にして最初のデプロイは旧色が元の形の公開サービスなので、見出しの旧色はサービス名になる（`Switching traffic from web to green`、`Stopping web`）。公開サービスが動いていない（初回、停止中、休止中）ときは `start`（`Starting <色> on port <ポート>`）→ `route` → `check` → `done` で、切り替えはない。読むときの要点:

- `start` か `check` か `switch` で失敗したら、`Removing <新色のサービス>; the previous release keeps serving.` の行のとおり新色を消しただけで、旧色・DB・ワーカーは動き続けている（ロールバックの行は出ない）。ただしマイグレーションは流れたあとかもしれない。古い版のコードと両立しないマイグレーションなら、旧色がエラーを返していることがある（prepare-app の requirements.md の「無停止デプロイ」）。
- `prepare` の `Images that follow the commit: <サービス, ...>` は、`image:` がコミットの変数に従うので毎回作り直されるサービス。
- `Not enough memory to run two <サービス> containers (<n> MiB available, <m> MiB needed).` で失敗したら（`start` の手順。デプロイの `error` も同じ文。新色は起動していない）、ホストの空きメモリが公開サービスの上限 1 つ分に足りない。リポジトリの問題ではない。利用者に、メモリを空けるか上限を下げるか、無停止デプロイを外してデプロイし直すよう伝える。
- `services` で失敗したら、koroku は旧色に戻し、ワーカーなどを前の版で起動し直している（`==> <時刻> rollback Switching back and restoring the other services of deployment #<id>` に続いて `Restored <サービス, ...> of deployment #<id>.`。戻せなければ `Restoring ... failed: ...`）。
- `services` に `Recreating <サービス> because the app's environment changed` があれば、環境変数を変えたデプロイで DB なども作り直したので、その数秒はエラーが出うる。
- デプロイの JSON（`deployments`）の `web_color` は、そのデプロイが起動した色（`blue` / `green`。公開サービスを元の形で起動したデプロイと、起動の前に終わったものは `null`）、`switched_at` は利用者のリクエストをその色へ切り替えた時刻。切り替えていない（並べる相手がなく全サービスを一度に起動した、切り替えの前に失敗した）か、切り替えたあと `services` で失敗して旧色に戻したときは `null`。`switched_at` が `null` の `failed` でも、ログに `rollback Switching back` があれば、数秒〜数十秒は新色がリクエストを受けていた。
- タスクとワンオフのコマンドは公開サービスの名前（`web`）で動く。`web-blue` / `web-green` を指定すると koroku が `422` で断る。
- `services`（`koroku-status.sh` の失敗時のサービスの状態）では、公開サービスの行に色（`blue` / `green`）が付く。デプロイで 2 つの色が並んでいる間は公開サービスの行が 2 つある。

- `rollback` の行があれば、koroku は直前の成功リリースに戻している（`Rolled back: deployment #N is running again.`）。本番が止まっているとは限らないので、そう伝える。
- 実行ログ（`runtime`）は、起動はしたが動かない場合（エラーを出して再起動を繰り返す、DB に繋がらない）の手がかりになる。
- デプロイは成功したのに動かないとき（落ちる、遅い、メモリが足りない）は、`containers` の印を見る。`oom_killed`（メモリ上限に当たって止められた）と `restarted` は、アプリのメモリの使い方か、koroku のリソースタブのメモリ上限の問題。`outdated` はコンテナが稼働中のリリースと違う設定のまま残っている状態で、管理画面から再起動か再デプロイすると揃う。`memory_limit` などの違いも同じ。リポジトリの Compose に書いた値が効いているかは `compose` の重ねた結果で確かめる（`mem_limit`、`oom_score_adj`、予約は koroku が上書きする）。`status` は印のあるコンテナを自動で出す。

koroku 側の前提（ポートの扱い、環境変数の注入、volume、ワンショットのサービス、ポリシー検査）は、同じプラグインの prepare-app の [requirements.md](../prepare-app/references/requirements.md) に書いてある。直す前に該当する項目を読む。

### 3. 直す

- 変えるのはリポジトリだけ。prepare-app と同じく、変更は最小限にし、手元の開発の流れを壊さない。秘密をリポジトリに書かない。
- 原因が環境変数の未設定なら、リポジトリは直さずに、どの変数をどのアプリ（slug、環境）に入れるかを利用者に伝える。値は聞かない。
- できれば prepare-app の `scripts/rehearse.sh` で手元でリハーサルしてから終える（Docker が要る）。
- 推測で直したところと、ログで確かめたところを分けて書く。

### 4. 報告する

- どのアプリのどのデプロイ（`#id`、コミット）が、どの手順で、なぜ失敗したか（ログの該当行を短く引用）。
- 変えたファイルと理由。リハーサルの結果。
- 利用者がすること: 管理画面で入れる環境変数、コミットと push、管理画面からのデプロイ（本番アプリなら確認ダイアログが出る）。
- ログに秘密らしい値（パスワード、トークン）が出ていたら、報告に貼らず、出ていたことだけを伝える。

## 守ること

- `KOROKU_TOKEN` の値を表示しない、ファイルに書かない、コミットしない、コマンドの引数に直接書かない（スクリプトは curl に標準入力で渡している）。`.envrc` などに書く場合は、そのファイルが git の管理外であることを確かめる。
- トークンで変更系の API を試さない（403 になるだけで、目的にも合わない）。
