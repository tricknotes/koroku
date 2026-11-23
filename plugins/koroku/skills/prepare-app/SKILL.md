---
name: prepare-app
description: Adapts the current repository so koroku (a self-hosted Heroku-like platform that deploys Docker Compose apps from GitHub) can deploy it, then rehearses the deploy locally. Use when the user wants to deploy, host, or run their repository on koroku, asks what koroku needs from a repository, or a koroku deploy failed with a policy violation, a health-check timeout, or a container that would not start. koroku にデプロイしたい、koroku 対応、デプロイが失敗した、などの依頼でも使う。
---

# リポジトリを koroku でデプロイできるようにする

koroku は VPS 1 台で動く個人用の PaaS で、GitHub リポジトリの Docker Compose ファイルをそのまま使ってアプリを起動する。リポジトリの Compose ファイルは書き換えず、koroku が生成する override を `-f` で重ねる。この Skill の仕事は、利用者のリポジトリ側を koroku の前提に合わせ、手元で本番に近い形でリハーサルし、koroku の画面に入れる値をまとめて渡すこと。

## 進め方

### 1. 調べる（まだ何も変えない）

- koroku でのデプロイがすでに失敗していて、その理由を知りたいときは、同じプラグインの deploy-status Skill で koroku からデプロイログと実行ログを読む（読み取り専用の API トークンが要る）。
- Compose ファイルの場所（koroku の既定は `compose.yaml`、なければ `docker-compose.yml`。別のパスも画面で指定できる）、Dockerfile、`.dockerignore`、言語とフレームワーク、DB やキャッシュなどの付属サービスを確認する。
- `docker compose -f <file> config --no-env-resolution` で正規化した結果を読む（`--no-env-resolution` を付けると env ファイルの中身が展開されないので、秘密が出力に出にくい）。
- Compose ファイルがなければ、作るところから始める（[references/examples.md](references/examples.md) の雛形を使う）。

### 2. 要件と照らし合わせる

[references/requirements.md](references/requirements.md) を読み、項目ごとに「満たしている／直す／利用者に確かめる」に分ける。特に見落としやすいのは次の点。

