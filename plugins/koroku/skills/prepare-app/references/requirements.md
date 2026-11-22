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
    oom_score_adj: -400      # staging は 500
    mem_reservation: 256m    # 本番だけ。staging はリポジトリの値も消す
  db:                        # ほかのサービス
    ports: !reset []
    env_file:
      - /var/lib/koroku/apps/<slug>/env
    restart: unless-stopped
    mem_limit: 512m
    oom_score_adj: -400
    mem_reservation: 256m
```

ここから分かる注意点:

| 点 | リポジトリ側でやること |
| --- | --- |
| 公開サービス以外の `ports` は消える | サービス同士はサービス名で通信する（`DATABASE_URL=postgres://...@db:5432/...`）。ホストの `localhost:5432` を前提にしない |
| 公開サービスの `ports` は置き換わる | アプリは `0.0.0.0` で待ち受ける（`127.0.0.1` だとコンテナの外から届かない）。ポート番号は `expose: ["3000"]` か `ports` に書いておくと、koroku の作成画面が読み取って初期値にする |
| env ファイルは全サービスに入る | 変数名がサービス間で衝突しないようにする。`POSTGRES_PASSWORD` のような変数は web にも見えるが、それで問題ない設計にする |
| `restart: unless-stopped` が付く | マイグレーションなど終わって欲しいワンショットのサービスには `restart: "no"` を書く（書かないと終了後に再起動を繰り返す）。さらに、そのサービスは必ず別のサービスから `depends_on: { <名前>: { condition: service_completed_successfully } }` で待たれるようにする。誰も待っていないワンショットのサービスが終了すると、終了コードが 0 でも `up --wait` が失敗し、デプロイが失敗する（下の「起動順」） |
| メモリ上限が付く | 既定 512m。リポジトリで `mem_limit` か `deploy.resources.limits.memory` を書けば、既定値以下なら尊重される。それより大きくしたいときは koroku のアプリ設定で上げる（リポジトリに書いても既定値で頭打ちになる） |
| `oom_score_adj` は koroku が決める | 本番アプリ（`environment = production`）は `-400`、staging は `500`。ホスト全体でメモリが尽きたとき、staging のコンテナから止まり、本番は最後になる。リポジトリに書いた値は上書きされ、ポリシー検査では拒否されない。環境を切り替えると次のデプロイから変わる |
| 本番はメモリを予約する | 本番アプリの各サービスに `mem_reservation`（cgroup の `memory.low`）が付き、メモリが足りないときに回収を後回しにされる。既定はそのサービスのメモリ上限の半分で、koroku のアプリ設定で全体とサービスごとに変えられる（`0` で予約しない）。上限を超える値は上限に切り詰められる。リポジトリの `deploy.resources` を使っていれば `deploy.resources.reservations.memory` に書かれる。リポジトリに書いた予約は使われない（本番は koroku の値、staging は消える） |
| `build:` のイメージ名は上書きされる | `image:` と `build:` を両方書いてもよいが、その名前のイメージは作られない。別サービスがそのイメージ名を参照する構成（ビルドを 1 回にして使い回す）は動かないので、各サービスに同じ `build:` を書く |

## 環境変数

- 値は koroku の画面で設定する。全サービスに `env_file` で入り、`${VAR}` の補間にも使われる。
- リポジトリの `.env` は補間に使われない。`${VAR:-既定値}` は koroku に `VAR` がなければ既定値になる。必須の変数は `${VAR:?VAR is required}` にすると、未設定のときに `config` の段階で分かりやすく失敗する。
- サービスが自分で `env_file: .env` と書いていれば、その中身はコンテナに入る（koroku の値が後ろに付くので、同じ名前なら koroku が勝つ）。ただしコミット済みの `.env` に秘密を書いてはいけない。
- Compose の `environment:` に直書きした値は `env_file` より強い。`RAILS_ENV: development` のように書いてあると koroku の画面で変えられないので、`RAILS_ENV: ${RAILS_ENV:-production}` のように補間にする。
- 補間をコンテナ側に遅らせたいときは `$$VAR` と書く。
- デプロイしたコミットは、koroku が足す `KOROKU_COMMIT_SHA`（40 桁）と `KOROKU_COMMIT_SHORT`（先頭 7 桁）で分かる。補間にも使えるので、CI がコミットごとにビルドしたイメージを `image: ghcr.io/<owner>/<name>:sha-${KOROKU_COMMIT_SHORT}` のように指せる（ホストでビルドせずに pull だけで済む）。`.git` は消えるので、ビルドや起動で `git rev-parse` などに頼らない。`KOROKU_` で始まる名前は予約されていて、画面からは設定できない。
- アプリの URL は、koroku が足す `KOROKU_APP_URL`（例 `https://blog-staging.dev.example.com`。プライマリの独自ドメインがあればその URL）と `KOROKU_APP_HOST`（そのホスト名）で分かる。値はデプロイした時点のもの。ホスト名をリポジトリや環境変数に直書きせず、これから組み立てる（Rails の `config.hosts << ENV["KOROKU_APP_HOST"]`、メールの中のリンクの `default_url_options`、`BASE_URL` の既定値など）。レビューアプリ（下記）はホスト名が作るまで決まらないので、直書きしていると開けない（Rails の `config.hosts` で弾かれる）。

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

