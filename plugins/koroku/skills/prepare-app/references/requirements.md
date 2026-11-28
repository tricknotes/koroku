# koroku がリポジトリに求めること

koroku の設計（`docs/ARCHITECTURE.md` の 6〜8 章と 10 章）から、デプロイされる側に関係するところだけを抜き出したもの。koroku 本体のポリシー検査や override の生成を変えたら、この文書も合わせて直す。

## デプロイで koroku がすること

1. 指定したコミットを `git fetch --depth 1` で取得し、`.git` を消す。submodule と Git LFS のファイルは取得しない。取得するのは、デプロイに指定したブランチかタグ（SHA だけの指定なら、koroku のアプリが追うブランチ）の先頭か、その祖先のコミットだけ。force-push でブランチから外れたコミットや、pull request にしかないコミットは、SHA を指定してもこの手順で失敗する。GitHub App で取得するアプリでは、取得の前に、リポジトリの名前が koroku のアプリに設定したリポジトリ（GitHub の ID で覚えている）をいまも指すことを確かめる。名前の変更と移譲では ID が変わらないので通る。消して同じ名前で作り直したリポジトリは ID が変わるので、この手順で失敗する（koroku のオーナーがアプリのリポジトリの ID を直す）。
2. 画面で設定した環境変数を env ファイルに、デプロイするコミット（`KOROKU_COMMIT_SHA`、`KOROKU_COMMIT_SHORT`）を別の env ファイル `env.release` に書き、override を生成する（下の「コミットの変数を受け取るサービス」）。
3. Compose ファイルと、そこから `include` / `extends.file` でたどれるファイルを読んで調べたうえで、`docker compose -p koroku-<slug> -f <Compose ファイル> -f koroku.override.yaml --env-file <env> --env-file <env.release> config` で正規化し、ポリシー検査をかける。Compose ファイルの `${VAR}` が読めるのは、koroku で設定した環境変数とコミットの変数（と `PATH`、`HOME` など動かすのに要るもの）だけ。
4. `build:` を持つサービスはホスト（x86_64）でビルドし、`image:` だけのサービスは pull する。
5. `up -d --pull missing --remove-orphans --wait --wait-timeout 300` で起動する。Compose は設定かイメージが変わったサービスだけを作り直す。公開サービス、ワンショット、`build:` を持つサービス（ワーカーや cron がアプリのイメージをビルドするならそれも）、コミットの変数を参照するサービスはデプロイのたびに作り直され、DB のようなほかのサービスは、アプリの環境変数かそのサービスの定義を変えたデプロイでだけ作り直される。動いていたコンテナは `SIGTERM` を受け、`stop_grace_period`（既定 10 秒）を過ぎると強制終了される（下の「デプロイのたびに止まる長いジョブ」）。
6. `http://127.0.0.1:<割当ポート><ヘルスチェックパス>` が 5xx 以外を返すまで最大 60 秒待つ。
7. 手順 5 以降で失敗したら、直前に成功したリリースへ自動で戻す（DB のマイグレーションは戻さない）。

前段の Caddy が `https://<slug>.<ベースドメイン>`（と独自ドメイン）で TLS を終端し、`127.0.0.1:<割当ポート>` へ転送する。転送時の Host ヘッダーは利用者がアクセスしたホスト名のままで、`X-Forwarded-Proto: https` が付く。

手順 5 でコンテナを作り直している間に来たリクエストは、Caddy が最長 30 秒手元に留め、新しい版が待ち受けたらそちらへ届ける（ロールバックと再起動でも同じ）。留められるのは、本文のない `GET` と、割当ポートに誰もいない間に届いたリクエストだけ。新しいコンテナの中でアプリが起動している間に来た `POST` などは 502 になる（Docker の `docker-proxy` が接続を受けてから閉じるので、Caddy はアプリが受け取ったかもしれないものとして送り直さない）。アプリの起動が短いほど、この間は短くなる。

## koroku に書く Compose

Compose ファイルは、リポジトリに置く代わりに koroku の画面に書いて持たせられる（アプリの作成画面か設定の「ソース」で、「Compose の置き場所」を「koroku に書く」にする）。Compose ファイルを持たないリポジトリや、置いてある Compose が開発用だったり下の検査を通らなかったりするリポジトリを、fork せずに動かすためのもの。

- koroku は手順 2 で、画面に書いた内容をチェックアウトの直下に `koroku.compose.yaml` として置き、これを Compose ファイルとして使う。リポジトリに同じ名前のファイルがあれば置き換える。リポジトリのほかの Compose ファイルは読まない（`include` や `extends.file` で名指しすれば読める）。
- 相対パス（`build: .`、`./config/app.conf:/etc/app.conf:ro`）は、リポジトリの直下から解決される。
- それ以外は、リポジトリの Compose ファイルと同じに扱われる。この文書の要件（ポート、環境変数、volume、ワンショット、ポリシー検査、無停止デプロイの条件）は、そのまま当てはまる。
- 秘密の値は書かない。koroku の環境変数に保存して `${VAR}` で参照する。必要な環境変数の照合は、書いた内容の `${VAR}` を読む。
- 書けるのは koroku のオーナーだけ。保存した内容は次のデプロイから使われ、動いている版と保持している版はデプロイした時点の内容で動く。
- 手元では `rehearse.sh --inline <ファイル>` で、同じ置き方でリハーサルできる。

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
      - /var/lib/koroku/apps/<slug>/env.release   # コミットの変数。受け取るサービスだけ
    restart: unless-stopped  # リポジトリが restart を書いていないときだけ
    mem_limit: 512m
    oom_score_adj: -400      # staging は 500
    mem_reservation: 256m    # 本番だけ。staging はリポジトリの値も消す
  db:                        # ほかのサービス（コミットの変数は受け取らない）
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
| env ファイルは全サービスに入る（`env.release` は一部だけ） | 変数名がサービス間で衝突しないようにする。`POSTGRES_PASSWORD` のような変数は web にも見えるが、それで問題ない設計にする |
| `restart: unless-stopped` が付く | マイグレーションなど終わって欲しいワンショットのサービスには `restart: "no"` を書く（書かないと終了後に再起動を繰り返す）。さらに、そのサービスは必ず別のサービスから `depends_on: { <名前>: { condition: service_completed_successfully } }` で待たれるようにする。誰も待っていないワンショットのサービスが終了すると、終了コードが 0 でも `up --wait` が失敗し、デプロイが失敗する（下の「起動順」） |
| メモリ上限が付く | 既定 512m。リポジトリで `mem_limit` か `deploy.resources.limits.memory` を書けば、既定値以下なら尊重される。それより大きくしたいときは koroku のアプリ設定で上げる（リポジトリに書いても既定値で頭打ちになる） |
| `oom_score_adj` は koroku が決める | 本番アプリ（`environment = production`）は `-400`、staging は `500`。ホスト全体でメモリが尽きたとき、staging のコンテナから止まり、本番は最後になる。リポジトリに書いた値は上書きされ、ポリシー検査では拒否されない。環境を切り替えると次のデプロイから変わる |
| 本番はメモリを予約する | 本番アプリの各サービスに `mem_reservation`（cgroup の `memory.low`）が付き、メモリが足りないときに回収を後回しにされる。既定はそのサービスのメモリ上限の半分で、koroku のアプリ設定で全体とサービスごとに変えられる（`0` で予約しない）。ほかのサービスが `depends_on` の `condition: service_completed_successfully` で待つワンショットのサービス（マイグレーションなど）には、設定によらず付かない（終わったあとは何も使わないため）。上限を超える値は上限に切り詰められる。リポジトリの `deploy.resources` を使っていれば `deploy.resources.reservations.memory` に書かれる。リポジトリに書いた予約は使われない（本番は koroku の値、staging は消える） |
| `build:` のイメージ名は上書きされる | `image:` と `build:` を両方書いてもよいが、その名前のイメージは作られない。別サービスがそのイメージ名を参照する構成（ビルドを 1 回にして使い回す）は動かないので、各サービスに同じ `build:` を書く |

