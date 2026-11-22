# 運用手順

`koroku` を載せる VPS の構築、更新、復旧の手順をまとめる。設計の背景は [ARCHITECTURE.md](ARCHITECTURE.md) の 3 章（配置）、8 章（Caddy）、10 章（セキュリティと運用）を参照。サーバー、DNS、GitHub App、オブジェクトストレージなど、外部サービス側の設定は [SETUP.md](SETUP.md) に順を追って書いてある。

> この文書の手順は、実機の VPS ではまだ一度も通していない。確認できた範囲と、できていない範囲は末尾の「[検証済みの範囲](#検証済みの範囲)」と「[未検証（実機で確認が必要）](#未検証実機で確認が必要)」に分けて書いてある。

## 1. 全体の流れ

1. 手元で `koroku` のバイナリをビルドする。
2. バイナリと `deploy/` ディレクトリを VPS に送り、`deploy/provision.sh` を root で実行する。
3. DNS を向け、`/etc/koroku/config.toml` と鍵ファイルを用意する（[SETUP.md](SETUP.md)）。
4. `koroku` を起動し、`koroku check` で外部サービスまで含めて確かめる。必要なら再起動（カーネル更新の反映）する。

対象は Ubuntu 24.04 か 26.04（amd64）の新規 VPS 1 台。それ以外の OS ではスクリプトが止まる（`--force` で続行できるが未確認）。

## 2. ビルド

開発用のツール一式は Docker イメージに入っている。開発機に Go や Node.js を入れる必要はない。

```sh
bin/dev make build      # フロントエンド込みの bin/koroku（linux/amd64）ができる
```

## 3. プロビジョニング

```sh
# 手元から VPS へ（ホスト名とユーザー名は例）
scp -r deploy bin/koroku admin@vps.example.com:

# VPS 上で
sudo ./deploy/provision.sh ./koroku
```

`deploy/` はディレクトリごと送る。スクリプトは隣にある `koroku.service`、`caddy-initial.json`、`caddy-koroku.conf`、`config.example.toml` をそのまま設置する。

| オプション | 意味 |
| --- | --- |
| `--skip-upgrade` | 導入済みパッケージを更新しない。アプリが動いているホストで設定だけ当て直すときに使う（Docker が更新されると全コンテナが再起動するため） |
| `--http3` | `443/udp` も開ける（既定は閉じたまま。8.2 節） |
| `--keep-ssh-password` | SSH のパスワードログインを止める手順（14）を飛ばす |
| `--force` | Ubuntu 24.04、26.04 の amd64 以外でも続行する（未確認） |

### スクリプトがすること

| 順 | 内容 |
| --- | --- |
| 1 | 実行条件の確認（root、OS、渡されたバイナリがこのホストで動くこと）。ここで失敗した場合はホストに何も変更を加えていない |
| 2 | 基本パッケージ（`curl`、`gnupg`、`git`、`jq`、`bzip2`、`tzdata`、`ufw` など） |
| 3 | `/etc/docker/daemon.json` に `json-file`、`max-size=10m`、`max-file=3` を設定。既存の設定は残して合成し、元のファイルは `daemon.json.before-koroku` に控える |
| 4 | Caddy の初期設定（`/etc/caddy/koroku-initial.json`）と systemd の drop-in（`caddy.service.d/koroku.conf`）を、Caddy を入れる前に置く |
| 5 | Docker と Caddy の公式 apt リポジトリを追加 |
| 6 | `apt-get full-upgrade`（カーネルを含む） |
| 7 | Docker Engine と Compose プラグイン。Compose が 2.30 未満なら止まる |
| 8 | Caddy。2.7 未満なら止まる。アクセスログ用の `/var/log/caddy/apps`（`caddy` 所有、0750）を作る |
| 9 | restic 0.19.1（公式リリースのバイナリ、SHA-256 を照合）を `/usr/local/bin/restic` に置く |
| 10 | 2GB の swap ファイル `/swapfile` と `/etc/fstab` への登録。有効な swap が合計 2GB 以上あれば何もしない。続けて zswap を有効にする（`/etc/tmpfiles.d/koroku-zswap.conf` に置き、その場でも反映する） |
| 11 | `/etc/koroku` と `/var/lib/koroku`（どちらも 0700）、`/etc/koroku/config.toml`（なければ例から作成、0600）、`/usr/local/bin/koroku`、`koroku.service` |
| 12 | ufw で 22（と sshd の待受ポート）、80、443 の TCP を許可して有効化 |
| 13 | SSH の keepalive。`/etc/ssh/sshd_config.d/02-koroku-keepalive.conf` に `ClientAliveInterval 30` と `ClientAliveCountMax 4` を置き、`sshd -t` で検査してから reload する。放置したセッションが、途中のルーターやファイアウォールに接続を忘れられて固まらないようにするため。応答のないクライアントは約 2 分で切る。鍵の有無に関係なく行う |
| 14 | SSH を鍵だけにする。`sudo` を実行したユーザー（直接 root で実行したなら root）の `~/.ssh/authorized_keys` に鍵があるときだけ、`/etc/ssh/sshd_config.d/01-koroku.conf` でパスワードと keyboard-interactive の認証を止め、root のログインを禁じる（root で実行したときは鍵での root ログインだけ残す）。`sshd -t` で検査してから reload する。鍵がなければ何もせず、まとめで知らせる（ARCHITECTURE 10.4） |
| 15 | `koroku` を有効化。`config.toml` に例の値（`example.com`、`203.0.113.x`）が残っていれば起動はしない |
| 16 | 結果のまとめと、再起動が必要かどうかの表示 |

- **再実行できる。** 内容が同じファイルは書き換えず、変更がなければ Docker も Caddy も `koroku` も再起動しない。`koroku` のバイナリか unit が変わったときだけ `koroku` を再起動する。
- **上書きしないもの。** `/etc/koroku/config.toml`、`master.key`、`restic.key`、`github-app.pem` は、存在すれば中身にも権限にも触れない（権限が緩いときは注意を表示するだけ）。
- **再起動は自動でしない。** カーネルなどが更新されて再起動が必要なときは、最後に `REBOOT REQUIRED` と表示する。都合のよいときに `sudo systemctl reboot` する。Docker、Caddy、`koroku` は起動時に自動で立ち上がり、アプリのコンテナも `restart: unless-stopped` で戻る。
- **途中で失敗したら。** 失敗したコマンドと行番号が表示される。原因を取り除いて同じコマンドをもう一度実行すればよい。
- SSH を 22 番以外で待ち受けている場合、スクリプトは `sshd -T` の `Port` と実行中のセッションのポート（`SSH_CONNECTION`）も許可する。`sshd_config` 以外の方法でポートを変えているときは、実行前に `ufw allow <port>/tcp` しておく。

## 4. プロビジョニングのあとに人がやること

| 項目 | 内容 |
| --- | --- |
| DNS | `*.<base_domain>` の A レコードを VPS の IP に向ける。ワイルドカードが登録できなければ `console.<base_domain>` とアプリごとの `<slug>.<base_domain>` を個別に登録する |
| 事業者側のファイアウォール | VPS の手前に事業者のファイアウォール（パケットフィルター、セキュリティグループなど）がある場合は、そちらでも 80/tcp と 443/tcp を開ける |
| `config.toml` | 5 章のとおりに記入する |
| SSH の鍵 | VPS の作成時に公開鍵を登録していなければ、`ssh-copy-id` で登録し、鍵でログインできることを確かめてからスクリプトを再実行する（パスワードログインはそのとき止まる） |
| GitHub App の秘密鍵 | `/etc/koroku/github-app.pem` に置く（root 所有、0600） |
| restic のパスワード | `/etc/koroku/restic.key` に置く（root 所有、0600） |
| 起動 | `sudo systemctl restart koroku`、`journalctl -u koroku -f` でログを確認 |
| 確認 | `sudo koroku check`。DNS、証明書、GitHub App、オブジェクトストレージ、restic を確かめ、足りないものと SETUP.md の節を示す |
| 控え | 7 章のものを VPS の外に保存する |
| 再起動 | スクリプトが求めていれば `sudo systemctl reboot` |
| 証明書の本番切替 | 動作を確かめたら `config.toml` の `acme_ca` の行を消して `koroku` を再起動する（例の値は Let's Encrypt の staging） |

## 5. `config.toml` に書くもの

キーの名前と既定値は、**導入するバイナリと同じコミットの `deploy/config.example.toml` が正**。`koroku` は知らないキーがあると起動を拒否するので、古い例や新しい例から写さない。

| キー | 書く値 |
| --- | --- |
| `base_domain` | アプリを `<slug>.<base_domain>` で公開するドメイン |
| `console_host` | 管理 UI のホスト名。省略すると `console.<base_domain>` |
| `public_ip` | VPS のグローバル IP。独自ドメインの DNS 検証で照合する |
| `[caddy] acme_ca` | 構築中は staging のまま。本番の証明書に切り替えるときに行ごと消す |
| `[caddy] acme_email` | ACME アカウントの連絡先（任意） |
| `[github] app_id`、`client_id`、`client_secret` | GitHub App の値。権限は Contents と Metadata の Read-only と Deployments の Read and write、callback は `https://<console_host>/auth/github/callback` |
| `[github] private_key_file` | `/etc/koroku/github-app.pem` |
| `[github] allowed_user_id` | オーナーの GitHub ユーザーの数値 ID（1 件）。オーナーはすべての操作ができ、メンバーとロールを管理画面で管理する。メンバーは DB にあり、ここには書かない。この値を変えると、前のオーナーのセッションと API トークンは使えなくなる（ARCHITECTURE 10.7） |
| `[github] installation_accounts` | 任意。App をインストールした別の org など、koroku が使うインストール先の login の一覧。オーナー自身のアカウントは書かなくても使う（SETUP 4.5） |
| `[github] webhook_secret` | 任意。push での自動デプロイと、ワークフローの成功を待つ自動デプロイ（ARCHITECTURE 5.2）に使う GitHub App の Webhook の secret（20 文字以上。`openssl rand -hex 32` で作る）。App 側の Webhook secret と同じ値にする。未設定なら `/webhooks/github` はすべての配送を断る（SETUP 4.6） |
| `[github] require_two_factor` | 使われない（古い版の設定）。書いてあれば起動時に警告が出るので、消してよい。2 段階認証は GitHub アカウント側で有効にする（ARCHITECTURE 10.3） |
| `[storage] endpoint`、`region`、`bucket`、`access_key_id`、`secret_access_key` | S3 互換のオブジェクトストレージ。endpoint と region は事業者の S3 互換 API のもの（region の概念がなければ `auto`）。パス形式（`https://<endpoint>/<bucket>/<key>`）で接続する。バケットは先に作っておく（`koroku` も restic もバケットは作らない）。このセクションがないと、ログは VPS 内にだけ残り、バックアップは無効になる |
| `[backup] restic_password_file` | `/etc/koroku/restic.key`（0600）。中身は自分で決めたパスワード 1 行。ファイルがない、または他人が読める権限だとバックアップは無効になる |
| `[backup] hour`、`timezone` | 日次バックアップの時刻（既定は `Asia/Tokyo` の 3 時）。`timezone` は定期タスクの予定（ARCHITECTURE 9.6）とログの日付の区切りにも使う |
| `[logs] retention_days`、`archive_interval_hours` | ログのローカル保持日数（既定 14）と退避の間隔（既定 4 時間） |
| `[tasks] timeout_minutes` | 管理 UI から実行するワンオフのコマンドと定期タスクの制限時間（既定 30 分、1〜1440） |

`[github]` を書かないと誰もログインできない（API は 401 を返す）。`koroku serve --insecure-no-auth` は開発機専用で、unit ファイルには書かない。

`listen`、`data_dir`、`master_key_file`、`[deploy]`、`[caddy] admin_url`、`access_log_dir` は例の値のままでよい。変える場合は `koroku.service` と `provision.sh` が前提にしているパス（`/etc/koroku`、`/var/lib/koroku`、`/var/log/caddy/apps`、`127.0.0.1:2019`）と合わせる。

## 6. 秘密情報の置き場所

| 秘密 | 場所 | 作り方 |
| --- | --- | --- |
| マスターキー（環境変数とレジストリのトークンの暗号化） | `/etc/koroku/master.key`（0600） | `koroku` が初回起動時に生成する。失うと保存済みの環境変数とレジストリのトークンを復号できない |
| GitHub App の秘密鍵 | `/etc/koroku/github-app.pem`（0600） | GitHub App の設定画面で発行して置く |
| GitHub App の client secret | `/etc/koroku/config.toml`（0600） | 同上 |
| オブジェクトストレージの Access Key ID / Secret Access Key | `/etc/koroku/config.toml`（0600） | バケット限定のアクセスキーを発行して書く |
| restic のリポジトリパスワード | `/etc/koroku/restic.key`（0600） | 自分で決めて置く。失うとバックアップを復元できない |
| private なイメージのレジストリ認証（アプリごと。推奨） | `koroku.db` に暗号化して保存。使うときに `/var/lib/koroku/apps/<slug>/docker/config.json`（0600）へ書き出す | 管理 UI のアプリの設定タブ「コンテナレジストリの認証」で入れる（管理以上のロールと再ログインが要る）。そのアプリのビルド・pull・起動にだけ使われ、ほかのアプリには見えない（ARCHITECTURE 6.1） |
| private なイメージのレジストリ認証（ホスト全体） | `/root/.docker/config.json` | VPS 上で `sudo docker login ghcr.io`。`koroku` は root で動くので、`sudo` を付けずに自分のユーザーでログインしても使われない。アプリごとの認証を設定していない全アプリが共有するので、メンバーにアプリを任せるときはアプリごとの認証を使う |
| TLS 証明書と ACME アカウント | `/var/lib/caddy`（Caddy が管理） | 自動。バックアップ対象外で、失っても再発行される |

`/etc/koroku` は 0700、`/var/lib/koroku` も 0700 で、どちらも root 専用。

## 7. VPS の外に控えるもの

ホストを失ったときに、これがないと復旧できない。パスワードマネージャなどに保存する。

- restic のリポジトリパスワード（`/etc/koroku/restic.key` の中身）
- オブジェクトストレージの endpoint、region、バケット名、Access Key ID、Secret Access Key
- GitHub App の秘密鍵（なくしても GitHub 側で再発行できる）

`master.key` と `config.toml` は基盤のバックアップ（`/etc/koroku`）に含まれるので、上の 2 つがあれば取り戻せる。バックアップが一度も成功していない間は `master.key` も控えておく。

## 8. 判断したことと理由

### 8.1 restic は公式リリースのバイナリを使う

Ubuntu 24.04 の apt が配る restic は 0.16.4（2024 年初めの版）で、24.04 の間は 0.16 系のまま変わらない（26.04 は 0.18.1）。`restore <snapshot>:<subpath>` と `backup --stdin --stdin-filename` は 0.16 でも使えるが、バックアップは「復元できること」がすべてなので、修正が入り続けている現行版に固定するほうを選んだ。OS の版によって restic の版が変わらないことも利点になる。

- `provision.sh` の `RESTIC_VERSION` と `RESTIC_SHA256` に版とチェックサムを固定し、ダウンロードしたファイルを照合してから `/usr/local/bin/restic` に置く。
- 欠点は、更新が自動で届かないこと。上げるときは 2 つの値を書き換えてスクリプトを再実行する（チェックサムはそのリリースの `SHA256SUMS` にある）。固定した版より新しいものが入っていれば、スクリプトは何もしない。
- systemd がサービスに渡す `PATH` は `/usr/local/bin` が先なので、apt 版が入っていても公式版が使われる。

### 8.2 ファイアウォールと HTTP/3

- ufw で TCP の 22、80、443 だけを許可する。既定のポリシー（受信は拒否、送信は許可）は Ubuntu の初期値のまま。既存のルールは消さない。
- **Docker が publish したポートは ufw を通らない。** アプリのポートを外から隠しているのは ufw ではなく、`koroku` が生成する override（全ポートを消すか `127.0.0.1` に限定する）である。
- **HTTP/3 は既定で使わない（`443/udp` は閉じたまま）。** 設定の再投入時に接続が保たれることを確かめたのは HTTP/1.1 と HTTP/2 だけで（ARCHITECTURE 8 章）、設計上の公開ポートも 22、80、443 としか決めていないため。Caddy 自体は HTTP/3 を有効にしたまま `Alt-Svc` で広告するが、UDP が届かなければクライアントは HTTP/2 で通信を続ける。開けると決めたら `--http3` を付けて再実行する。きれいに無効化するには、生成する Caddy 設定でプロトコルを `h1`、`h2` に絞る変更が要る（未対応）。

### 8.3 Caddy の起動方法

- Caddy の設定は `koroku` が admin API（`127.0.0.1:2019`）で丸ごと置き換える。パッケージ付属の `Caddyfile` は使わない。
- drop-in で `ExecStart` を `caddy run --environ --resume --config /etc/caddy/koroku-initial.json` に替えている。`--resume` があると、Caddy は最後に投入された設定（`/var/lib/caddy/.config/caddy/autosave.json`）で起動するので、Caddy だけ再起動しても、ホストを再起動しても、すぐに全アプリの経路が戻る。`koroku` も 5 分ごとに設定を投入し直すが、それを待たずに済み、`koroku` が止まっている間も経路が保たれる。
- `--config` の初期設定は、まだ一度も投入されていない最初の起動でだけ使われる。admin API をループバックに置くだけで、何も配信しない。
- `ExecReload` は空にしてある。パッケージの定義は `Caddyfile` を読み込むので、`systemctl reload caddy` で経路が消えてしまう。空にしたので `reload` はエラーになる。
- 保存された設定が原因で Caddy が起動しなくなったら、`sudo rm /var/lib/caddy/.config/caddy/autosave.json && sudo systemctl restart caddy` で初期設定に戻す。`koroku` が 5 分以内に投入し直す（`systemctl restart koroku` ならすぐ）。

### 8.4 `koroku.service` の停止動作

- アプリのコンテナは Docker デーモンの管理下にあり、`koroku` の control group には入っていない。**`koroku` を止めても再起動しても、アプリは止まらない。**
- `koroku` の control group にいるのは、`koroku` 自身と、その子プロセス（`docker compose` の `up` / `build` / `pull` / `logs` / `exec`、`git`、`restic`）だけ。
- `KillMode=mixed` にしている。停止時、SIGTERM は `koroku` だけに届く。`koroku` は実行中のデプロイを `failed (interrupted)` と記録し、子プロセスを自分で止めて終了する。`koroku` の終了後にまだ残っているプロセスがあれば systemd が SIGKILL する。子プロセスが残って次に起動した `koroku` と競合することがない。
- **デプロイ中に `koroku` を再起動すると**、実行中の `docker compose up` は打ち切られる。すでに作成・起動されたコンテナはそのまま残り、ロールバックは走らない（ARCHITECTURE 7 章）。新旧のコンテナが混ざった状態になりうるので、そのアプリをもう一度デプロイして揃える。**`koroku` の更新と再起動は、デプロイやバックアップが走っていないときに行う。**
- `TimeoutStopSec=90` は、`koroku` の終了処理（HTTP の停止に最大 10 秒、デプロイワーカーの待ち合わせに最大 45 秒）より長くしてある。
- `OOMPolicy=continue` は、メモリ不足で子プロセス（`restic` など）が 1 つ kill されたときに、unit 全体を止めさせないため。`koroku` は失敗したコマンドとして記録する。

### 8.5 `koroku.service` の hardening

`koroku` は root で動き、Docker ソケットを操作できる。これはホスト全体を操作できるのと同じなので、systemd のサンドボックスで得られるのは「誤動作の被害を減らす」程度である。壊れた unit は緩い unit より悪いので、確実に害のないものだけを入れた。

| 入れたもの | 理由 |
| --- | --- |
| `NoNewPrivileges=yes` | setuid や file capability による権限の獲得を禁じる。全プロセスがすでに root なので影響がない |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` | 使うソケットは Docker ソケット（unix）、GitHub・オブジェクトストレージ・Caddy・アプリへの TCP（IPv4/IPv6）、名前解決時の netlink だけ |
| `RestrictNamespaces=yes` | コンテナと名前空間を作るのは Docker デーモンで、`koroku` や docker CLI ではない |
| `SystemCallArchitectures=native` | 子プロセスはすべて x86-64 のバイナリ |
| `LockPersonality=yes`、`RestrictRealtime=yes` | どのプロセスも使わない |
| `CapabilityBoundingSet=~CAP_SYS_MODULE CAP_SYS_BOOT CAP_SYS_TIME CAP_SYS_RAWIO` | カーネルモジュールの読込、再起動、時刻変更、I/O ポートの直接操作は不要 |

| 入れなかったもの | 理由 |
| --- | --- |
| `ProtectSystem=`、`ProtectHome=`、`PrivateTmp=`、`ReadWritePaths=` | `koroku` と子プロセスは `/etc/koroku`（マスターキーの生成）、`/var/lib/koroku`、`/root/.docker`、`/root/.cache/restic`、`/var/lib/docker/volumes`（リストアの書き込み先）に書く。これらの指定はサービス専用のマウント名前空間を作るので、Docker が作るマウントとの相性を実機で確かめるまでは入れない |
| `UMask=0077` | `git` が取り出すファイルが 0600 になり、それがビルドコンテキスト経由でイメージに入る。root 以外で動くコンテナがアプリのファイルを読めなくなる。秘密のファイルは `koroku` が 0600 を明示して書き、`/var/lib/koroku` 自体が 0700 なので、umask に頼らなくてよい |
| `RestrictSUIDSGID=yes` | volume のリストアで setuid / setgid ビット（setgid ディレクトリはよくある）を再現できなくなる |
| capability のこれ以上の削減 | リストアは任意の所有者、モード、デバイスノード、拡張属性を再現する必要がある |
| `SystemCallFilter=`、`MemoryDenyWriteExecute=` | docker CLI、`git`、`restic` が使うシステムコールを網羅して確かめていない。漏れがあると子プロセスが理由の分かりにくい形で落ちる |
| `ProtectKernelTunables=`、`ProtectKernelModules=`、`ProtectControlGroups=`、`ProtectClock=`、`PrivateDevices=` | いずれもマウント名前空間かデバイスの制限を伴う。上と同じ理由 |
| `OOMScoreAdjust=` | 子プロセスにも引き継がれ、`restic` などがアプリのコンテナより kill されにくくなってしまう |

### 8.6 そのほか

- **needrestart**: スクリプト中の apt は `NEEDRESTART_MODE=l`（一覧表示のみ）で動かす。自動再起動に任せると、スクリプトの途中で Docker が再起動され、コンテナも巻き込まれるため。古いライブラリを使い続けているサービスは、最後に案内される再起動で入れ替わる。
- **Docker のログ設定**は、設定後に作られたコンテナから効く。既存のコンテナは次のデプロイで作り直されたときに切り替わる。
- **swap**: `/swapfile` を作るのは、有効な swap の合計が 2GB に満たないときだけ。OS イメージに最初から十分な swap があれば何も足さない。
- **zswap**: swap に書き出すページを、先に RAM の中で圧縮して持つ（圧縮は `zstd`、使うのは RAM の最大 20%（カーネルの既定））。アクセスの少ないアプリのヒープはよく縮むので、ディスクの swap より速く、同じ RAM に多く載る。カーネル 6.8 以降では `shrinker_enabled` で、圧縮したページのうち古いものを `/swapfile` へ追い出す。起動のたびに systemd-tmpfiles が `/sys/module/zswap/parameters` に書く。状態は `grep -r . /sys/module/zswap/parameters` で、使われ具合は `sudo grep -r . /sys/kernel/debug/zswap` で見られる。zswap のないカーネルでは何もせず、まとめで知らせる。
- **Caddy の `GOGC=50`**: drop-in の `Environment=GOGC=50` で、Caddy のヒープが前回の GC の 1.5 倍になったところで GC する（Go の既定は 2 倍）。常駐が 10 MiB ほど減る代わりに、CPU を少し多く使う。drop-in が変わったときは、スクリプトが Caddy を再起動する（`--resume` で経路はすぐ戻るが、開いていた接続は 1 度切れる）。
- **タイムゾーン**: システムのタイムゾーンは変えない。`tzdata` だけ入れる。

## 9. `koroku` の更新

デプロイやバックアップが走っていないことを管理 UI で確かめてから行う（8.4 節）。

```sh
# 手元
bin/dev make build
scp bin/koroku admin@vps.example.com:koroku.new

# VPS 上
sudo cp -p /usr/local/bin/koroku /usr/local/bin/koroku.prev   # 戻せるように控える
sudo install -m 0755 koroku.new /usr/local/bin/koroku
sudo systemctl restart koroku
systemctl status koroku --no-pager
journalctl -u koroku -n 50 --no-pager
```

- アプリのコンテナは影響を受けない。管理 UI と、開いているライブログの接続が数秒切れる。
- 新しい版で `config.toml` のキーが増減していないか、同じコミットの `deploy/config.example.toml` と見比べる。知らないキーがあると起動しない。
- `koroku.service` や Caddy の drop-in など `deploy/` の中身も変わった更新では、`deploy/` も送って `sudo ./deploy/provision.sh --skip-upgrade ./koroku.new` を実行する。変わったファイルだけを入れ替え、`koroku` を再起動する。
- 戻すときは `koroku.prev` を同じ手順で置き直す。ただし、新しい版が DB のマイグレーションを適用したあとは、古い版が動く保証はない。

## 10. 日常の保守

- **OS の更新**: Ubuntu の `unattended-upgrades` が入れるのはセキュリティ更新だけで、Docker と Caddy のリポジトリは対象外。月に一度ほど、デプロイのない時間に `sudo ./deploy/provision.sh ./koroku`（または `sudo apt update && sudo apt full-upgrade`）を実行する。**Docker が更新されるとデーモンが再起動し、全アプリのコンテナが数秒から数十秒止まる。**
- **再起動**: カーネル更新のあとは `sudo systemctl reboot`。起動後に `systemctl is-active docker caddy koroku` と管理 UI を確かめる。
- **状態の確認**: `systemctl status koroku caddy docker`、`journalctl -u koroku -f`、`sudo ufw status verbose`、`swapon --show`、`df -h /var/lib`。

### 10.1 アプリのコンテナで対話的にコマンドを実行する

`rails console` や `psql` のように端末が要るコマンドは、ホストで `koroku run` を使う（ARCHITECTURE 9.6）。管理 UI のタスクタブの「コマンドを実行」に、選んだサービスと入力したコマンドから組み立てた 1 行が出るので、コピーして SSH のセッションに貼る。

```sh
sudo koroku run blog web bin/rails console
sudo koroku run blog db psql -U postgres app
sudo koroku run blog web                       # コマンドを省くと sh
sudo koroku run blog web sh -c 'echo $DATABASE_URL'   # $ やパイプはコンテナの sh に渡す
sudo koroku run blog db psql -U postgres app < dump.sql   # 端末でない入力はそのまま渡る
```

- 稼働中のリリースと同じイメージ・環境変数・メモリ上限の新しいコンテナで動き、終わると消える。依存するサービス（DB など）は起動しないので、アプリが止まっていると DB につながらない。
- 管理 UI の実行履歴には残らず、出力もマスクされない。操作ロックも取らないので、実行中にデプロイ・停止・削除ができる。デプロイで入れ替わっても、このコンテナは起動したときのイメージのまま動く。
- 終了コードはコマンドのものを返す。
- `koroku` のバイナリを入れ替えたあと、`koroku` を再起動するまでは「schema is version N but this binary expects M」で断る。`sudo systemctl restart koroku` のあとに実行する。

### 10.2 アクセスがない間の休止

アプリの設定タブで「アクセスがない間は止める」を有効にしたアプリは、決めた時間 Caddy を通るリクエストがなければ koroku がコンテナを止め、次のリクエストで起こす（ARCHITECTURE 8.4）。

- アクセスは Caddy のアプリ別のアクセスログ（`[caddy] access_log_dir`、既定 `/var/log/caddy/apps`）から数える。このディレクトリがない設定（`access_log_dir = ""` か Caddy を使わない）では有効にできない。
- 休止中のアプリのコンテナは `docker ps -a` に `Exited` として残る。手で `docker compose start` や `docker start` で起動しないで、管理 UI の「起こす」か、アプリの URL を開く（koroku の DB が休止中のままだと、Caddy は起こす経路のままになる。起こす処理は動いているコンテナを見つけると、そのまま休止を解く）。
- 休止中のアプリのコンテナを手で消した場合は、次のリクエストで koroku が稼働中のリリースのファイルから作り直す。
- 休止させた・起こしたことは `journalctl -u koroku` に `app went to sleep` / `app woke up` として出る。起こせなかったときは `waking the app failed` と理由が出る。
- 休止と起こすたびに Caddy の設定を投入するので、ほかのアプリの WebSocket は最長 5 分後に 1 回切れる（アプリの起動・停止と同じ）。
- 休止中のアプリの定期タスクは、アプリを起こさずに、タスクのサービスが `depends_on` で依存するサービスだけを起動して実行し、終わると止め直す（10.3）。

### 10.3 定期タスク

タスクタブで登録したタスクに予定（cron の 5 つの欄）を書くと、koroku がその時刻に実行する（ARCHITECTURE 9.6）。

- 予定の時刻は `config.toml` の `timezone`（既定 `Asia/Tokyo`）で読む。ホストの OS のタイムゾーンには依らない。
- 全アプリで 1 件ずつ順に実行する。同じ時刻に重なった予定は待ち行列で順番を待つ。
- `koroku` が止まっていた（更新や再起動の）間の予定は、あとから実行しない。決まった時刻の更新を避けたいときは、予定の時刻を外して `systemctl restart koroku` する。
- 実行は実行履歴に「定期」として残る。見送った予定（アプリが停止中、前の回が実行中、デプロイなどが 10 分以上続いた、実行の前の検査に通らない）は、タスクの行に直近の 1 件が出る。`journalctl -u koroku` には `scheduled task started` / `scheduled task skipped` が毎回出る。
- 実行の途中で `koroku` が再起動すると、その実行は `interrupted` で失敗になる。休止中のアプリなら、起動時に依存先を止め直す。

### 10.4 レビューアプリ

staging アプリの pull request ごとのレビューアプリ（ARCHITECTURE 9.9、作る手順は README）は、アプリが増える機能なので、ホストの資源を見ながら使う。

- 数の上限も期限もない。pull request が閉じても消えないので、アプリ一覧（親の下に字下げして並ぶ）や親の概要タブで「マージ済み」「クローズ」のものを見つけて、設定タブから削除する。削除すると volume も消える。
- 既定でアクセスがない間は休止するので、開いていないレビューアプリはメモリを使わない。ディスク（イメージとリリース、volume）は消すまで残る。
- レビューアプリは日次のバックアップに入らない。レビューアプリが残っている親アプリは削除できない。
- ホスト全損から戻すとき（11 章）、レビューアプリは作り直すほうが早い（再デプロイすれば動き、データは親からコピーし直せる）。
- 作成のあとでホスト名が解決されないと画面が出したら、DNS にワイルドカードのレコードがないので、そのホスト名を登録する。
- `journalctl -u koroku` には、データのコピーを始めたとき `copying the parent's data into the review app`、始められなかったとき `the parent's data is not copied into the review app` が出る。

## 11. ホスト全損からの復旧

ARCHITECTURE 10.2 の「ホスト全損」の手順。必要なのは 7 章で控えたもの（restic のパスワード、オブジェクトストレージの接続情報）と、`koroku` のバイナリ。

1. 新しい VPS を用意し、3 章のとおりにプロビジョニングする。この時点の `config.toml` は例の値なので、`koroku` は起動していない。
2. 復元に必要な最小限の設定を新しいホストに置く。`/etc/koroku/config.toml` に `base_domain` と `[storage]`（7 章で控えた接続情報）を書き、`/etc/koroku/restic.key`（0600）に restic のパスワードを書く。
3. `sudo koroku restore-system` を実行する（`--config` の既定は `/etc/koroku/config.toml`）。最新の基盤スナップショットから、`koroku.db` を `/var/lib/koroku/` に、`/etc/koroku` にあったファイル（`master.key`、`github-app.pem` など）を `/etc/koroku/` に戻す。
   - 既にあるファイルは上書きしない。手で書いた `config.toml` と `restic.key` は残り、バックアップにあった版は隣に `<名前>.restored-<時刻>` として置かれる。`config.toml` は `.restored-` の版を元に、`public_ip` を新しい VPS の IP に直して使う。
   - `koroku.db` が既にあるホストでは実行を拒否する（新しいホスト用のコマンドのため）。`--force` を付けると既存のファイルを `<名前>.before-restore-<時刻>` に退避してから置き換える。`--snapshot <ID>` で最新以外のスナップショットを選べる。
   - `master.key` が戻っていることを確かめる（これが元の鍵でないと、保存済みの環境変数を復号できない）。
4. `sudo systemctl restart koroku` で起動し、管理 UI にログインする。DNS を切り替える前に確かめたいときは、手元の `hosts` に `console.<base_domain>` を新しい IP で書く。
5. 各アプリを再デプロイし、volume をスナップショットからリストアする。DB はダンプをダウンロードして流し込む。
6. DNS（`*.<base_domain>` と独自ドメインの A レコード）を新しい IP に向ける。証明書は Caddy が取り直す。
7. 独自ドメインは「再検証」で `active` に戻ることを確かめる。

- 証明書は取り直しになる。Let's Encrypt は同じホスト名の組み合わせの証明書を週に 5 枚までしか出さないので、復旧の練習を繰り返すときは `acme_ca` を staging にしておく。
- 最後のログ退避（4 時間ごと）以降のログと、最後のバックアップ（日次）以降のデータは戻らない。

## 12. 証明書のトラブル

`sudo koroku check` の `https` の行で、何が起きているかが分かる。

| 表示 | 原因 | 直し方 |
| --- | --- | --- |
| `[warn] ... serves a Let's Encrypt staging certificate` | `acme_ca` が Let's Encrypt の staging のまま（構築中の設定。SETUP 9 節） | 一通り動いたら `config.toml` の `acme_ca` の行を消し、`sudo systemctl restart koroku` |
| `[FAIL] ... still serves a Let's Encrypt staging certificate` | `acme_ca` は消したが、Caddy が手元にある staging の証明書を使い続けている（有効期限内の証明書は、設定を入れ替えても取り直さない） | `sudo systemctl restart caddy`。Caddy は `--resume` で最後の設定に戻り、本番の証明書を取り直す。数十秒後に `koroku check` をもう一度実行する |
| `[FAIL] ... certificate from an unknown authority` | Let's Encrypt 以外の証明書が返っている（手前に別のプロキシや CDN がある、DNS がほかのホストを向いている、など） | `dig +short <ホスト名>` がこのサーバーの IP だけを返すか確かめる。`journalctl -u caddy -n 100 --no-pager` を見る |
| `[FAIL] ... connection refused` / タイムアウト | DNS が向いていない、80/443 がどこかで閉じている、Caddy が止まっている | SETUP 2・3 節。`systemctl status caddy` |

- `sudo systemctl restart caddy` でも staging の証明書が残るときは、staging の証明書を消してから再起動する: `sudo rm -r /var/lib/caddy/.local/share/caddy/certificates/acme-staging-v02.api.letsencrypt.org-directory && sudo systemctl restart caddy`。
- 返っている証明書の発行者は `echo | openssl s_client -connect <ホスト名>:443 -servername <ホスト名> 2>/dev/null | openssl x509 -noout -issuer -dates` で確かめられる。staging なら発行者に `(STAGING)` が付く。
- Let's Encrypt の本番は、同じホスト名の組に週 5 枚までしか証明書を出さない。取り直しを何度も繰り返さない。

## 13. 他のサービスからデータを移す（Heroku Postgres）

Heroku の PostgreSQL のデータを、koroku で動かすアプリの PostgreSQL に移す手順。使う頻度が低いので、koroku の機能にはせず手作業で行う。

**前提**

- koroku 側のアプリを一度デプロイし、DB のサービス（例: `db`）が動いていること。DB のコンテナ名は `docker ps` で確かめる（`koroku-<slug>-<サービス>-1`。例: `koroku-blog-db-1`）。
- koroku 側の PostgreSQL のイメージの版が、Heroku 側と同じかそれより新しいこと。`pg_restore` は自分より新しい版のダンプを読めない。Heroku 側の版は `heroku pg:info -a <Heroku のアプリ名>` の `PG Version` で分かる。
- 流し込む前に、koroku のアプリの「バックアップ」タブで一度バックアップを取っておく（失敗したときに戻せるように）。

**手順**

1. 手元で、Heroku のバックアップを取ってダウンロードする（カスタム形式のダンプになる）。

   ```sh
   heroku pg:backups:capture -a <Heroku のアプリ名>
   curl -o latest.dump "$(heroku pg:backups:url -a <Heroku のアプリ名>)"
   ```

2. ダンプをサーバーに送る。

   ```sh
   scp latest.dump <ユーザー>@<サーバー>:
   ```

3. 書き込みが起きないよう、koroku の管理画面でアプリを止めるか、Web のサービスだけを止める（`sudo docker stop koroku-<slug>-web-1`）。DB のサービスは動かしたままにする。
4. サーバー上で流し込む。`<DB 名>` と `-U` のユーザーは、アプリの Compose（`POSTGRES_DB`、`POSTGRES_USER`）に合わせる。

   ```sh
   sudo docker exec -i koroku-<slug>-db-1 pg_restore --clean --if-exists --no-owner --no-acl -U postgres -d <DB 名> < latest.dump
   ```

   - `--no-owner --no-acl` は、Heroku 側のロール名を持ち込まないため。
   - Heroku の拡張（`pg_stat_statements` など）がないという警告は、アプリが使っていなければ無視してよい。
5. アプリを起動し直し、動作を確かめる。終わったら手元とサーバーの `latest.dump` を消す（個人情報を含みうる）。

**ダウンロードを挟まずに流し込む**

サーバーから Heroku の署名付き URL を直接読める。URL の有効期限は 60 分。

```sh
curl -sSL "<heroku pg:backups:url の出力>" | sudo docker exec -i koroku-<slug>-db-1 pg_restore --clean --if-exists --no-owner --no-acl -U postgres -d <DB 名>
```

## 検証済みの範囲

実機がないので、次の方法で確かめた（2026-11-18。SSH の手順は 2026-11-20、Ubuntu 26.04 は 2026-11-21）。

| 確認 | 方法 | 結果 |
| --- | --- | --- |
| シェルの構文と静的検査 | 使い捨ての `ubuntu:24.04` コンテナで `bash -n` と `shellcheck`（0.9.0） | 指摘なし |
| unit ファイル | `systemd-analyze verify`（systemd 255）。`koroku.service` と、パッケージの `caddy.service` に drop-in を重ねたもの | 警告なし |
| Caddy の初期設定 | `caddy validate`（Caddy 2.11.9） | `Valid configuration` |
| スクリプトの通し実行 | `deploy/test/dry-run.sh`。使い捨ての `ubuntu:24.04`（24.04.5）と `ubuntu:26.04`（26.04.1）のコンテナで、それぞれ 4 回続けて実行（4 回目は drop-in が古いホストの再実行） | 24.04 は 50 項目、26.04 は 49 項目（apt の restic が古いことの確認は 24.04 だけ）すべて成功 |

通し実行で確かめたこと:

- Docker、Caddy の apt リポジトリが 24.04（noble）と 26.04（resolute）の両方で解決でき、どちらでも Docker 29.10.0、Compose 5.8.0、Caddy 2.11.9 が入り、版の確認を通る。restic 0.19.1 はチェックサムが一致する。24.04 の Ubuntu 自身の apt が配るのは Caddy 2.6.2、restic 0.16.4。26.04 の apt の restic は 0.18.1。
- 既存の `daemon.json`（無関係な設定と別のログオプション入り）に合成され、元の設定が残る。壊れた `daemon.json` は上書きせずに止まる。
- 2 回目、3 回目の実行で、管理対象のファイル、鍵、権限が 1 バイトも変わらない。Docker と Caddy を再起動しない。swap と ufw を二重に有効化しない。
- Caddy の drop-in が古い（`GOGC` の行がない）ホストで再実行すると、drop-in を入れ替えて Caddy を再起動し、投入済みの設定で戻る。
- `config.toml` に例の値がある間は `koroku` を起動せず、書き換えたあとの実行で起動する。
- drop-in の `ExecStart` を `caddy` ユーザーでそのまま実行すると、初回は初期設定（admin API のみ、80 番は応答なし）で起動する。`koroku` が設定を投入したあとに Caddy を再起動すると、`koroku` が止まっていても投入済みの設定で戻る（`--resume`）。
- `caddy` ユーザーが `/var/log/caddy/apps` に書ける。
- SSH の keepalive は 1 回目から入り（鍵の有無に関係なく）、2 回目以降は書き換えない。
- root に鍵がない 1 回目は パスワードログインの設定に触れず、鍵を置いた 2 回目に drop-in を入れて reload し、3 回目は reload しない。

コンテナには systemd もカーネルも本物のネットワークもないので、zswap はその場では反映できず（「zswap がない」と知らせる経路を通る）、`systemctl`、`swapon`、`ufw`、`sshd`、`docker info` は呼び出しを記録するだけの代役に差し替えた。つまり、これらが実際に何をするかは確かめていない。

## 未検証（実機で確認が必要）

最初の VPS で、上から順に確かめる。

- [ ] **レビューアプリ**: 実際の GitHub App で pull request の一覧が読めること（Pull requests の権限の承認の前後）、作ったレビューアプリの GitHub Deployments が pull request の画面に URL 付きで出ること、head のブランチへの push で自動デプロイされること、親からのデータのコピーが最初のデプロイのあとに始まること。ユニットテストと API のテストは偽の GitHub と偽のコピーで確かめている。

- [ ] **スクリプトの通し実行**: 事業者の Ubuntu 24.04 と 26.04 のイメージで最後まで通るか（26.04 は、対応に加える前に `--force` を付けて最後まで通ることを確かめた。2026-11-21）。特に、初回起動直後の `unattended-upgrades` と apt のロックが競合したときの待ち合わせ、`full-upgrade` 中に対話プロンプトで止まらないこと。
- [ ] **カーネル更新**: `REBOOT REQUIRED` が表示されるか。再起動後に Docker、Caddy、`koroku`、アプリのコンテナ、swap が自動で戻るか。
- [ ] **`koroku.service`**: systemd の下で起動するか。hardening を付けた状態で、`docker compose` の `build` / `pull` / `up` / `logs` / `exec`、`git fetch`、`restic` の `backup` / `restore`（volume への書き戻しを含む）が通るか。
- [ ] **停止動作**: デプロイ中に `systemctl restart koroku` したとき、8.4 節のとおりになるか（デプロイは `failed (interrupted)`、子プロセスは残らない、アプリのコンテナは動き続ける）。
- [ ] **Caddy の drop-in**: パッケージ導入時の最初の起動が drop-in を使うか（使っていなければスクリプトが再起動する）。`Type=notify` と `--resume` の組み合わせ、Caddy のパッケージ更新時や再起動後に経路が戻ること。
- [ ] **ufw**: 有効化しても実行中の SSH セッションが切れないこと。Docker の iptables ルールと共存し、コンテナからの外向き通信とイメージのビルドができること。アプリのポート（`127.0.0.1` に bind）が外から届かないこと（受け入れシナリオ 4）。
- [ ] **事業者側のファイアウォール**: 既定で有効か、80/443 を開ける操作が要るか（事業者による）。
- [ ] **swap**: OS イメージに最初から swap があるか。`/swapfile` が再起動後も有効か。
- [ ] **zswap**: 事業者のカーネルに zswap と `zstd` があるか。スクリプトの実行直後と再起動後に `grep -r . /sys/module/zswap/parameters` が `enabled:Y`、`compressor:zstd` になるか。swap が使われたとき `/sys/kernel/debug/zswap/stored_pages` が増えるか。
- [ ] **Caddy の `GOGC`**: `sudo cat /proc/$(pidof caddy)/environ | tr '\0' '\n' | grep GOGC` で効いているか。システム画面で Caddy のメモリが減ったか。
- [ ] **Docker のログ設定**: 実際のデーモンが `daemon.json` を読み、新しいコンテナのログが 10MB × 3 で回るか（コンテナではデーモンを起動していない）。
- [ ] **証明書**: Let's Encrypt（staging、本番）での発行。`443/udp` を閉じた状態でブラウザが支障なく HTTP/2 で通信するか。
- [ ] **needrestart**: `NEEDRESTART_MODE=l` で、スクリプトの途中にサービスが自動再起動されないこと。
- [ ] **SSH の keepalive**: 実機で、放置したセッションが固まらず、回線が切れたクライアントのセッションが約 2 分で消えること。
- [ ] **SSH を鍵だけにする手順**: 実際の sshd が `01-koroku.conf` を `50-cloud-init.conf` より優先し、パスワードでのログインを断ること（別の端末から `ssh -o PubkeyAuthentication=no` で確かめる）。reload で実行中のセッションが切れないこと。Ubuntu 24.04 の `ssh.socket` による起動でも `systemctl try-reload-or-restart ssh.service` が効くこと。
- [ ] **アクセスがない間の休止**: 実際の Caddy で、休止中のアプリのホスト名が `koroku` に転送され（`X-Koroku-Wake`）、保護したアプリではゲートを通ったリクエストだけが起こすこと。起こしたあとの設定の投入で、起こしたリクエストと WebSocket がどうなるか。アクセスログが Caddy の書き込みからコレクタ経由で見張りに届き、ログのローテーションをまたいでも休止の判定が狂わないこと。Compose 2.30 の `docker compose start --wait`（開発環境の Compose 5.8.0 では結合テストで確認済み）。
- [ ] **`koroku check`**: 実際の DNS、Let's Encrypt（staging と本番）、GitHub、オブジェクトストレージに対して、各項目が期待どおりの結果になること。
- [ ] **SSH のポート検出**: `sshd -T` が待受ポートを返すこと（22 番以外にしている場合）。
- [ ] **`bin/dev make build` のバイナリ**: 通し実行で使ったのは、フロントエンドを含めずにビルドしたバイナリ。
- [ ] **restic 0.19.1 とバックアップ機能の組み合わせ**: バックアップの実装が入ってから、実際のオブジェクトストレージのバケットで `backup`、`restore <snapshot>:<subpath>`、`forget --prune`、`check` を確かめる。
- [ ] **復旧手順（11 章）**: `koroku restore-system` は、S3 互換サーバー（MinIO）と本物の restic を使った結合テストで「バックアップ → 別ディレクトリを新ホストに見立てて復元 → 復元した DB にアプリが入っている」まで確認した。実際のオブジェクトストレージと、新しい VPS での一連の手順（再デプロイ、volume のリストア、DNS の切り替え）は未確認。