## ビルド時と実行時の設定

staging と本番は同じリポジトリを別々の koroku アプリとして動かす。本番アプリに昇格元の staging アプリを設定すると、本番のデプロイ画面から「staging で稼働中のリリースを本番へ昇格」できる。昇格では、`build:` を持つサービスごとに次を比べ、同じなら staging でビルドしたイメージにタグを付け直して使い、ビルドしない。

- 同じコミット（ビルドコンテキストの中身が同じ）
- 正規化したビルド定義（`context`、`dockerfile`、`target`、`additional_contexts` など）
- **`build.args` の補間後の値**。`args: { ASSET_HOST: ${ASSET_HOST} }` のように環境変数から渡す引数は、staging と本番それぞれの環境変数で補間して比べる。違えばそのサービスは本番の値でビルドし直し、デプロイログと確認画面に違ったキーの名前（値は出さない）を出す。

つまり koroku が見つけられるのは `build.args` の違いまで。Dockerfile の `RUN` には koroku の env ファイルは渡らないので、イメージに環境ごとの値が入る道は `build.args` と、リポジトリにコミットしたファイルだけ。どちらにも環境ごとの値を置かないようにすると、昇格でビルドが省け、staging で確かめたものと同じイメージが本番で動く。

| よくある書き方 | 問題 | 直し方 |
| --- | --- | --- |
| Rails の `config.asset_host` をビルド時の値（`ARG ASSET_HOST` → `ENV`）で決め、`assets:precompile` で焼き込む | 本番でも staging の CDN を指す（koroku は引数の違いを見つけてビルドし直すが、ビルドは省けない） | `asset_host` は実行時に読む（`config.asset_host = ENV["ASSET_HOST"]` を production の設定に置き、`assets:precompile` の結果に URL を含めない）。プリコンパイルには `SECRET_KEY_BASE_DUMMY=1` など環境に依存しない値だけを渡す |
| `.erb` のアセット（`application.js.erb` など）で `ENV` や環境ごとの設定を読む | ビルド時の値が焼き込まれる | `.erb` で環境ごとの値を読まない。値はレイアウトの `<meta>` や `data-*` 属性に実行時に書き出し、JS から読む |
| JS のバンドラーが `process.env.API_URL` などをビルド時に埋め込む（Vite の `import.meta.env`、webpack の `DefinePlugin`、Next.js の `NEXT_PUBLIC_*` など） | ビルドした環境の値がバンドルに残る | 環境ごとの値はサーバーがページに実行時に書き出す（`<meta name="api-url" content="...">`、`window.__CONFIG__` を返すエンドポイントなど）。バンドルに埋め込むのは環境に依存しない値だけ |
| `build.args` に `RAILS_ENV` や `NODE_ENV` を環境変数から渡す | staging と本番で値が違えば毎回ビルドし直しになる | staging も本番も同じ値（`production`）でビルドする。環境の違いは実行時の環境変数で表す |