## 環境変数

- 値は koroku の画面で設定する。全サービスに `env_file` で入り、`${VAR}` の補間にも使われる。koroku が足す変数のうちコミットの 2 つだけは、一部のサービスにしか入らない（下の「コミットの変数を受け取るサービス」）。
- リポジトリの `.env` は補間に使われない。`${VAR:-既定値}` は koroku に `VAR` がなければ既定値になる。必須の変数は `${VAR:?VAR is required}` にすると、未設定のときに `config` の段階で分かりやすく失敗する。
- サービスが自分で `env_file: .env` と書いていれば、その中身はコンテナに入る（koroku の値が後ろに付くので、同じ名前なら koroku が勝つ）。ただしコミット済みの `.env` に秘密を書いてはいけない。
- Compose の `environment:` に直書きした値は `env_file` より強い。`RAILS_ENV: development` のように書いてあると koroku の画面で変えられないので、`RAILS_ENV: ${RAILS_ENV:-production}` のように補間にする。
- 補間をコンテナ側に遅らせたいときは `$$VAR` と書く。
- デプロイしたコミットは、koroku が足す `KOROKU_COMMIT_SHA`（40 桁）と `KOROKU_COMMIT_SHORT`（先頭 7 桁）で分かる。補間にも使えるので、CI がコミットごとにビルドしたイメージを `image: ghcr.io/<owner>/<name>:sha-${KOROKU_COMMIT_SHORT}` のように指せる（ホストでビルドせずに pull だけで済む）。同じ Compose ファイルを koroku の外（手元など）でも使うなら、`image: ghcr.io/<owner>/<name>:${KOROKU_COMMIT_SHORT:+sha-}${KOROKU_COMMIT_SHORT:-latest}` と書くと、変数がないときは `latest` を指す（入れ子の既定値を使わないので、必要な変数の判定でも任意の変数として素直に読まれる）。タグを `docker/metadata-action` で付けるなら、`type=sha` は既定で `sha-` と 7 桁の短い SHA なので `KOROKU_COMMIT_SHORT` に、`type=sha,format=long` は 40 桁なので `KOROKU_COMMIT_SHA` に対応する（[type=sha](https://github.com/docker/metadata-action#typesha)）。例は [examples.md](examples.md) の「CI がビルドしたイメージを使う」。CI のイメージを指すサービスに `pull_policy: always` は要らない（koroku は `image:` だけのサービスをデプロイのたびに pull する）。書いてあっても、起動・ロールバックの `up` は `--pull missing` で呼ばれ、ホストにあるイメージをそのまま使う（レジストリに届かなくても前の版に戻せるようにするため）。`.git` は消えるので、ビルドや起動で `git rev-parse` などに頼らない。`KOROKU_` で始まる名前は予約されていて、画面からは設定できない。
- アプリの URL は、koroku が足す `KOROKU_APP_URL`（例 `https://blog-staging.dev.example.com`。プライマリの独自ドメインがあればその URL）と `KOROKU_APP_HOST`（そのホスト名）で分かる。値はデプロイした時点のもの。ホスト名をリポジトリや環境変数に直書きせず、これから組み立てる（Rails の `config.hosts << ENV["KOROKU_APP_HOST"]`、メールの中のリンクの `default_url_options`、`BASE_URL` の既定値など）。レビューアプリ（下記）はホスト名が作るまで決まらないので、直書きしていると開けない（Rails の `config.hosts` で弾かれる）。

### コミットの変数を受け取るサービス

`KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` はデプロイのたびに変わるので、koroku はこの 2 つを別の env ファイル（`env.release`）に書き、デプロイのたびにどのみち作り直されるサービスにだけ `env_file` で渡す。全サービスに渡すと、コードだけを変えたデプロイでも DB の設定が変わり、毎回作り直されて数秒止まるため。無停止デプロイ（下記）を使うかどうかによらず同じ。

受け取るのは次のサービス:

- 公開サービス（無停止デプロイでは `web-blue` / `web-green` と元の `web` も）
- ワンショットのサービス（ほかのサービスが `condition: service_completed_successfully` で待つもの）
- `build:` を持つサービス
- Compose の定義がコミットの変数で変わるサービス。`image: ...:sha-${KOROKU_COMMIT_SHORT}` や `environment: { KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA} }` のように補間で参照するもの。koroku はコミットの変数だけをすべて 0 にして Compose をもう一度正規化し、定義が変わるかで見分ける。見つかれば、デプロイログの `prepare` の手順に `Services that follow the commit: <サービス, ...>` と出る

ここから分かること:

- **DB、Redis などの `image:` だけのサービスは、この 2 つの変数を受け取らない**。中で読んでも空になる。代わりに、アプリの環境変数を変えていないデプロイでは作り直されない。
- **`build:` を持たないサービスでコミットを読みたいとき**（エラー報告のリリース名を付けるサイドカー、CI のイメージを使うワーカーなど）は、`environment` で補間して参照する。`image:` に書いていればそれで足りる。

  ```yaml
  services:
    reporter:
      image: example/reporter:1
      environment:
        KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA}
  ```

  値は補間でコンテナに届き、上の判定にも当たるので `KOROKU_COMMIT_SHORT` も届く。そのサービスはデプロイのたびに作り直される（コミットが変わると定義が変わるので）。値は `${KOROKU_COMMIT_SHA}` の形で書く。
- **アプリの環境変数を変えたデプロイでは、DB などのサービスも作り直す**（どのサービスがどの変数を読むかを koroku は知らないため）。デプロイログの起動の手順（無停止デプロイでは `services`）に `Recreating <サービス> because the app's env file changed (<変数の名前>, ...); requests may fail while it restarts.` と出る。並ぶのは変わった変数の名前だけ（値は出ない）。
- koroku を更新して最初のデプロイでは、それまでコミットの変数を受け取っていた DB などが一度だけ作り直され、ログの変数の名前に `KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` が出る。

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

- **アプリのコードだけが読む変数は koroku から見えない**。koroku は env ファイルでアプリの全変数を全サービスに注入するので、Compose に書かなくてもアプリには届く。そのため Compose に現れず、照合の対象にならない。必要な変数は上のように Compose に並べる（`environment:` に同じ名前で書いても、値は koroku の画面のものになる）。
- `:?` の説明は画面に出る。値の例や作り方を書き、秘密そのものは書かない。
- 読まれるのは koroku に設定した Compose ファイル 1 つだけ。`include:` や `extends: { file: }` で別ファイルに書いた参照、`environment: [VAR]` のような値なしの受け渡し、リポジトリ内の `env_file:` の中身は読まれない。
- YAML のコメントの中の `${VAR}` は読まれない。`$$VAR`（コンテナ側で展開させる書き方）も対象外。
- `.env.example` を使いたいリポジトリは、アプリ設定で照合元を「.env.example」に切り替えられる（パスも変えられる）。この場合はキーをすべて必須として扱い、キーの直前のコメント行を説明として出す。必須と任意を分けたいなら Compose の `${VAR}` を使う。
- 照合元は利用者が koroku の画面（環境変数タブの照合の欄）で選ぶ。リポジトリ側では決められない。

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
| `foreign_namespace` | `network_mode` / `pid` / `ipc` / `volumes_from` の `container:<名前>`。`pid` / `ipc` / `uts` / `userns_mode` / `cgroup` の、アプリの中で完結しない値（使えるのは `ipc` の `none`・`private`・`shareable`、`cgroup` の `private`、`pid` / `ipc` の `service:<名前>`） | 同じアプリ内なら `service:<名前>` を使う | 可 |
| `docker_socket` | `/var/run/docker.sock` のマウント | 外す。Docker を操作するアプリは koroku に載せない | 可 |
| `bind_outside_release` | リポジトリの外への bind mount（`../`、絶対パス、外を指すシンボリックリンク） | named volume にするか、リポジトリ内のファイルにする | 可 |
| `bind_writable` | `:ro` のない bind mount | 設定ファイルを渡す bind には `:ro` を付ける。書き込むデータは named volume に置く | 可 |
| `volume_host_path` | `driver_opts.device` にホストのパスを指定した named volume。`driver_opts.type` が `nfs`・`nfs4`・`tmpfs` 以外（`none` + `o: bind`、`overlay` など） | 普通の named volume にする | 可 |
| `foreign_volume` / `foreign_network` | `external: true`、`name:` で名前を固定した volume / ネットワーク。`network_mode` に書いたネットワーク名や `bridge`（使えるのは `none` と `service:<名前>`） | `external` と `name:` を外す（プロジェクト名が接頭辞に付く）。ネットワークは `networks:` に書く | 可 |
| `capability` | Docker の既定と `IPC_LOCK`・`SYS_NICE` 以外の `cap_add`（`SYS_ADMIN`、`NET_ADMIN` など） | 外す | 可 |
| `device` | `devices`、`device_cgroup_rules` | 外す | 可 |
| `security_opt` | `no-new-privileges` 以外の `security_opt`（`seccomp` / `apparmor` の `unconfined`、ラベルの無効化、`seccomp=<ファイル>` など） | 外す | 可 |
| `runtime` | `runc` 以外の `runtime` | 外す | 可 |
| `memory_guard` | `oom_kill_disable: true`、`memswap_limit: -1` | 外す | 可 |
| `stop_grace_period` | 全サービスの `stop_grace_period` の合計が 10 分を超える（Compose は依存の順に 1 サービスずつ止めるので、待ち時間が足し合わさる） | 長い猶予が要るサービスだけに付け、合計を 10 分以内にする。それより長いジョブは途中で止まっても続きからやり直せるようにする | **不可** |
| `file_outside_release` | リポジトリ外の `env_file`、`secrets` / `configs` の `file`、ビルドコンテキスト、Dockerfile（`dockerfile: ../../...` のように相対パスで外へ出るものを含む）、`include` と `extends.file` のファイル | リポジトリ内に置く。秘密は koroku の環境変数で渡す | 可 |
| `foreign_image` | `build:` のないサービスの `image:` が `koroku-` で始まる（koroku がビルドしたイメージの名前）。同じアプリのサービスのイメージは除く | レジストリのイメージを指すか、`build:` を書く | 可 |
| `unsupported_setting` | koroku が許可する設定の一覧にない設定と、許可していない値（下の「許可していない設定」） | 外す。開発用にだけ要るなら、手元用の別の Compose ファイルに移す | 可 |
| `blue_green` | 無停止デプロイを有効にしたアプリだけ: 公開サービスの `container_name`、`deploy.replicas`（か `scale`）が 2 以上、公開サービスがワンショット、ほかのサービスから公開サービスへの `depends_on` や `network_mode` / `ipc` / `pid` / `volumes_from` の `service:<公開サービス>`、`<公開サービス>-blue` / `<公開サービス>-green` という名前のサービス、公開サービスが `include:` した別ファイルにある | 下の「無停止デプロイ」 | **不可** |

「検査を緩める」は 1 つのフラグでアプリ単位にしか切り替えられず、立てると上の保護がまとめて効かなくなる。勧めない。

### 許可していない設定（`unsupported_setting`）

koroku は、正規化した結果を「許可する設定の一覧」と突き合わせ、一覧にない設定があるとデプロイを拒否する。危険な設定だけを拒否するのではなく、確かめた設定だけを通す。エラーは `service "web": build.tags is not a setting koroku allows` のように設定の名前を示す。

普通のアプリが使う設定（`image`、`build` の `context`・`dockerfile`・`target`・`args`、`command`、`environment`、`env_file`、`depends_on`、`healthcheck`、`volumes`、`networks`、`ports`、`restart`、`user`、`tmpfs`、`cap_drop`、`ulimits`、`deploy.resources` の `cpus`・`memory` など）は一覧にある。次は一覧にない。

| 場所 | 一覧にない設定 |
| --- | --- |
| サービス | `provider`、`use_api_socket`、`gpus`、`models`、`cgroup_parent`、`external_links`、`label_file`、`credential_spec`、`blkio_config`、`isolation` |
| `build` | `tags`（イメージの名前は koroku が決める）、`ssh`、`entitlements`、`isolation` |
| `post_start` / `pre_stop` | `privileged`（`command`、`user`、`working_dir`、`environment` は使える） |
| `deploy.resources.reservations` | `devices`、`generic_resources` |
| `volumes` の各項目 | `bind.propagation` |
| ネットワークの定義 | `driver_opts` |
| secret / config の定義 | `driver`、`template_driver` |
| トップレベル | `models` |

一覧にある設定でも、次の値は同じルールで拒否する。

- `volumes` の `type` が `bind`・`volume`・`tmpfs` 以外（`image` など）。
- volume の `driver` が `local` 以外、ネットワークの `driver` が `bridge` 以外。
- `logging.driver` が `json-file`・`local`・`none` 以外（koroku は Docker からログを読む）。
- `build.cache_from` / `build.cache_to` の `type=local`、`build.network` の `default`・`none` 以外。

値が空の設定（`use_api_socket: false`、`tags: []` など）と、`x-` で始まる拡張の項目は拒否しない。

### `include` と `extends.file`

koroku は、Compose に読ませる前に、Compose ファイルと、そこから `include` / `extends.file` でたどれるファイルを自分で読んで調べる。

- 使えるのは、リポジトリの中のファイルを指す、そのままのパス（`include: [infra/db.yaml]`、`extends: { file: common/base.yaml, service: base }`）。
- `include` の `env_file` と `project_directory` は使えない（`unsupported_setting`）。`include` したファイルの `${VAR}` も、アプリの環境変数で補間される。
- リポジトリの外のファイル（`../`、絶対パス、外を指すシンボリックリンク）は `file_outside_release`。
- Git や OCI のアドレス（`https://...`、`git@...:`、`oci://...`）、`${VAR}` を含むパス、ディレクトリを指すパスは `unsupported_setting`。
- YAML として読めないファイル（キーの重複など）は `unsupported_setting`。

## 検査ではないが動かなくなるもの

| 点 | 理由と直し方 |
| --- | --- |
| `container_name` | 同じリポジトリを staging と本番の 2 アプリで動かすと名前が衝突する。外す |
| ホスト名やドメインの直書き | 同じリポジトリが `<slug>-staging` と `<slug>` の両方で動き、独自ドメインも足せる。ホスト名は環境変数にする |
| 開発モード | koroku にアプリの開発モードはない。本番の設定で起動する Compose にする。開発用の設定（ソースの bind mount、ホットリロード、デバッグ用のポート）は `compose.override.yaml` に分けると、手元の `docker compose up` では自動で重なり、koroku では使われない（koroku は `-f` を明示するため）。`compose.override.yaml` を `.gitignore` するかコミットするかは利用者の流儀に合わせる |
| ログをファイルに書く | koroku は `docker compose logs` を集める。標準出力と標準エラーに出す（Rails なら `RAILS_LOG_TO_STDOUT=1`、Rails 7.1 以降は既定で標準出力） |
| 大量のログ | koroku が残すのは、1 サービスにつき 1 日 100 MiB まで（ホストの設定で変わる）。超えた分は日付が変わるまで残らず、ログに `[koroku] The log of this service reached ...` の行が 1 行入る。本番ではログレベルを `info` 以上にし、リクエストごとの本文やデバッグ出力を標準出力に流さない。1 行は 16 KiB で切られる |
| アセットの実行時生成 | ソースを bind mount できないので、アセットのビルドはイメージのビルド時に行う |
| ヘルスチェックパスが重い・5xx を返す | 起動直後に DB などへ依存せず、すぐ 200 を返すパスを用意する（Rails 7.1 以降の `/up` など）。認証が要るパスでも、3xx や 401 は 5xx でないので通る |
| 起動に時間がかかる | `up --wait` は 300 秒、HTTP 確認は 60 秒で打ち切る。公開サービスに `healthcheck` を書いておくと、`up --wait` がそれを待つ。`healthcheck` がないと、起動直後に落ちて再起動を繰り返していても `up --wait` は成功し、HTTP 確認で初めて失敗が分かる |
| 起動順 | DB を待つには、DB に `healthcheck` を書き、依存側で `depends_on: { db: { condition: service_healthy } }`。マイグレーションをワンショットのサービスにするなら `restart: "no"` を書き、公開サービスから `condition: service_completed_successfully` で待つ。誰からも待たれていないワンショットのサービスは、正常に終了しても `up --wait` が `container ... exited (0)` で失敗する（Compose v5.8.0 で確認） |
| WebSocket | koroku が Caddy の設定を入れ替えると、アップグレード済みの接続は最長 5 分後に切れる。クライアントは再接続できるようにする |
| CPU アーキテクチャ | 本番は x86_64（`linux/amd64`）。pull するイメージは amd64 版が必要。Apple シリコンの手元で作ったイメージを push して使う場合も amd64 で作る |
| ビルドの重さ | ビルドは本番と同じホスト（目安 2 vCPU・4GB、swap 2GB）で、ほかのアプリが動いたまま行う。マルチステージにし、`.dockerignore` で `node_modules`・`tmp`・`log` などを外す |
| private なイメージ | アプリごとの「コンテナレジストリの認証」（koroku の設定タブ。レジストリ、ユーザー名、トークン）で取得する。`image:` のイメージの pull にも、Dockerfile の `FROM` に書いた private なベースイメージのビルドにも使われ、ほかのアプリとは混ざらない。トークンは読み取りだけができるもの（GHCR なら `read:packages` だけの classic な Personal access token か、読み取り専用のボットアカウントのトークン）にする。設定できるのは、そのアプリの「管理」以上のロールを持つ人。1 アプリに 1 つなので、private なレジストリを 2 つ使うアプリは 1 つにまとめる（認証を設定したアプリは、ホストの root の `docker login` を見ない）。認証がないか、トークンが切れている・権限が足りないと、デプロイログの `build`・`pull`・`start` の手順に `The registry refused access` と出て失敗し、稼働中の版はそのまま。CI がビルドしたイメージを使うなら、そのコミットのイメージができてからデプロイする（`sha-${KOROKU_COMMIT_SHORT}` のタグがまだなければ pull で失敗し、デプロイログに `The registry has no image with that tag (manifest unknown)` と出る）。push での自動デプロイを使うなら、koroku の設定タブの「先に待つワークフロー」にイメージを作るワークフローのファイル（例 `.github/workflows/image.yml`）を入れる。koroku は push ではなく、そのワークフローがブランチの最新のコミットで成功したときにデプロイする。ワークフローは `.github/workflows/` の直下に置き、追うブランチへの push のたびに走らせる（`paths` で絞ると、走らなかった push は自動では出ない）。CI のあとに `on: workflow_run` で別のワークフローがイメージを作るなら、そちらを入れる（成功したときの `head_sha` は元の push のコミット）。失敗・取り消しの実行ではデプロイせず、koroku の操作の記録に残る。手元のリハーサル（`scripts/rehearse.sh`）は手元の Docker のログイン（`docker login`）で pull する |
| 自前のリバースプロキシ | 静的ファイルのためだけに nginx を前に置いているなら、外してアプリのプロセスに配らせる（下の「静的ファイルの配り方」）。残す場合や、アプリの中に Caddy や nginx を置いて振り分けている場合、TLS は koroku の Caddy が終端するので、アプリ側は平文の HTTP で待ち受け、証明書の取得をやめる（Caddy なら `auto_https off` と `:8080` のようなサイト）。前段の koroku からの `X-Forwarded-For` / `X-Forwarded-Proto` を信頼する設定（Caddy の `trusted_proxies static private_ranges`、nginx の `set_real_ip_from`）がないと、利用者の IP が全員同じ（前段のアドレス）に見え、IP ごとの回数制限が全員まとめてかかる。Caddy の回数制限やログは `{remote_host}` ではなく `{client_ip}` を使う |
| `memswap_limit` | swap を禁じるために `mem_limit` と同じ値を書いてよい。koroku の画面でメモリ上限を上げると、koroku が `memswap_limit` も同じ値に上げる |
| サービス名の `-数字` | ログはコンテナ名からサービス名を求める。サービス名は英小文字と `-` / `_` にしておくと扱いやすい |

## 静的ファイルの配り方

koroku の前段の Caddy はアプリのファイルを配らず、アプリのホスト名へのリクエストはすべて公開サービスへ転送する。ビルドした JS・CSS、画像、フォントなどの静的ファイルは、公開サービスのプロセス自身が配る。ホストのメモリをいちばん使わない形で、無停止デプロイやメモリの予約ともかみ合う。

| 形 | 扱い |
| --- | --- |
| 公開サービスのプロセスが配る（Rails の `public_file_server` か Thruster、Django の WhiteNoise、Express の `express.static`、Next.js の組み込みの配信） | 勧める。増えるメモリはほぼない |
| nginx などのコンテナを公開サービスにして、アプリの前に置く | 勧めない（下記）。すでにこの形のリポジトリは、アプリのプロセスが配る形に寄せられないか確かめる |
| 静的サイト（サーバー側の処理がない） | nginx や Caddy のコンテナを公開サービスにしてよい。それ自体がアプリ |
| koroku の Caddy に配らせる | できない |

**アプリの前に nginx を置かない理由**

- 無停止デプロイが効かなくなる。koroku が並べて入れ替えるのは公開サービスだけ。nginx を公開サービスにすると、後ろのアプリは公開サービスでなくなり、`build:` を持つのでデプロイのたびに止めて作り直される。その間、nginx は 502 を返す。
- メモリが増える。コンテナ 1 つ分の常駐に加え、本番アプリでは koroku が全サービスにメモリの予約（既定は上限の半分。上限の既定 512m なら 256m）を付ける。nginx が数 MB しか使わなくても、ホストの予約の合計に 256m が乗る。
- アセットを named volume でアプリと nginx に共有すると、新しいリリースのアセットに入れ替わらない。Docker がイメージの中身を volume に写すのは、volume が空のときだけ。
- それでも残すなら、nginx のメモリ上限と予約を小さくし（koroku の「リソース」タブ。予約は `0` でもよい）、アセットは volume ではなく nginx のイメージに入れ（マルチステージで同じビルドから写す）、デプロイ中の数秒の 502 を受け入れる。

**配るときの設定**

- ファイル名に fingerprint（中身のハッシュ）が付いたファイルは `Cache-Control: public, max-age=31536000, immutable` で返す。2 回目からはブラウザーが問い合わせず、アプリのワーカーも CPU も使わない。メモリと負荷にいちばん効くのはここ。
- fingerprint のないファイル（`favicon.ico`、`robots.txt` など）は短いキャッシュにする。
- 圧縮は、ビルド時に `.gz` / `.br` を作って配るか、配るプロセスに任せる。リクエストのたびに圧縮すると CPU を使う。
- 配るプロセスのキャッシュは、そのコンテナのメモリとして数えられる。Thruster は既定で 64MB までの HTTP キャッシュを持つ（`THRUSTER_CACHE_SIZE` で変えられる）。
- 利用者がアップロードしたファイル（Active Storage、Django の `MEDIA_ROOT` など）を named volume に置いてアプリが配るのは、そのままで動き、koroku のバックアップにも入る。アクセスが多ければ、S3 互換のオブジェクトストレージに置いて、ブラウザーにはそこから直接（署名付きの URL などで）読ませる形を利用者に紹介する。koroku はアプリが使うオブジェクトストレージをバックアップしない。

## CDN

koroku は CDN を扱わない（設定も連携もない）。アプリの構成を見て効きそうなときだけ、利用者に紹介する。使うかどうかを決め、CDN を契約して設定するのは利用者。特定の事業者は勧めない（条件で書く）。

**紹介するとき**（すべてに当てはまる）

- 本番アプリで、アクセスの保護なしに外へ公開している。
- 静的ファイルが多いか大きい（画像やフォント、数百 KB を超える JS バンドル、ダウンロードさせるファイル）か、利用者がホストから遠い地域にいる。
- アセットのファイル名に fingerprint が付いている（付いていないと、更新のたびに CDN のキャッシュを消す手間が要る）。

**紹介しないとき**

- staging、レビューアプリ、社内向けの小さなアプリ、アクセスの保護の後ろにあるアプリ。保護したアプリには CDN が取りに来られない（`GET` は管理画面への 302 になる）。
- 静的ファイルが少なく、ブラウザーのキャッシュ（上の `immutable`）で足りるもの。

**勧める形: アセットだけを別のホスト名で CDN から配る**

- CDN に `assets.example.com` のようなホスト名を割り当て、取りに行く先（オリジン）をアプリの URL（`https://<slug>.<ベースドメイン>` か、koroku で有効にした独自ドメイン）にする。CDN からオリジンへのリクエストの Host はオリジンのホスト名にする（koroku の Caddy はホスト名でアプリを選び、知らないホスト名には 404 を返す）。
- アセットの URL の土台は、実行時の環境変数でアプリに渡す（例 `ASSET_HOST`）。本番アプリにだけ設定し、staging では空にしてアプリ自身が配る。ビルド時に焼き込まない（上の「ビルド時と実行時の設定」。昇格で staging のイメージを本番に使うため）。
- 別のホスト名から読むフォントには CORS のヘッダー（`Access-Control-Allow-Origin`）が要る。アプリの静的ファイルの応答に付け、CDN がそれを保ったままキャッシュするようにする。
- CDN はアプリの `Cache-Control` に従わせる。fingerprint が変わるので、デプロイのたびにキャッシュを消す操作は要らない。
- CDN の各拠点の最初の取得はアプリに届くので、アプリが配る設定（上の「静的ファイルの配り方」）はそのまま要る。

**勧めない形: サイト全体を CDN の後ろに置く（アプリのドメインの DNS を CDN に向ける）**

- koroku は独自ドメインを、名前解決の結果がホストの IP と一致したときに有効にする。CDN に向けると一致しないので、koroku のオーナーが「強制有効化」するまで経路も証明書も付かない。
- 証明書はホスト名ごとに HTTP-01 で取る。CDN が `/.well-known/acme-challenge/` を素通しにしないと、取得と更新が失敗する。
- アプリと koroku のアクセスログに届く送信元が CDN のアドレスになる。利用者の IP は CDN が付けるヘッダーから読むことになる。
- 使いたいと言われたら、この 3 点を伝えて利用者に判断してもらう。

## デプロイのたびに止まる長いジョブ

デプロイ（とロールバック、停止、再起動、アクセスがない間の休止）では、ジョブを動かすだけのサービス（supercronic などの cron、Sidekiq、Solid Queue のワーカー）も止まる。アプリのイメージをビルドするワーカーは、デプロイのたびに作り直される（上の「コミットの変数を受け取るサービス」）。Docker はコンテナに `SIGTERM` を送り、`stop_grace_period`（書かなければ 10 秒）を過ぎたら `SIGKILL` で強制終了する。30 分かかる集計を cron のコンテナで走らせていると、その間のデプロイで途中で殺され、どこにも記録が残らない。

- **ジョブは途中で殺されてもよいように作る**: 何度実行しても同じ結果になる（冪等）か、続きから再開できるようにする。進み具合を DB に記録し、次の回がそこから続ける。1 回のジョブは短く分ける（1 件ずつのジョブをキューに入れる、範囲を区切る）。猶予を延ばしても、ホストの再起動や OOM では止まる。
- **走っている処理を終えてから止まれるサービスには `stop_grace_period` を付ける**:
  - supercronic: `SIGTERM`（`SIGINT`、`SIGQUIT` も）を受けると新しい実行を始めず、走っているジョブの終わりを待ってから終了する（ジョブ自身にはシグナルを送らず、待つ時間に上限もない）。ジョブがかかる時間より長い `stop_grace_period` を付ける。supercronic をコンテナの PID 1 にする（`command: ["supercronic", "/app/crontab"]` のような exec 形式。`sh -c` で包むとシグナルが届かず、猶予のあとで強制終了される）。
  - Sidekiq: `SIGTERM` を受けると新しいジョブを取らず、走っているジョブを shutdown timeout（既定 25 秒、`-t` か `:timeout:`）まで待ち、終わらなかったジョブを Redis に戻して終了する（戻したジョブは次の起動でやり直される）。`stop_grace_period` はそれより長く（30 秒以上）する。Puma の中で動かす埋め込みモードでも同じで、Web のサービスに付ける。
  - Solid Queue: 停止のときに `SolidQueue.shutdown_timeout`（既定 5 秒）まで走っているジョブを待ち、終わらなかったジョブはキューに戻る。延ばすなら、それより長い `stop_grace_period` を付ける。Puma の中で動かすなら（`plugin :solid_queue`）Web のサービスに付ける。
- **上限は合計 10 分**: koroku は全サービスの `stop_grace_period` の合計が 10 分を超える Compose を拒否する（ポリシー検査の `stop_grace_period`。「検査を緩める」でも許可されない）。Compose は依存の順に 1 サービスずつ止めるので、待ち時間が足し合わさるため。
- **長い猶予は待たされる**: デプロイはホスト全体で 1 件ずつ順に進む。猶予の長いサービスがジョブの終わりを待つ間、このアプリのデプロイも、その後ろに並んだほかのアプリのデプロイも待つ。起動に失敗して自動で戻すときは、もう一度待つ。デプロイログの `start` の手順に、猶予が既定より長いコンテナと、待ちうる時間の合計が出る。猶予は、止められるコンテナが作られたときの値が使われる（`stop_grace_period` を変えたデプロイでは、古いコンテナは古い値で止まる）。
- **koroku の定期タスクの実行中の push**: koroku のタスク（定期タスクを含む）の実行中は、そのアプリにデプロイを積まない。push での自動デプロイは断らずに預かり、実行が終わってから積む（デプロイのタブに「コマンドの実行が終わったら自動デプロイします」と出る）。画面からの手動のデプロイは「実行中」として断られる。

## アクセスの保護

koroku はアプリ単位で「管理画面にログインしている人だけが開ける」ようにできる。staging のアプリ（`environment = staging`）は最初から有効、本番は無効で、あとから画面で切り替えられる。アプリには手を入れない（koroku の前段の Caddy が止める）。

| 点 | 内容と対処 |
| --- | --- |
| ブラウザー以外は通れない | 保護したアプリには、ログインできない呼び出し元（GitHub や決済サービスの Webhook、外部の死活監視、モバイルアプリ、ほかのサービスからの API 呼び出し）が届かない。`GET` は管理画面へ 302、それ以外は 401 になる。受け口のパスを洗い出し、利用者に koroku の設定画面の「保護を外すパス」へ登録してもらう |
| 外すパスの書き方 | 前方一致で、大文字と小文字を区別する（`/webhooks/` を登録しても `/Webhooks/x` は外れない）。ディレクトリは末尾を `/` にする（`/webhooks` だと `/webhooks-admin` も外れる）。英数字と `. _ ~ - / @ : +` だけで、`*` や `%` は使えない。20 件まで。受け口を 1 つのディレクトリ（`/webhooks/` など）にまとめておくと登録が少なくて済む |
| 外れない呼び出し方 | 登録したとおりの綴りで始まる URL だけが外れる。パスに `.` や `..` だけの区切り、`//`、`\`、パーセントエンコードした `.`・`/`・`\`（`%2e`・`%2f`・`%5c`）を含む呼び出しは外れず、ログインを求められる（`GET` は 302、ほかは 401）。呼び出し元に登録する URL は、アプリのルーティングと同じ大文字・小文字で、`https://<host>/webhooks/github` のように余計な `/` を挟まずに書く。クエリーの中身は問わない |
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

## 無停止デプロイ（blue/green。選んだアプリだけ）

ここに書くことは、koroku のアプリ設定で「無停止デプロイ」を有効にしたアプリにだけ関係する（既定は無効。デプロイタブの「デプロイの設定」で、そのアプリの「管理」以上のロールを持つ人が切り替える）。無効のアプリは、上の各節のとおりデプロイのたびに公開サービスを作り直し、数秒止まる。設定の変更は次のデプロイから効く。

有効にすると、koroku は公開サービスだけを 2 つ並べて入れ替える。

1. 公開サービス（例 `web`）を `web-blue` と `web-green` の 2 つのサービスに写し（`extends: { file: <Compose ファイル>, service: web }`）、元の `web` はプロファイル（`koroku-disabled`）で `up` の対象から外す。2 つの色は別々のループバックのポートで待ち受け、どちらも元のサービスのネットワークで `web` という別名を持つ。
2. デプロイでは、マイグレーションなどのワンショットのサービスと、いま使っていない色（新色）だけを起動する（`up --no-deps --wait`）。DB、ワーカー、いま使っている色（旧色）には触れない。
3. 新色のポートにヘルスチェックパスで HTTP の確認をし、通ったら前段の Caddy の転送先を新色に切り替える。
4. ワーカーやスケジューラーなどほかのサービスを作り直す。
5. 切り替えのあとの待ち時間（アプリの設定「旧色を止めるまでの待ち時間」。0〜300 秒、既定 10 秒）だけ待ってから、旧色を `SIGTERM` で止め（`stop_grace_period` の間、処理中のリクエストを待つ）、消す。

override は次の形になる（`build:` を持つ公開サービスの例）。

```yaml
services:
  web:
    profiles: ["koroku-disabled"]   # up では起動しない。ビルドとタスク（run）で使う
    image: koroku-<slug>-web:r<デプロイ ID>
    ports: !reset []
    env_file: [/var/lib/koroku/apps/<slug>/env, /var/lib/koroku/apps/<slug>/env.release]
  web-blue:
    extends: { file: compose.yaml, service: web }
    profiles: !reset []
    build: !reset null              # ビルドは元の web だけが行い、両方の色がそのイメージを使う
    image: koroku-<slug>-web:r<デプロイ ID>
    pull_policy: never
    ports: !override
      - "127.0.0.1:<割当ポート>:<コンテナ内ポート>"
    env_file: [/var/lib/koroku/apps/<slug>/env, /var/lib/koroku/apps/<slug>/env.release]
    networks:
      default:
        aliases: [web]
    # restart、メモリ上限、oom_score_adj、予約は上の「override の中身」と同じ
  web-green:
    # web-blue と同じで、ポートだけが 2 つ目の割当ポート
```

- `KOROKU_COMMIT_SHA` と `KOROKU_COMMIT_SHORT` は、無停止デプロイかどうかによらず `env.release` に分かれる（上の「コミットの変数を受け取るサービス」）。色の付いたサービスと元の公開サービスも受け取る。有効にするときも無効にするときも、DB などに渡る env は変わらないので、切り替えのデプロイで DB は作り直されない。
- **アプリの環境変数を変えたデプロイでは、DB などのサービスも作り直す**。その数秒は新色も DB につながらず、エラーを返しうる。デプロイログの `services` の手順に `Recreating <サービス> because the app's env file changed (<変数の名前>, ...); requests may fail while it restarts.` と出る。
- **ホストの空きメモリ（`MemAvailable` と `SwapFree` の合計）が公開サービスのメモリ上限 1 つ分に満たないときは、新色を起動せずにデプロイが失敗になる**（デプロイログと失敗の理由に `Not enough memory to run two <公開サービス> containers (<空き> MiB available, <上限> MiB needed).`）。アプリは旧色のまま動き続ける。その場でコンテナを作り直す形には切り替わらない。メモリを空けるか、無停止デプロイを外してからデプロイし直す。
- 新色が起動しないか確認に通らなければ、新色を消してデプロイは失敗になり、旧色・DB・ワーカーはそのまま動き続ける（ロールバックは要らない）。ワンショットのサービスは流れたあとかもしれない。
- 無停止にするのはデプロイと即時ロールバックだけ。**管理画面の「再起動」は、いまの色とほかのサービスの普通の再起動で、数秒の断がある**。起動、停止、アクセスがない間の休止から起こす処理、ホストの再起動も同じ（Caddy がその間のリクエストを待たせて送り直す。上の「デプロイで koroku がすること」の最後の段落）。
- 有効にして最初のデプロイは、元の形の `web`（1 つ目の割当ポート）を旧色として green を起動し、`drain` で `web` を消す（ログの見出しは `Switching traffic from web to green`、`Stopping web`）。以後は blue と green を交互に使う。公開サービスが動いていない（初回、停止中、休止中）ときは並べる相手がないので、いまの色（なければ blue）で全サービスを一度に起動し、切り替えはない（`start` → `route` → `check` → `done`）。無効にしたあとの最初のデプロイは、色の付いたコンテナを消して元の形に戻すので、その 1 回は数秒の断がある。

リポジトリ側で揃えること:

| 点 | 内容と対処 |
| --- | --- |
| マイグレーションは古い版のコードで動く形にする（expand / contract） | マイグレーションは新色の起動の前に流れ、そのあとも切り替えまで古い版が利用者のリクエストを受ける。足す（テーブル、NULL を許すか既定値のあるカラム、インデックス）のは 1 回でよい。消す・名前を変える・型を変える・NULL を禁じるのは 2 回のデプロイに分ける。1 回目でコードがそのカラムを使わないようにし（Rails なら `self.ignored_columns += ["old_column"]`）、2 回目で消す。守れないマイグレーションを出すときは、そのデプロイの間だけ無停止デプロイを無効にする（守らないと、新色が確認に通らなかったとき、古い版が新しいスキーマのまま動き続けてエラーを返す。マイグレーションは戻らない） |
| ジョブの引数も前後の版で読める形にする | 切り替えからワーカーの作り直しまでの間、新しい版の公開サービスがキューに入れたジョブを古い版のワーカーが受けることがある。ジョブのクラスや引数を変えるときも、足してから消す順にする |
| 公開サービスは状態を持たない | 2 つの色が数十秒並ぶ。セッションは Cookie か DB・Redis に、アップロードは named volume（両方の色が同じ volume をマウントする）かオブジェクトストレージに置く。公開サービスのコンテナの中の SQLite に書くアプリは使わない（並ぶ間に古い版と新しい版が同じファイルに書く） |
| 同時に 2 つ動くと困る処理を公開サービスで動かさない | プロセス内の cron（`rufus-scheduler` など）や、起動時に 1 回だけのつもりの処理は、並ぶ間に 2 回走る。ワーカーかスケジューラーのサービスに分けるか、koroku の定期タスクにする。Puma の中で動かすジョブキュー（`plugin :solid_queue`、Sidekiq の埋め込み）はキューの側で排他されるので並んでも壊れないが、旧色が止まるときに `stop_grace_period` の間で終わる必要がある |
| ヘルスチェックは準備ができてから返す | koroku は新色の確認が通った時点で切り替える。DB への接続やアプリの読み込みが済む前に 200 を返すと、切り替えの直後のリクエストが失敗する。Rails 7.1 以降の `/up` は、アプリが起動してから応える |
| 処理中のリクエストを終えてから止まる | 旧色は待ち時間のあとに `SIGTERM` を受ける。Puma などは新しい接続を断って処理中のものを終える。待ち時間より長いリクエストを抱えるアプリは、アプリの設定で待ち時間を延ばす（最大 300 秒） |
| `container_name` を書かない | 2 つ並べると名前が衝突する（koroku はデプロイの検証で断る） |
| ほかのサービスから公開サービスに `depends_on` しない | 元の `web` は `up` の対象から外れるので、依存の先がなくなる（koroku はデプロイの検証で断る）。`network_mode: service:web` なども同じ。名前で呼ぶ（`http://web:3000`）のはよい（色の付いたサービスが `web` の別名を持つ。並ぶ間はどちらの色にも解決される） |
| `web-blue` / `web-green` という名前のサービスを作らない | 色の付いたサービスと衝突する（koroku はデプロイの検証で断る） |
| 公開サービスを Compose ファイルの中で定義する | `include:` した別ファイルにあると写せない（koroku はデプロイの検証で断る） |
| 公開サービスをワンショットにしない、`deploy.replicas`（か `scale`）を 2 以上にしない | 並べる単位が 1 コンテナであること（koroku はデプロイの検証で断る） |
| WebSocket と SSE は再接続できるようにする | 旧色を止めたときに切れる |
| メモリの上限 | デプロイの間だけ公開サービスが 2 つになり、上限 1 つ分のメモリが余分に要る。足りなければデプロイが失敗する。上限を大きくしすぎない |

- ワンオフのコマンドと定期タスクで公開サービスを選ぶと、これまでどおり `web` の名前で、同じイメージと環境変数のコンテナで動く（プロファイルで外したサービスも、名前を指定すれば `run` できる）。`web-blue` / `web-green` はタスクのサービスに指定できない（koroku が `422` で断る）。報告の表のタスクにも `web` と書く。
- デプロイタブの「無停止デプロイ」の欄は、稼働中のリリースの Compose が上の条件を満たさないとき、警告と理由を出す（デプロイのたびに koroku が判定して記録したもの。API ではアプリの `blue_green_problems`）。公開サービスが `include:` した別ファイルにあることは、有効にしてデプロイするまで分からない。
- koroku の実行ログ、メモリの履歴、リソースの画面では、色の付いたサービスも `web` として出る。デプロイの履歴には、起動した色と切り替えた時刻が出る。

### 手元でなぞる

`scripts/rehearse.sh --blue-green` が、上の override と切り替えを手元の Docker でなぞる。

- **`env.release`**: 無停止デプロイかどうかによらず、`rehearse.sh` は koroku と同じくコミットの変数を `env.release` に書いて、受け取るサービスにだけ渡す。コミットの変数をすべて 0 にして正規化し直し、定義がコミットに従うサービスを `Services that follow the commit:` に出す。DB などがコミットの変数を読んでいたら、ここで空になることが分かる。
- 最初に、koroku が検証で断る形（上の表の `container_name`、依存、名前の衝突、`replicas` と `scale`、ワンショット、`include:`）を調べ、当てはまればすべて並べて失敗する。
- blue（`--host-port`）とほかのサービスを起動して確かめたあと、もう一度デプロイする（同じビルドを次のリリースのタグで使う）。ワンショットのサービスと green（`--alt-host-port`、既定 28081）を `up --no-deps --wait` で起動し、green を確かめ、切り替え、ほかのサービスを作り直し（サービスごとに「作り直した／同じコンテナのまま」を出す）、`--drain` の秒数（既定 10）待って blue を止めて消す。
- Caddy はないので、「いまの転送先」のポートを出す。2 回目のデプロイの間、0.2 秒ごとにそのポートへリクエストを送り、接続エラーか 5xx が 1 件でもあれば失敗にする。マイグレーションが古い版と両立しないと、ここで旧色のエラーとして出る。
- コミットの変数を受け取るサービスの判定は、無停止デプロイでないときと同じ（上の「`env.release`」）。手元では 2 回目のデプロイも同じコミットなので、定義がコミットに従うサービスは作り直されない（koroku では新しいコミットのたびに作り直される）。
- `config`、`build`、`pull` には koroku と同じく `--profile koroku-disabled` を付け、元の公開サービスを含めて検証し、ビルドする。
- 空きメモリが公開サービスの上限に満たなければ、koroku と同じく green を起動せずに `Not enough memory to run two <サービス> containers (<n> MiB available, <m> MiB needed).` で失敗する。green が起動しないか確認に通らなければ、green を消し、blue が答え続けたことを出して失敗する。
- `--sleep` は、いまの色とほかのサービスを名前で並べて `start --wait` する（koroku が休止から起こすときと同じ）。`--task web:...` は元の `web` で動く（`--task web-green:...` は koroku と同じく断る）。