- **公開サービスは 1 つ**（既定名 `web`）。コンテナ内で `0.0.0.0:<port>` の平文 HTTP を待ち受ける。TLS は前段の Caddy が終端する。
- **ポートの公開は koroku が決める**。公開サービスの `ports` は置き換えられ、ほかのサービスの `ports` は消される。サービス同士はサービス名（`db:5432`）で通信する。
- **環境変数は koroku の画面で設定し、全サービスに注入される**。リポジトリの `.env` は `${VAR}` の補間に使われない。Compose の `environment:` に直書きした値は koroku の値より強いので、本番で変えたい値は `${VAR:-既定値}` の形にする。
- **アプリが必要とする環境変数は Compose ファイルに `${VAR}` で宣言する**。koroku は Compose ファイルの `${VAR}` を読み、環境変数の画面とデプロイの前に、未設定のものを知らせる。必須の変数は `${VAR:?説明}`、任意の変数は `${VAR:-既定値}`（空でよければ `${VAR:-}`）と書く。koroku は env ファイルで全変数を全サービスに注入するので、アプリのコードが直接読むだけの変数は Compose に現れず、koroku からは見えない。新しく使い始めた変数も、ここに足すまで知らされない。
- **環境ごとの値はビルド時ではなく実行時に読む**。koroku は staging のリリースを本番へ昇格するとき、ビルド定義（補間後の `build.args` を含む）が同じならビルド済みのイメージをそのまま使う。イメージに焼き込んだ環境ごとの値（`asset_host`、API の URL、`process.env` を埋め込んだ JS バンドルなど）は本番に持ち越されるので、環境ごとの値を `build.args` で渡さず、起動時の環境変数から読む（[references/requirements.md](references/requirements.md) の「ビルド時と実行時の設定」）。
- **ホスト名を直書きしない**。koroku はアプリの URL を `KOROKU_APP_URL` と `KOROKU_APP_HOST` で渡す。pull request ごとのレビューアプリはホスト名が作るまで決まらないので、Rails の `config.hosts` やメールの URL の土台はこれから組み立てる。レビューアプリには親（staging）の環境変数が写り、`KOROKU_PR_NUMBER` が入るので、外へ送るもの（メール、Webhook、決済）はこれを見て止める（[references/requirements.md](references/requirements.md) の「pull request ごとのレビューアプリ」）。
- **データは named volume に置く**。書き込み可能な bind mount、`external` や `name:` 付きの volume、`container_name` は使えないか、同じリポジトリを staging と本番の 2 アプリで動かすと衝突する。
- **ワンショットのサービス（マイグレーションなど）は `restart: "no"` を書き、別のサービスから `condition: service_completed_successfully` で待たれるようにする**。誰も待っていないと、正常終了でも `up --wait` が失敗する。
- **デプロイのたびに全サービスが作り直される**。cron（supercronic など）やジョブキューのワーカーも `SIGTERM` を受け、`stop_grace_period`（既定 10 秒）で強制終了される。長いジョブは途中で殺されても続きからやり直せるように作り、走っている処理を待って止まれるサービスには `stop_grace_period` を付ける（全サービスの合計で 10 分まで。[references/requirements.md](references/requirements.md) の「デプロイのたびに止まる長いジョブ」）。
- **無停止デプロイ（blue/green）を使うなら、マイグレーションを古い版のコードと両立させる**。koroku のアプリ設定で選んだアプリだけ、公開サービスを 2 つ並べて入れ替える。マイグレーションは古い版が動いている間に流れるので、カラムの削除や名前の変更は 2 回のデプロイに分ける。公開サービスは状態を持たず、プロセス内の cron のような処理を動かさず、`container_name` を書かず、ほかのサービスから `depends_on` されないようにする。デプロイの間は公開サービスのメモリ上限 1 つ分が余分に要り、足りなければデプロイが失敗する（旧版は動き続ける）。無停止になるのはデプロイと即時ロールバックだけで、再起動は普通の再起動、アプリの環境変数を変えたデプロイでは DB も作り直す（[references/requirements.md](references/requirements.md) の「無停止デプロイ」）。
- **デプロイ時に取得されるのはコミット済みのファイルだけ**。`.git` は消され、submodule と Git LFS は取得されない。
- **staging のアプリは既定で koroku のログインの後ろにある**（アクセスの保護）。ブラウザー以外の呼び出し元（Webhook、外部の死活監視、ほかのサービスからの API 呼び出し）は通れないので、受け口のパスを洗い出し、利用者に「保護を外すパス」への登録を頼む（例 `/webhooks/`）。外したパスはアプリ自身で認証する（署名の検証など）。`/.koroku/` で始まるパスは koroku が使うのでアプリには届かない。
- **本番の設定で起動する**。koroku にアプリの開発モードはない。ホットリロードやソースの bind mount は `compose.override.yaml` に分ければ、手元の `docker compose up` では使われ、koroku では使われない（koroku は `-f` を明示するので自動で読まない）。

### 3. 直す

- 変更は最小限にし、手元の開発の流れ（`docker compose up` など）を壊さない。本番専用の値は環境変数にする。
- 秘密（パスワード、API キー、`SECRET_KEY_BASE` など）をリポジトリに書かない。必要な変数は一覧にして利用者に渡す。
- アプリが読む環境変数（コードの `ENV["..."]`、`process.env.X`、`os.Getenv` など、フレームワークの設定ファイルを含む）を洗い出し、公開サービスの `environment:` に `${VAR:?説明}` か `${VAR:-既定値}` として並べる（[references/requirements.md](references/requirements.md) の「必要な変数の宣言」）。値は koroku の画面で入れるので、`:?` の説明には値ではなく意味や作り方を書く。
- 方針の分かれる変更（Dockerfile の新規作成、DB の種類の変更、開発用設定の `compose.override.yaml` への移動など）は、手を付ける前に利用者に確かめる。
- ポリシー検査の「検査を緩める」フラグに頼る直し方は勧めない。どうしても要るなら理由と影響を説明して、利用者に判断してもらう。