- 環境ごとの値を `build.args` に渡していても壊れはしない（koroku が違いを見つけてビルドし直す）。ただ、ビルドの時間と負荷がかかり、staging で確かめたイメージとは別物になる。
- `build.args` に固定値（`NODE_VERSION: "22"` など）を書くのは問題ない。両方で同じ値になる。

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
| private なイメージ | アプリごとの「コンテナレジストリの認証」（koroku の設定タブ。レジストリ、ユーザー名、トークン）で取得する。`image:` のイメージの pull にも、Dockerfile の `FROM` に書いた private なベースイメージのビルドにも使われ、ほかのアプリとは混ざらない。トークンは読み取りだけができるもの（GHCR なら `read:packages` だけの classic な Personal access token か、読み取り専用のボットアカウントのトークン）にする。設定できるのは、そのアプリの「管理」以上のロールを持つ人。1 アプリに 1 つなので、private なレジストリを 2 つ使うアプリは 1 つにまとめる（認証を設定したアプリは、ホストの root の `docker login` を見ない）。認証がないか、トークンが切れている・権限が足りないと、デプロイログの `build`・`pull`・`start` の手順に `The registry refused access` と出て失敗し、稼働中の版はそのまま。CI がビルドしたイメージを使うなら、そのコミットのイメージができてからデプロイする（`sha-${KOROKU_COMMIT_SHORT}` のタグがまだなければ pull で失敗する）。push での自動デプロイを使うなら、koroku の設定タブの「先に待つワークフロー」にイメージを作るワークフローのファイル（例 `.github/workflows/image.yml`）を入れる。koroku は push ではなく、そのワークフローがブランチの最新のコミットで成功したときにデプロイする。ワークフローは `.github/workflows/` の直下に置き、追うブランチへの push のたびに走らせる（`paths` で絞ると、走らなかった push は自動では出ない）。CI のあとに `on: workflow_run` で別のワークフローがイメージを作るなら、そちらを入れる（成功したときの `head_sha` は元の push のコミット）。失敗・取り消しの実行ではデプロイせず、koroku の操作の記録に残る。手元のリハーサル（`scripts/rehearse.sh`）は手元の Docker のログイン（`docker login`）で pull する |
| 自前のリバースプロキシ | アプリの中に Caddy や nginx を置いて振り分けている場合、TLS は koroku の Caddy が終端するので、アプリ側は平文の HTTP で待ち受け、証明書の取得をやめる（Caddy なら `auto_https off` と `:8080` のようなサイト）。前段の koroku からの `X-Forwarded-For` / `X-Forwarded-Proto` を信頼する設定（Caddy の `trusted_proxies static private_ranges`、nginx の `set_real_ip_from`）がないと、利用者の IP が全員同じ（前段のアドレス）に見え、IP ごとの回数制限が全員まとめてかかる。Caddy の回数制限やログは `{remote_host}` ではなく `{client_ip}` を使う |
| `memswap_limit` | swap を禁じるために `mem_limit` と同じ値を書いてよい。koroku の画面でメモリ上限を上げると、koroku が `memswap_limit` も同じ値に上げる |
| サービス名の `-数字` | ログはコンテナ名からサービス名を求める。サービス名は英小文字と `-` / `_` にしておくと扱いやすい |

## アクセスの保護

koroku はアプリ単位で「管理画面にログインしている人だけが開ける」ようにできる。staging のアプリ（`environment = staging`）は最初から有効、本番は無効で、あとから画面で切り替えられる。アプリには手を入れない（koroku の前段の Caddy が止める）。

| 点 | 内容と対処 |
| --- | --- |
| ブラウザー以外は通れない | 保護したアプリには、ログインできない呼び出し元（GitHub や決済サービスの Webhook、外部の死活監視、モバイルアプリ、ほかのサービスからの API 呼び出し）が届かない。`GET` は管理画面へ 302、それ以外は 401 になる。受け口のパスを洗い出し、利用者に koroku の設定画面の「保護を外すパス」へ登録してもらう |
| 外すパスの書き方 | 前方一致で、大文字と小文字を区別しない。ディレクトリは末尾を `/` にする（`/webhooks` だと `/webhooks-admin` も外れる）。英数字と `. _ ~ - / @ : +` だけで、`*` や `%` は使えない。20 件まで。受け口を 1 つのディレクトリ（`/webhooks/` など）にまとめておくと登録が少なくて済む |
| 外したパスの認証 | 外したパスは誰でも呼べる。Webhook の署名の検証など、アプリ自身で確かめる |
| `/.koroku/` | koroku のログインに使う予約パス。保護したアプリでは、このパスへのリクエストはアプリに届かない。アプリで使わない |
| Cookie | ログインを示す `__Host-koroku_gate` の Cookie は、Caddy がアプリへ転送する前に取り除く。アプリの Cookie はそのまま届く |
| デプロイのヘルスチェック | koroku はループバックの割当ポートへ直接確かめるので、保護の影響を受けない |
| 検索エンジン | staging のアプリの応答には、保護の有無にかかわらず `X-Robots-Tag: noindex` が付く |

