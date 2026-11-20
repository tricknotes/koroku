# 初回セットアップ（外部サービス）

本番のサーバーに `koroku` を載せるまでに、外部サービス側で行う設定を順に並べる。ホストの構築（`provision.sh`）と更新・復旧は [OPERATIONS.md](OPERATIONS.md)、設計の背景は [ARCHITECTURE.md](ARCHITECTURE.md) を参照。

- 特定の事業者には依存しない。各節の冒頭に「満たすべき条件」を書き、続けて一般的な手順を書く。画面の名前や操作は事業者によって違うので、それぞれのドキュメントで読み替える。
- 各節の最後に、`config.toml` のどこに何を書くかを示す。値はまとめて 7 節に書き込む。
- 終わったら `sudo koroku check` を実行する（8 節）。足りないものと、この文書のどの節を読めばよいかが表示される。途中で何度実行してもよい。
- ここに出てくるドメインと IP は例（`dev.example.com`、`203.0.113.10`）。

## 1. 全体の流れ

順番に意味がある。DNS は反映に時間がかかるので早めに、証明書は DNS とポートが揃ってから取れる。

| 順 | すること | 節 | 控える値 |
| --- | --- | --- | --- |
| 1 | サーバーを用意し、事業者側のファイアウォールで 80/443 を開ける | 2 | グローバル IPv4 アドレス |
| 2 | DNS に A レコードを登録する | 3 | — |
| 3 | `provision.sh` を実行する | [OPERATIONS 3](OPERATIONS.md#3-プロビジョニング) | — |
| 4 | GitHub App を登録し、自分のアカウントにインストールする | 4 | App ID、Client ID、client secret、秘密鍵、ユーザー ID |
| 5 | オブジェクトストレージのバケット、ライフサイクルルール、アクセスキーを用意する | 5 | endpoint、region、Access Key ID、Secret Access Key |
| 6 | restic のパスワードを作り、秘密をサーバーの外に控える | 6 | restic のパスワード |
| 7 | `config.toml` を書いて `koroku` を起動する | 7 | — |
| 8 | `koroku check` で確かめ、初回のバックアップを取る | 8 | — |
| 9 | 証明書を Let's Encrypt の本番に切り替える | 9 | — |

## 2. サーバー

**満たすべき条件**

| 項目 | 条件 |
| --- | --- |
| OS | Ubuntu 24.04 か 26.04、amd64（x86_64）。それ以外では `provision.sh` が止まる |
| 大きさ | 2 vCPU、メモリ 4GB、ディスク 40GB 程度から（アプリ 3〜5 個、ビルドをサーバー上で行う前提の目安） |
| ネットワーク | 固定のグローバル IPv4 アドレス。80/tcp と 443/tcp に外から届くこと（Let's Encrypt の HTTP-01 / TLS-ALPN-01 の検証に要る） |
| ログイン | SSH の公開鍵でログインできる、sudo 可能なユーザー |

**手順**

1. 事業者の管理画面でサーバーを作る。OS に Ubuntu 24.04 か 26.04 を選び、作成時に SSH の公開鍵を登録できるなら登録する（鍵がなければ手元で `ssh-keygen -t ed25519` で作る）。
2. 割り当てられたグローバル IPv4 アドレスを控える。これが `public_ip` と DNS の値になる。
3. 事業者側にファイアウォール（パケットフィルター、セキュリティグループなどと呼ばれる）があれば、22/tcp、80/tcp、443/tcp の受信を許可する。443/udp（HTTP/3）は開けなくてよい（OPERATIONS 8.2）。ホストの中でも ufw が同じ 3 つだけを通すので、事業者側は使わなくても動くが、二重にしておくほうが安全。
4. 手元から鍵でログインできることを確かめる。作成時に鍵を登録できなかった場合は、パスワードでログインして `ssh-copy-id` で鍵を登録し、鍵でログインし直す。`provision.sh` は、実行したユーザーに鍵が登録されているときだけパスワードでのログインを止める（OPERATIONS 3 章）。

   ```sh
   ssh <ユーザー>@203.0.113.10
   ```

> IPv6 のアドレスは DNS に登録しない（AAAA レコードを作らない）。`koroku` は、名前解決で得られた **すべての** アドレスが `public_ip` と一致することを確かめるので（ARCHITECTURE 8 章）、AAAA があると管理 UI の確認と独自ドメインの検証が通らない。

## 3. DNS

**満たすべき条件**: `console.<base_domain>`（管理 UI）と `<slug>.<base_domain>`（各アプリ）が、サーバーの IPv4 アドレス **だけ** に解決されること。

`base_domain` は、持っているドメインそのもの（`example.com`）でも、そのサブドメイン（`dev.example.com`）でもよい。

**手順**

1. ドメインの DNS を管理しているサービス（レジストラ、DNS ホスティング）の画面で、次のレコードを足す。ホスト名の書き方（`*.dev` とだけ書くか、`*.dev.example.com` と書くか）はサービスによって違う。

   | 名前 | 種類 | 値 |
   | --- | --- | --- |
   | `*.dev.example.com` | A | サーバーの IPv4 |
   | `console.dev.example.com` | A | サーバーの IPv4 |

   - `console` の行はワイルドカードで足りるが、明示しておくとワイルドカードを消したときも管理 UI が残る。
   - **ワイルドカードのレコードを登録できないサービスでは**、アプリを作るたびに `<slug>.dev.example.com` の A レコードを足す。`koroku check` はワイルドカードがないことを警告として表示する（失敗ではない）。
   - CDN やプロキシを挟む機能（DNS サービスによっては既定で有効）は使わない。証明書は Caddy が取るので、DNS は IP をそのまま返す設定にする。
   - TTL は、作り直しの多い最初のうちは短め（数分〜1 時間）にしておく。
2. 反映を確かめる。

   ```sh
   dig +short console.dev.example.com
   dig +short anything.dev.example.com
   ```

   どちらもサーバーの IP だけが返れば済み。
3. ドメインに CAA レコードがある場合は、`letsencrypt.org` を許可するものを足す（なければ何もしなくてよい）。

## 4. GitHub App

ログイン、private リポジトリの取得、デプロイの状態をリポジトリの Deployments に写すのに使う（ARCHITECTURE 5 章）。

### 4.1 登録

<https://github.com/settings/apps/new> を開き、次のとおりに入力する。書いていない項目は既定のまま。

| 項目 | 値 |
| --- | --- |
| GitHub App name | 誰とも重ならない名前（例: `koroku-<自分の login>`） |
| Homepage URL | `https://console.dev.example.com` |
| Callback URL | `https://console.dev.example.com/auth/github/callback`（1 つだけ） |
| Expire user authorization tokens | オンのまま |
| Request user authorization (OAuth) during installation | オフ |
| Enable Device Flow | オフ |
| Webhook → Active | **オフ**（`koroku` は Webhook を受けない） |
| Repository permissions → Contents | **Read-only** |
| Repository permissions → Metadata | Read-only（自動で付く） |
| Repository permissions → Deployments | **Read and write**（デプロイの状態をリポジトリの Deployments に出す。なくても動くが、反映されない） |
| ほかの権限 | すべて No access |
| Where can this GitHub App be installed? | **Only on this account**（別の org のリポジトリも使うなら **Any account**。4.5） |

あとから Deployments の権限を足した場合は、App の設定画面の **Permissions & events** で変えて保存したあと、インストール先（**Install App** → 自分のアカウントの歯車）に出る権限の変更の依頼を **Accept** する。受け入れるまで、GitHub は koroku からの記録を 403 で断る（koroku は警告を出してデプロイを続ける）。

**Create GitHub App** を押したあと、App の設定画面で次を行う。

1. **App ID** と **Client ID** を控える。
2. **Client secrets** → **Generate a new client secret**。表示は一度きりなので、すぐに控える。
3. **Private keys** → **Generate a private key**。`<app 名>.<日付>.private-key.pem` がダウンロードされる。

### 4.2 インストール

App の設定画面の左の **Install App** → 自分のアカウントの **Install**。**Only select repositories** でデプロイしたいリポジトリだけを選ぶ（あとから足せる）。public リポジトリは選ばなくてもデプロイできるが、ブランチやコミットの一覧が速くなるので選んでおくとよい。

App をインストールしていない public リポジトリ（OSS など）は、作成画面の「一覧にない公開リポジトリを URL で指定」から URL で指定できる。

### 4.3 ログインを許可するユーザー ID

数値の ID を調べる（login 名ではない）。

```sh
curl -s https://api.github.com/users/<自分の login> | grep '"id"'
```

このアカウントの 2 段階認証を、GitHub の Settings → Password and authentication で有効にしておく。管理画面へのログインは GitHub へのサインインを経るので、2 段階認証は GitHub がそこで求める。koroku 自身はアカウントの 2 段階認証の状態を確かめられない（GitHub は GitHub App にそれを知らせない。ARCHITECTURE 10.3）。

### 4.4 サーバーに置く

```sh
# 手元
scp ~/Downloads/<app 名>.*.private-key.pem <ユーザー>@203.0.113.10:github-app.pem

# サーバー上
sudo install -m 0600 -o root -g root github-app.pem /etc/koroku/github-app.pem
rm github-app.pem
```

ダウンロードした `.pem` は、パスワードマネージャなどに入れてから手元から消す。

**`config.toml`**: `[github]` の `app_id`、`client_id`、`client_secret`、`allowed_user_id`。`private_key_file` は既定の `/etc/koroku/github-app.pem` のまま。

### 4.5 別の org のリポジトリを使う（任意）

自分が管理する別の org の private リポジトリをデプロイするときだけ行う。

1. App の設定画面の **Advanced** → **Make public**（「Where can this GitHub App be installed?」を **Any account** にする）。App のページを知っていれば誰でもインストールできるようになるが、koroku は下の 3 で許したアカウントのインストールしか使わない。
2. App のページ（`https://github.com/apps/<app 名>`）の **Install** から、その org を選んでインストールする。**Only select repositories** でデプロイしたいリポジトリだけを選ぶ。
3. `config.toml` の `[github]` に org の login を書き、`koroku` を再起動する。

   ```toml
   installation_accounts = ["my-org"]
   ```

4. `sudo koroku check` の `github.install` に org が出ることを確かめる。知らないアカウントにインストールされていれば警告が出るので、その場合は App の設定画面の **Install App** の一覧を確かめる。
## 5. オブジェクトストレージ

ログの退避（`logs/`）とバックアップ（`restic/`）を 1 つのバケットに置く（ARCHITECTURE 10.1、10.2）。

**満たすべき条件**

| 項目 | 条件 |
| --- | --- |
| API | S3 互換 API（署名 V4）。`koroku` は事業者固有の API を使わない |
| 接続方式 | パス形式（`https://<endpoint>/<bucket>/<key>`）を受け付けること。仮想ホスト形式（`https://<bucket>.<endpoint>/`）にしか対応しないサービスは使えない |
| 通信 | `https` の endpoint |
| バケット | 先に作っておく。`koroku` も restic もバケットを作らない |
| アクセスキー | このバケットだけに限定し、オブジェクトの読み取り・書き込み・一覧・削除を許可する。バケットの作成や設定変更の権限は要らない |
| ライフサイクルルール | プレフィックス `logs/` のオブジェクトを 90 日で削除できること（できなければ、退避したログが消えずに溜まり続ける） |
| ダウンロード | 署名付き URL（presigned URL）での取得。過去ログのダウンロードに使う |

**手順**

1. バケットを作る。名前は `koroku` など。公開アクセスは無効のまま（ログもバックアップも非公開）。バージョニングは不要。
2. ライフサイクルルールを 1 つ足す。

   | 項目 | 値 |
   | --- | --- |
   | 対象 | プレフィックス `logs/` |
   | 動作 | 作成から 90 日で削除 |

   **`restic/` を対象にするルールは作らない。** 期限で消されると restic のリポジトリが壊れ、どのバックアップも戻せなくなる。プレフィックスを空にしたルール（バケット全体）も同じ理由で作らない。「未完了のマルチパートアップロードを中止する」ルールはあってよい。
3. このバケットに限定したアクセスキー（API トークン）を作り、**Access Key ID** と **Secret Access Key** を控える。Secret は作成時にしか表示されないことが多い。
4. 事業者の S3 互換 API の **endpoint**（`https://...`）と **region** を控える。region の概念がないサービスでは `auto` にする。

**`config.toml`**: `[storage]` の `endpoint`、`region`、`bucket`、`access_key_id`、`secret_access_key`。

## 6. restic のパスワードと、サーバーの外に控えるもの

サーバー上でパスワードを作り、そのまま鍵ファイルにする。

```sh
sudo sh -c 'umask 077; openssl rand -base64 32 > /etc/koroku/restic.key'
sudo cat /etc/koroku/restic.key    # パスワードマネージャなどに控える
```

次を、パスワードマネージャなどサーバーの外に控える。ホストを失ったときの復旧（OPERATIONS 11 章）に要る。

- restic のパスワード（`/etc/koroku/restic.key` の中身）
- オブジェクトストレージの endpoint、region、バケット名、Access Key ID、Secret Access Key
- GitHub App の client secret と秘密鍵（なくしても GitHub で作り直せる）
- `master.key`: `koroku` の初回起動で `/etc/koroku/master.key` に作られる。最初のバックアップが成功するまでは、`sudo base64 /etc/koroku/master.key` の出力も控えておく（そのあとは基盤のバックアップに入る）

## 7. `config.toml` を書く

`provision.sh` が `/etc/koroku/config.toml` を例から作ってある（0600）。`sudo -e /etc/koroku/config.toml` で次を書き換える。キーの一覧と既定値は、導入したバイナリと同じコミットの `deploy/config.example.toml` が正（OPERATIONS 5 章）。

```toml
base_domain = "dev.example.com"     # 3 節
public_ip = "203.0.113.10"          # 2 節

[caddy]
# 9 節まではこのまま（Let's Encrypt の staging）
acme_ca = "https://acme-staging-v02.api.letsencrypt.org/directory"
# acme_email = "you@example.com"    # 任意。証明書の期限切れの連絡先

[github]                            # 4 節
app_id = 123456
client_id = "Iv23li..."
client_secret = "..."
allowed_user_id = 1234567

[storage]                           # 5 節
endpoint = "https://s3.example.com"
region = "auto"
bucket = "koroku"
access_key_id = "..."
secret_access_key = "..."
```

書いたら起動する。

```sh
sudo systemctl restart koroku
journalctl -u koroku -n 50 --no-pager
```

`koroku` は知らないキー、半端な `[github]` や `[storage]`、緩い権限のファイルがあると起動を拒否し、その理由をログに出す。

## 8. `koroku check` で確かめる

```sh
sudo koroku check
```

1 行ずつ `[ ok ]`、`[warn]`、`[FAIL]`、`[skip]` と結果が出る。`[FAIL]` と `[warn]` には直し方（`->` の行）と、この文書の節が付く。`[FAIL]` が残っている間は終了コードが 1 になる。

| 名前 | 確かめること |
| --- | --- |
| `config` | 例の値（`example.com`、`203.0.113.x`）が残っていない |
| `public_ip` | ホストのアドレスに含まれる（NAT の下では警告になる） |
| `dns.console`、`dns.apps` | 管理 UI のホスト名と、未登録の `<乱数>.<base_domain>`（ワイルドカード）が `public_ip` だけに解決される |
| `caddy` | Caddy の admin API が応答する |
| `https` | 管理 UI に HTTPS で接続でき、証明書が信頼できる（staging の間は警告） |
| `master_key` | `master.key` があり、0600 |
| `github.app` | App ID と秘密鍵の組が正しく、Client ID が一致し、権限が Contents と Metadata の Read-only と Deployments の Read and write だけ（Deployments がなければ警告） |
| `github.install` | App が 1 つ以上のアカウントにインストールされている |
| `github.user` | 許可するユーザー ID のアカウントが実在する |
| `storage` | バケットに書き、読み、一覧し、消せる（`koroku-check/` に小さなファイルを書いてすぐ消す） |
| `backup` | restic のリポジトリを読める（パスワードが合う）。基盤のスナップショットがあり、新しい |

ライフサイクルルールと署名付き URL は、`koroku check` では確かめない（ルールの読み取りには、アクセスキーに要らない権限が要るため）。5 節のとおりに設定したことを、事業者の画面で確かめる。

すべて `[ ok ]` か、意図した `[warn]` だけになったら:

1. ブラウザーで `https://console.dev.example.com` を開き、GitHub でログインする（staging の証明書なので警告が出る。9 節で消える）。
2. **システム** 画面で **バックアップ** の **今すぐ実行** を押し、成功したら `koroku check` をもう一度実行する。`backup` が `[ ok ]` になれば、restic のリポジトリがバケットにできている。

## 9. 証明書を本番に切り替える

staging のままで、DNS、ログイン、デプロイ、バックアップまで一通り動くことを確かめてから切り替える。Let's Encrypt の本番は、同じホスト名の組み合わせに週 5 枚までしか証明書を出さないため（設定を何度もやり直すと使い切る）。

1. `sudo -e /etc/koroku/config.toml` で `[caddy]` の `acme_ca` の行を消す。
2. `sudo systemctl restart koroku`。`koroku` が Caddy の設定を入れ直し、Caddy が本番の証明書を取り直す（数十秒）。
3. `sudo koroku check` の `https` が `[ ok ]` になることを確かめる。ならなければ `journalctl -u caddy -n 100 --no-pager` を見る。