### 4. 手元でリハーサルする

[scripts/rehearse.sh](scripts/rehearse.sh) が koroku のデプロイ手順をなぞる。Docker と Compose 2.24.4 以上が要る。

```sh
<skill-dir>/scripts/rehearse.sh --service web --port 3000 --health /up --env-file /path/to/test.env
```

本番アプリとして出すなら `--environment production` を付けると、koroku が本番に付ける `oom_score_adj: -400` とメモリの予約（既定は上限の半分、`--reservation` で変更）も重ねる。既定は staging（`oom_score_adj: 500`、予約なし）。rootless の Docker は負の `oom_score_adj` を付けられないので、本番のリハーサルは rootful の Docker か Docker Desktop で行う。

登録しておくと便利なコマンドがあれば `--task web:'bin/rails db:migrate:status'` のように渡すと、koroku のタスクと同じ形（新しいコンテナ、端末と入力なし）で実行して確かめる。

アクセスがない間の休止を使うアプリ（たいていの staging）は `--sleep` を付ける。koroku が休止と起動でするのと同じく、`stop` してから `start --wait` し、もう一度 HTTP で確かめ、起きるまでの秒数（休止のあとの最初のリクエストが待つ時間）を出す。

無停止デプロイ（blue/green）を使う、または使えるか確かめたいアプリは `--blue-green` を付ける。koroku が断る形（公開サービスの `container_name`、`deploy.replicas` と `scale`、ほかのサービスからの依存、`<公開サービス>-blue` / `-green` という名前、`include:`）を先に調べる。`image:` がコミットの変数（`KOROKU_COMMIT_SHA` / `KOROKU_COMMIT_SHORT`）に従うサービスを koroku と同じ方法で見つけて出し（`Images that follow the commit:`）、色の付いた override を重ねて blue を起動したあと、もう一度デプロイして green への切り替えをなぞる（ワンショットと green を `up --no-deps --wait` → green の確認 → 切り替え → ほかのサービスの作り直し → `--drain` 秒（既定 10）待って blue を止める）。その間 0.2 秒ごとに「いまの転送先」のポートへリクエストを送り、接続エラーか 5xx が 1 件でもあれば失敗にする。green のポートは `--alt-host-port`（既定 28081）。DB が作り直されずに残ったかもサービスごとに出す。

```sh
<skill-dir>/scripts/rehearse.sh --service web --port 3000 --health /up --env-file /path/to/test.env --blue-green
```

- リポジトリの追跡中と未追跡（ignore されていない）のファイルだけを一時ディレクトリにコピーし、`.git` なしで動かす。
- koroku と同じ形の override（公開サービスのポートを `127.0.0.1` に置き換え、ほかのポートを消し、env ファイルを全サービスに注入し、メモリ上限 512m、`oom_score_adj`、本番ならメモリの予約）を重ねて `config` → `build` / `pull` → `up --wait` → HTTP 確認（5xx 以外が返るまで最大 60 秒）を行う。
- 最初に Compose ファイルが参照する変数（`docker compose config --variables`）を出す。koroku が照合に使うのと同じ一覧なので、アプリが必要とする変数が並んでいるか確かめる。
- 変数を読む `build.args` を出す。ここに環境ごとの値があると、staging から本番への昇格でイメージを再利用できず、値がイメージに焼き込まれる。実行時に読む形へ直せないか確かめる。
- 失敗したらコンテナの出力の末尾を出す。終わると Compose プロジェクトを volume ごと消す（`--keep` で残す）。
- テスト用の env ファイルは一時ディレクトリに作り、本物の秘密を入れない。リポジトリには置かない。
- Docker が使えない環境では、スクリプトを動かさずに `config` の確認までにとどめ、そう報告する。