## アクセスがない間の休止

koroku はアプリ単位で「アクセスがない間は止める」ようにできる（既定は無効。たまにしか開かない staging のメモリを空けるため）。有効にすると、決めた時間（15 分〜3 時間など）Caddy を通るリクエストがなければ `docker compose stop` でプロジェクトの全サービスを止め、次のリクエストで `docker compose start --wait` して、HTTP の確認が通ってからそのリクエストに応える。リクエストは最長 60 秒待ち、それを超えると 503 と `Retry-After: 5`（ブラウザーには自動で読み直す「起動しています」のページ）が返る。

| 点 | 内容と対処 |
| --- | --- |
| 止めて起こしても壊れない | コンテナは消さずに止め、同じコンテナを起動し直す（イメージ、環境変数、volume は同じ）。メモリ上のキャッシュやセッション、実行中のジョブは消える。続きが要る状態は DB や volume に置く。`SIGTERM` で行儀よく止まる（`stop` は既定 10 秒待ってから強制終了する） |
| 最初のリクエストが待つ時間 | コンテナの起動、`healthcheck` が通るまで、アプリの準備（Rails なら 5〜15 秒ほど）の合計。起動を軽くする（起動時の重い処理をやめる、`bootsnap` を効かせる）と待ちが短くなる。`--sleep` を付けたリハーサルで秒数が分かる |
| ワンショットのサービス | 起こすたびに、ほかのサービスと一緒にもう一度走る（`start` は止まっているコンテナをすべて起動する）。マイグレーションなら、何度走っても害がない（`db:migrate` はそう）ようにしておく。誰からも待たれていないワンショットのサービスは、デプロイと同じく `start --wait` を失敗させる |
| DB の `healthcheck` | 休止中のアプリのバックアップでは、ダンプフックのサービスだけを `start --wait <サービス>` で起動してダンプを取り、また止める。`healthcheck` がないと、DB が接続を受け付ける前にダンプが始まって失敗しうる。DB のサービスに `healthcheck` を書く（起動順のためにも要る） |
| `restart: always` | 書かない（koroku は書かれていなければ `unless-stopped` を付ける）。`always` のサービスは、ホストや Docker の再起動のあと休止中でも勝手に起動し、メモリを空けられない |
| 休止しない呼び出し | 外部の死活監視、クローラー、開いたままのタブのポーリング、外部サービスからの定期的な Webhook も 1 回のアクセスになり、設定の時間より短い間隔で来ると休止しない。アクセスの保護を有効にしたアプリでは、ログインしていない呼び出し（管理画面への 302 と 401）は数えない |
| 起こさないもの | koroku のデプロイのヘルスチェック（ループバックの割当ポートへ直接行く）は数えない。`/.koroku/` 以下は休止中のアプリを起こさない |
| 定期実行（cron、ワーカー） | 休止の間はワーカーやスケジューラーのコンテナも止まり、アプリの中のスケジューラー（Solid Queue の `recurring.yml`、`sidekiq-cron`、cron のコンテナなど）は走らない。決まった時刻の処理は koroku の定期タスク（下の「ワンオフのコマンドとタスク」）に移し、ジョブをその場で実行する 1 回のコマンドにする（例: `bin/rails runner 'NightlyCleanupJob.perform_now'`）。`perform_later` のようにキューに入れるだけのコマンドは、ワーカーが止まっているので実行されない。タスクのサービスが DB を使うなら、そのサービスの `depends_on` に DB を書く（休止中は `depends_on` でたどれるサービスだけが起動する）。移せない処理がある（常に動いている必要がある）アプリでは休止を使わない |
| WebSocket と SSE | 休止と起こすたびに Caddy の設定が入れ替わり、アップグレード済みの接続は最長 5 分後に切れる（ほかのアプリの起動・停止と同じ）。クライアントは再接続できるようにする |

