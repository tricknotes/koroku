# koroku がリポジトリに求めること

koroku の設計（`docs/ARCHITECTURE.md` の 6〜8 章と 10 章）から、デプロイされる側に関係するところだけを抜き出したもの。koroku 本体のポリシー検査や override の生成を変えたら、この文書も合わせて直す。

## デプロイで koroku がすること

1. 指定したコミットを `git fetch --depth 1` で取得し、`.git` を消す。submodule と Git LFS のファイルは取得しない。
2. 画面で設定した環境変数を env ファイルに書き、override を生成する。
3. `docker compose -p koroku-<slug> -f <Compose ファイル> -f koroku.override.yaml --env-file <env ファイル> config` で正規化し、ポリシー検査をかける。
4. `build:` を持つサービスはホスト（x86_64）でビルドし、`image:` だけのサービスは pull する。
5. `up -d --remove-orphans --wait --wait-timeout 300` で起動する。
6. `http://127.0.0.1:<割当ポート><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒待つ。
7. 手順 5 以降で失敗したら、直前に成功したリリースへ自動で戻す（DB のマイグレーションは戻さない）。

前段の Caddy が `https://<slug>.<ベースドメイン>`（と独自ドメイン）で TLS を終端し、`127.0.0.1:<割当ポート>` へ転送する。転送時の Host ヘッダーは利用者がアクセスしたホスト名のままで、`X-Forwarded-Proto: https` が付く。

## override の中身

koroku はリポジトリの Compose ファイルを書き換えず、次の override を重ねる。

```yaml
services:
  web:                       # 公開サービス
    image: koroku-<slug>-web:r<デプロイ ID>   # build: を持つサービスだけ
    ports: !override
      - "127.0.0.1:<割当ポート>:<コンテナ内ポート>"
    env_file:
      - /var/lib/koroku/apps/<slug>/env
    restart: unless-stopped  # リポジトリが restart を書いていないときだけ
    mem_limit: 512m
  db:                        # ほかのサービス
    ports: !reset []
    env_file:
      - /var/lib/koroku/apps/<slug>/env
    restart: unless-stopped
    mem_limit: 512m
```

ここから分かる注意点:

| 点 | リポジトリ側でやること |
| --- | --- |
| 公開サービス以外の `ports` は消える | サービス同士はサービス名で通信する（`DATABASE_URL=postgres://...@db:5432/...`）。ホストの `localhost:5432` を前提にしない |
| 公開サービスの `ports` は置き換わる | アプリは `0.0.0.0` で待ち受ける（`127.0.0.1` だとコンテナの外から届かない）。ポート番号は `expose: ["3000"]` か `ports` に書いておくと、koroku の作成画面が読み取って初期値にする |
| env ファイルは全サービスに入る | 変数名がサービス間で衝突しないようにする。`POSTGRES_PASSWORD` のような変数は web にも見えるが、それで問題ない設計にする |
| `restart: unless-stopped` が付く | マイグレーションなど終わって欲しいワンショットのサービスには `restart: "no"` を書く（書かないと終了後に再起動を繰り返す）。さらに、そのサービスは必ず別のサービスから `depends_on: { <名前>: { condition: service_completed_successfully } }` で待たれるようにする。誰も待っていないワンショットのサービスが終了すると、終了コードが 0 でも `up --wait` が失敗し、デプロイが失敗する（下の「起動順」） |
| メモリ上限が付く | 既定 512m。リポジトリで `mem_limit` か `deploy.resources.limits.memory` を書けば、既定値以下なら尊重される。それより大きくしたいときは koroku のアプリ設定で上げる（リポジトリに書いても既定値で頭打ちになる） |
| `build:` のイメージ名は上書きされる | `image:` と `build:` を両方書いてもよいが、その名前のイメージは作られない。別サービスがそのイメージ名を参照する構成（ビルドを 1 回にして使い回す）は動かないので、各サービスに同じ `build:` を書く |

## 環境変数

- 値は koroku の画面で設定する。全サービスに `env_file` で入り、`${VAR}` の補間にも使われる。
- リポジトリの `.env` は補間に使われない。`${VAR:-既定値}` は koroku に `VAR` がなければ既定値になる。必須の変数は `${VAR:?VAR is required}` にすると、未設定のときに `config` の段階で分かりやすく失敗する。
- サービスが自分で `env_file: .env` と書いていれば、その中身はコンテナに入る（koroku の値が後ろに付くので、同じ名前なら koroku が勝つ）。ただしコミット済みの `.env` に秘密を書いてはいけない。
- Compose の `environment:` に直書きした値は `env_file` より強い。`RAILS_ENV: development` のように書いてあると koroku の画面で変えられないので、`RAILS_ENV: ${RAILS_ENV:-production}` のように補間にする。
- 補間をコンテナ側に遅らせたいときは `$$VAR` と書く。
- デプロイしたコミットの SHA を受け取る仕組みはない。`.git` も消えるので、ビルドや起動で `git rev-parse` などに頼らない。