ポリシー検査（特権、ホストの名前空間、bind mount など）はこのスクリプトでは行わない（`--blue-green` のときの無停止デプロイの条件だけは調べる）。正規化した `config` を [references/requirements.md](references/requirements.md) の表と照らして確かめる。

### 5. 報告する

最後に次をまとめて利用者に渡す。koroku の「アプリ作成」と「設定」の画面にそのまま入れられる形にする。

| 項目 | 内容 |
| --- | --- |
| Compose ファイルのパス | 例 `compose.yaml` |
| 公開サービスとコンテナ内ポート | 例 `web` / `3000` |
| ヘルスチェックパス | 5xx 以外をすぐ返すパス。例 `/up` |
| 環境変数 | 名前と意味、必須か任意か、秘密かどうか、値の作り方（例 `openssl rand -hex 64`）。値そのものは書かない。Compose に `${VAR}` で宣言したことも書く（koroku の「設定」で照合元が「Compose ファイルの `${VAR}`」になっていれば、環境変数の画面に未設定のものが出る） |
| メモリ上限 | 512m を超えるサービスがあれば、その値と理由（アプリ設定で上げる） |
| メモリの予約（本番だけ） | 本番アプリで、上限の半分（既定）では足りない、または予約の要らないサービスがあれば、その値（`0` で予約しない）。koroku の「設定」タブで入れる。Compose に `mem_reservation` や `oom_score_adj` を書いても koroku が上書きする |
| コンテナレジストリの認証 | private なイメージ（CI がビルドしたイメージや private なベースイメージ）を使うなら、そのレジストリ（例 `ghcr.io`）と、読み取り専用のトークンの作り方（GHCR なら `read:packages` だけの classic な Personal access token か、読み取り専用のボットアカウント）。koroku の「設定」タブの「コンテナレジストリの認証」に入れる。トークンの値は書かない |
| 先に待つワークフロー | `image:` だけの Compose で、イメージを GitHub Actions がコミットごとにビルドするなら、そのワークフローのファイル（例 `.github/workflows/image.yml`。CI のあとに `on: workflow_run` でつなぐなら、最後にイメージを置くほう）。koroku の「設定」タブの自動デプロイの「先に待つワークフロー」に入れる。push で自動デプロイするときに、イメージができる前に pull して失敗しないようにするため |
| バックアップ | ダンプフック（サービス名とコマンド。例 `db` / `pg_dump -U postgres app`）と、ファイルのバックアップから外す volume（DB のデータ volume） |
| 本番の DB を staging へコピーするとき | ロードフック（例 `db` / `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"`）と、個人情報を消すコマンドがあればそのサービスとコマンド（`references/requirements.md` の「本番の DB を staging にコピーする」） |
| タスク | 管理画面に登録しておくと便利なコマンド（名前、サービス、コマンド。例 `マイグレーションの状態` / `web` / `bin/rails db:migrate:status`）。対話的なコマンドは使えない |
| 無停止デプロイ | 使うなら、`rehearse.sh --blue-green` の結果（条件を満たすか、切り替えの間に失敗したリクエストの数）と、旧色を止めるまでの待ち時間（既定 10 秒で足りなければその秒数。最大 300）。koroku のデプロイタブの「デプロイの設定」で有効にする |
| 独自ドメインを使うときの準備 | ホスト名の許可リストなど、アプリ側で設定する環境変数 |

あわせて、変えたファイルと理由、リハーサルの結果（実行したコマンドと出力の要点）、確かめられなかったことを書く。リハーサルしていないものを「動く」と書かない。

動くようになったあとでメモリやイメージの大きさを詰めたいときは、同じプラグインの tune-app Skill を使う（`rehearse.sh --keep` で起動したまま測れる）。