## ワンオフのコマンドとタスク

koroku の画面から、稼働中のリリースと同じイメージ・環境変数・メモリ上限で 1 回だけコマンドを実行できる（Heroku の `heroku run` に当たる）。よく使うものは名前を付けて「タスク」として登録しておける。

- 実行は `docker compose run --rm --no-deps -T <サービス> sh -c '<コマンド>'`。コンテナに `sh` が要る（ディストロレスのイメージでは使えない）。
- 新しいコンテナで動く。公開サービスのコンテナの中ではないので、そのコンテナが実行中に書いたファイルは見えない。DB などの依存サービスは起動しない（`--no-deps`）ので、稼働中のものにつながる。アクセスがない間に休止しているアプリは、手で実行するときは実行の前に koroku が起こす（定期タスクは下）。
- 端末も標準入力もない。`rails console` や対話モードの `psql` は使えない。入力を待たずに終わるコマンドにする（`bin/rails runner '...'`、`psql -c '...'` など）。
- ポートは公開されない。最長 30 分（ホストの設定で変わる）で打ち切られる。
- 出力は環境変数の値をマスクして記録されるが、コマンドの文字列そのものは画面と記録に残る。秘密はコマンドに直接書かず、環境変数で渡す。
- マイグレーションはデプロイのたびに走るよう Compose 側に書く（「検査ではないが動かなくなるもの」の起動順）。タスクは、状態の確認（`bin/rails db:migrate:status`）や、手で 1 回だけ行う作業に使う。
- 登録しておくと便利なタスクがあれば、報告の表に書く。

**定期タスク**: 登録したタスクに予定（cron の 5 つの欄: 分 時 日 月 曜日。例 `0 3 * * *`）を書くと、koroku がその時刻に実行する。時刻はホストの設定のタイムゾーン（既定 `Asia/Tokyo`）で読む。

- 実行のしかたは手で実行するときと同じ（新しいコンテナ、`--no-deps`、最長 30 分、出力のマスク）。履歴に「定期」と出る。
- アクセスがない間に休止しているアプリでは、アプリを起こさない。タスクのサービスの `depends_on` を推移的にたどったサービス（DB など）だけを起動してタスクを実行し、終わったら止め直す。`depends_on` に書いていないサービスは起動しないので、タスクのサービスから使う DB やキャッシュは `depends_on` に書く。DB には `healthcheck` と `condition: service_healthy` を書く（起動を待てるように）。
- 1 回で終わるコマンドにする。ジョブをキューに入れるだけのコマンド（`perform_later`、`enqueue`）は、休止中はワーカーが止まっているので実行されない。`bin/rails runner 'SomeJob.perform_now'` のようにその場で実行する。
- ホスト全体で 1 件ずつ順に実行する。前の回が終わっていなければその回は見送り、アプリが停止中なら実行しない。`koroku` が止まっていた間の予定は、あとから実行しない。何度実行しても困らない処理にしておく。
- アプリの中のスケジューラーから移したら、そのスケジューラーのサービスや設定（`recurring.yml` の該当行など）を消す。両方に残すと二重に走る。

## バックアップ

- named volume は日次でファイルとしてバックアップされる。稼働中の DB のデータディレクトリをファイルで取っても整合しないので、DB は「ダンプフック」で取り、DB の volume はアプリ設定でファイルのバックアップから外す。
- ダンプフックは `docker compose exec -T <サービス> sh -c '<コマンド>'` で実行され、標準出力が保存される。コンテナに `sh` が要る。出力が空なら失敗扱い。
  - PostgreSQL: `pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"`（`-Fc` を付けると `pg_restore` 用の形式）
  - MySQL: `mysqldump -u root -p"$MYSQL_ROOT_PASSWORD" --single-transaction "$MYSQL_DATABASE"`
  - SQLite（アプリのコンテナにある場合）: `sqlite3 /data/app.db .dump`（イメージに `sqlite3` が要る）
- ダンプの流し込みは手動。リストアの手順を README などに残しておくとよい。

## pull request ごとのレビューアプリ

koroku は、staging アプリを親にして、pull request ごとに別のアプリ（レビューアプリ）を作れる。ホスト名は `<親の slug>-pr-<番号>.<ベースドメイン>`、設定と環境変数は親の写しで、pull request のブランチへの push で自動でデプロイし直す。リポジトリ側では次を揃える。