### 必要な変数の宣言（koroku が照合する）

koroku はアプリ設定の「必要な環境変数の照合」（既定は「Compose ファイルの `${VAR}`」）に従ってリポジトリから必要な変数を読み、画面に保存された変数と照らし合わせる。環境変数の画面には未設定の変数が「必須」「任意」で並び、デプロイの画面と本番の確認ダイアログには、選んだコミットで足りない必須の変数が警告として出る。デプロイは止めない。

| Compose の書き方 | koroku の扱い | 未設定のとき Compose は |
| --- | --- | --- |
| `${VAR:?説明}`、`${VAR?説明}` | 必須。`説明` を画面に出す | `config` で失敗する（デプロイは手順 4 で止まり、旧版が動き続ける） |
| `${VAR}`、`$VAR` | 必須 | 空文字に置き換えて起動する（警告だけ） |
| `${VAR:-既定値}`、`${VAR-既定値}` | 任意。既定値を画面に出す | 既定値を使う |
| `${VAR:-}` | 任意 | 空文字を使う |
| `${VAR:+値}`、`${VAR+値}` | 任意 | 空文字を使う |

勧める形:

```yaml
services:
  web:
    environment:
      SECRET_KEY_BASE: ${SECRET_KEY_BASE:?openssl rand -hex 64 で作る}
      DATABASE_URL: ${DATABASE_URL:?postgres://app:<password>@db:5432/app}
      SMTP_HOST: ${SMTP_HOST:?送信メールのサーバー}
      RAILS_MAX_THREADS: ${RAILS_MAX_THREADS:-5}
      SENTRY_DSN: ${SENTRY_DSN:-}
```

- **アプリのコードだけが読む変数は koroku から見えない**。koroku は env ファイルで全変数を全サービスに注入するので、Compose に書かなくてもアプリには届く。そのため Compose に現れず、照合の対象にならない。必要な変数は上のように Compose に並べる（`environment:` に同じ名前で書いても、値は koroku の画面のものになる）。
- `:?` の説明は画面に出る。値の例や作り方を書き、秘密そのものは書かない。
- 読まれるのは koroku に設定した Compose ファイル 1 つだけ。`include:` や `extends: { file: }` で別ファイルに書いた参照、`environment: [VAR]` のような値なしの受け渡し、リポジトリ内の `env_file:` の中身は読まれない。
- YAML のコメントの中の `${VAR}` は読まれない。`$$VAR`（コンテナ側で展開させる書き方）も対象外。
- `.env.example` を使いたいリポジトリは、アプリ設定で照合元を「.env.example」に切り替えられる（パスも変えられる）。この場合はキーをすべて必須として扱い、キーの直前のコメント行を説明として出す。必須と任意を分けたいなら Compose の `${VAR}` を使う。
- 照合元は利用者が koroku の画面（設定タブ）で選ぶ。リポジトリ側では決められない。

## ポリシー検査（違反するとデプロイを拒否）

正規化した `config` に対して検査する。アプリ設定の「検査を緩める」で個別に許可できる項目と、できない項目がある。

| ルール名 | 拒否する内容 | 直し方 | 緩められるか |
| --- | --- | --- | --- |
| `web_service_missing` | 画面で指定した公開サービスが Compose にない | サービス名を合わせる | 可 |
| `port_exposure` | 公開サービス以外のポート公開、`127.0.0.1:<割当ポート>` 以外への公開 | override が消すので、通常は気にしなくてよい。`network_mode: service:...` などで回避しない | **不可** |
| `privileged` | `privileged: true`、`build.privileged: true` | 外す | 可 |
| `host_network` | `network_mode: host`、`build.network: host` | 外す。ビルド中にホストへ届く必要があるならビルドの設計を変える | 可 |
| `host_pid` / `host_namespace` | `pid` / `ipc` / `uts` / `userns_mode` / `cgroup` の `host` | 外す | 可 |
| `foreign_namespace` | `network_mode` / `pid` / `ipc` / `volumes_from` の `container:<名前>` | 同じアプリ内なら `service:<名前>` を使う | 可 |
| `docker_socket` | `/var/run/docker.sock` のマウント | 外す。Docker を操作するアプリは koroku に載せない | 可 |
| `bind_outside_release` | リポジトリの外への bind mount（`../`、絶対パス、外を指すシンボリックリンク） | named volume にするか、リポジトリ内のファイルにする | 可 |
| `bind_writable` | `:ro` のない bind mount | 設定ファイルを渡す bind には `:ro` を付ける。書き込むデータは named volume に置く | 可 |
| `volume_host_path` | `driver_opts.device` にホストのパスを指定した named volume | 普通の named volume にする | 可 |
| `foreign_volume` / `foreign_network` | `external: true`、`name:` で名前を固定した volume / ネットワーク | `external` と `name:` を外す（プロジェクト名が接頭辞に付く） | 可 |
| `capability` | Docker の既定と `IPC_LOCK`・`SYS_NICE` 以外の `cap_add`（`SYS_ADMIN`、`NET_ADMIN` など） | 外す | 可 |
| `device` | `devices`、`device_cgroup_rules` | 外す | 可 |
| `security_opt` | `seccomp` / `apparmor` の `unconfined`、ラベルの無効化など | 外す | 可 |
| `runtime` | `runc` 以外の `runtime` | 外す | 可 |
| `memory_guard` | `oom_kill_disable: true`、`memswap_limit: -1` | 外す | 可 |
| `file_outside_release` | リポジトリ外の `env_file`、`secrets` / `configs` の `file`、ビルドコンテキスト、Dockerfile | リポジトリ内に置く。秘密は koroku の環境変数で渡す | 可 |