- **ホスト名を直書きしない**。`KOROKU_APP_URL` と `KOROKU_APP_HOST`（上の「環境変数」）から、許可するホスト（Rails の `config.hosts`、Django の `ALLOWED_HOSTS`）、メールやリダイレクトの URL の土台、CORS の許可元を組み立てる。
- **レビューアプリかどうかは `KOROKU_PR_NUMBER` で分かる**（レビューアプリにだけ入る）。環境変数は親（staging）の値がそのまま写るので、外へ届くもの（メールの送信、外部サービスへの Webhook、決済、SMS）は、この変数があるときに止めるか、送り先をテスト用に切り替える書き方にする。画面に「PR #12 のレビュー環境」と出すのにも使える。
- **外部サービスのコールバックは届かないことがある**。OAuth のリダイレクト URL や Webhook の送り先は、外部サービスの側に登録したホスト名（staging）のまま。レビューアプリで確かめたいなら、利用者がレビューアプリの環境変数タブでテスト用の値に直す。
- **データ**: 作るときに「親のデータをコピーする」を選ぶと、最初のデプロイが成功したあとで、親のダンプフックの出力をレビューアプリのロードフックへ流し、コピー後のコマンドを実行する（下の「本番の DB を staging にコピーする」と同じ仕組み。フックは親の設定が写る）。親にダンプフックかロードフックがなければ空の DB で始まる。そのときに要るデータ（初期のユーザーなど）は、起動時のワンショットのサービス（`db:prepare` と `db:seed` を冪等に流すなど）で入れる。
- **起動のたびに動くマイグレーション**は、親のスキーマにレビュー中のブランチのマイグレーションを当てることになる。コピーのあとはサービスが起動し直すので、マイグレーションは起動時に流す作り（ワンショットのサービスか起動コマンド）にしておく。
- レビューアプリは日次のバックアップの対象外で、不要になったら利用者が手で消す（volume も消える）。手元のリハーサルでは `scripts/rehearse.sh --pr <番号>` で `KOROKU_PR_NUMBER` を入れて確かめられる。

## 本番の DB を staging にコピーする

koroku は、本番アプリの DB を staging アプリへ流し込める（コピー元のダンプフックの出力を、コピー先の「ロードフック」の標準入力へつなぐ）。使うなら、リポジトリ側で次を揃える。

- コピー元（本番）にダンプフック、コピー先（staging）にロードフックを設定する。どちらも `docker compose exec -T <サービス> sh -c '<コマンド>'` で実行される。画面の「種類から入力」に同じ既定値がある。
  - PostgreSQL: ダンプ `pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"`、ロード `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"`（ダンプは `-Fc` の形式にする）
  - MySQL: ダンプ `mysqldump --single-transaction --routines --triggers -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"`、ロード `mysql -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"`
  - MariaDB: `mariadb-dump` と `mariadb`（変数は `MARIADB_*`）
  - SQLite: ダンプ `sqlite3 /data/app.sqlite3 .dump`、ロードは新しいファイルに読ませてから置き換える（`set -e; rm -f /data/app.sqlite3.load; sqlite3 /data/app.sqlite3.load; mv /data/app.sqlite3.load /data/app.sqlite3`）。ロードフックのサービスは止まらないので、コピーのあとアプリを再起動する。
- ダンプとロードは各コンテナの中のクライアントで動く。**本番と staging の DB のメジャーバージョンを揃える**（同じ Compose ファイルなら揃う）。
- ロードの間、koroku はロードフックのサービス以外を止め、終わったら起動する。アプリは DB の中身が入れ替わることに耐える必要はないが、起動時に DB のマイグレーションを流すなら、それは本番のスキーマのまま動く。
- 個人情報を消すコマンド（例 `bin/rails db:anonymize`）をリポジトリに用意しておくと、「コピー後のコマンド」に設定できる。止めたサービスで設定すると、公開する前に 1 回限りのコンテナ（`docker compose run --rm --no-deps`）で実行される。失敗するとサービスは止めたままになるので、冪等で、DB に接続できれば完了する作りにする。
- アップロードされたファイルなど、DB 以外のデータは named volume に置けばそのままバックアップされる。