「検査を緩める」は 1 つのフラグでアプリ単位にしか切り替えられず、立てると上の保護がまとめて効かなくなる。勧めない。

## 検査ではないが動かなくなるもの

| 点 | 理由と直し方 |
| --- | --- |
| `container_name` | 同じリポジトリを staging と本番の 2 アプリで動かすと名前が衝突する。外す |
| ホスト名やドメインの直書き | 同じリポジトリが `<slug>-staging` と `<slug>` の両方で動き、独自ドメインも足せる。ホスト名は環境変数にする |
| 開発モード | koroku にアプリの開発モードはない。本番の設定で起動する Compose にする。開発用の設定（ソースの bind mount、ホットリロード、デバッグ用のポート）は `compose.override.yaml` に分けると、手元の `docker compose up` では自動で重なり、koroku では使われない（koroku は `-f` を明示するため）。`compose.override.yaml` を `.gitignore` するかコミットするかは利用者の流儀に合わせる |
| ログをファイルに書く | koroku は `docker compose logs` を集める。標準出力と標準エラーに出す（Rails なら `RAILS_LOG_TO_STDOUT=1`、Rails 7.1 以降は既定で標準出力） |
| アセットの実行時生成 | ソースを bind mount できないので、アセットのビルドはイメージのビルド時に行う |
| ヘルスチェックパスが重い・5xx を返す | 起動直後に DB などへ依存せず、すぐ 200 を返すパスを用意する（Rails 7.1 以降の `/up` など）。認証が要るパスでも、3xx や 401 は 5xx でないので通る |
| 起動に時間がかかる | `up --wait` は 300 秒、HTTP 確認は 60 秒で打ち切る。公開サービスに `healthcheck` を書いておくと、`up --wait` がそれを待つ。`healthcheck` がないと、起動直後に落ちて再起動を繰り返していても `up --wait` は成功し、HTTP 確認で初めて失敗が分かる |
| 起動順 | DB を待つには、DB に `healthcheck` を書き、依存側で `depends_on: { db: { condition: service_healthy } }`。マイグレーションをワンショットのサービスにするなら `restart: "no"` を書き、公開サービスから `condition: service_completed_successfully` で待つ。誰からも待たれていないワンショットのサービスは、正常に終了しても `up --wait` が `container ... exited (0)` で失敗する（Compose v5.8.0 で確認） |
| WebSocket | koroku が Caddy の設定を入れ替えると、アップグレード済みの接続は最長 5 分後に切れる。クライアントは再接続できるようにする |
| CPU アーキテクチャ | 本番は x86_64（`linux/amd64`）。pull するイメージは amd64 版が必要。Apple シリコンの手元で作ったイメージを push して使う場合も amd64 で作る |
| ビルドの重さ | ビルドは本番と同じホスト（目安 2 vCPU・4GB、swap 2GB）で、ほかのアプリが動いたまま行う。マルチステージにし、`.dockerignore` で `node_modules`・`tmp`・`log` などを外す |
| private なイメージ | koroku のホストで `docker login` してあることが前提。ホストの管理者に頼む |
| サービス名の `-数字` | ログはコンテナ名からサービス名を求める。サービス名は英小文字と `-` / `_` にしておくと扱いやすい |

## バックアップ

- named volume は日次でファイルとしてバックアップされる。稼働中の DB のデータディレクトリをファイルで取っても整合しないので、DB は「ダンプフック」で取り、DB の volume はアプリ設定でファイルのバックアップから外す。
- ダンプフックは `docker compose exec -T <サービス> sh -c '<コマンド>'` で実行され、標準出力が保存される。コンテナに `sh` が要る。出力が空なら失敗扱い。
  - PostgreSQL: `pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"`（`-Fc` を付けると `pg_restore` 用の形式）
  - MySQL: `mysqldump -u root -p"$MYSQL_ROOT_PASSWORD" --single-transaction "$MYSQL_DATABASE"`
  - SQLite（アプリのコンテナにある場合）: `sqlite3 /data/app.db .dump`（イメージに `sqlite3` が要る）
- ダンプの流し込みは手動。リストアの手順を README などに残しておくとよい。
- アップロードされたファイルなど、DB 以外のデータは named volume に置けばそのままバックアップされる。
