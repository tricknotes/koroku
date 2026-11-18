# 運用手順

`koroku` を載せる VPS の構築、更新、復旧の手順をまとめる。設計の背景は [ARCHITECTURE.md](ARCHITECTURE.md) の 3 章（配置）、8 章（Caddy）、10 章（セキュリティと運用）を参照。

> この文書の手順は、実機の VPS ではまだ一度も通していない。確認できた範囲と、できていない範囲は末尾の「[検証済みの範囲](#検証済みの範囲)」と「[未検証（実機で確認が必要）](#未検証実機で確認が必要)」に分けて書いてある。

## 1. 全体の流れ

1. 手元で `koroku` のバイナリをビルドする。
2. バイナリと `deploy/` ディレクトリを VPS に送り、`deploy/provision.sh` を root で実行する。
3. DNS を向け、`/etc/koroku/config.toml` と鍵ファイルを用意する。
4. `koroku` を起動し、必要なら再起動（カーネル更新の反映）する。

対象は Ubuntu 24.04（amd64）の新規 VPS 1 台。それ以外の OS ではスクリプトが止まる（`--force` で続行できるが未確認）。

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
| `--force` | Ubuntu 24.04 amd64 以外でも続行する（未確認） |

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
| 10 | 2GB の swap ファイル `/swapfile` と `/etc/fstab` への登録。有効な swap が合計 2GB 以上あれば何もしない |
| 11 | `/etc/koroku` と `/var/lib/koroku`（どちらも 0700）、`/etc/koroku/config.toml`（なければ例から作成、0600）、`/usr/local/bin/koroku`、`koroku.service` |
| 12 | ufw で 22（と sshd の待受ポート）、80、443 の TCP を許可して有効化 |
| 13 | `koroku` を有効化。`config.toml` に例の値（`example.com`、`203.0.113.x`）が残っていれば起動はしない |
| 14 | 結果のまとめと、再起動が必要かどうかの表示 |

- **再実行できる。** 内容が同じファイルは書き換えず、変更がなければ Docker も Caddy も `koroku` も再起動しない。`koroku` のバイナリか unit が変わったときだけ `koroku` を再起動する。
- **上書きしないもの。** `/etc/koroku/config.toml`、`master.key`、`restic.key`、`github-app.pem` は、存在すれば中身にも権限にも触れない（権限が緩いときは注意を表示するだけ）。
- **再起動は自動でしない。** カーネルなどが更新されて再起動が必要なときは、最後に `REBOOT REQUIRED` と表示する。都合のよいときに `sudo systemctl reboot` する。Docker、Caddy、`koroku` は起動時に自動で立ち上がり、アプリのコンテナも `restart: unless-stopped` で戻る。
- **途中で失敗したら。** 失敗したコマンドと行番号が表示される。原因を取り除いて同じコマンドをもう一度実行すればよい。
- SSH を 22 番以外で待ち受けている場合、スクリプトは `sshd -T` の `Port` と実行中のセッションのポート（`SSH_CONNECTION`）も許可する。`sshd_config` 以外の方法でポートを変えているときは、実行前に `ufw allow <port>/tcp` しておく。

## 4. プロビジョニングのあとに人がやること

| 項目 | 内容 |
| --- | --- |
| DNS | `*.<base_domain>` の A レコードを VPS の IP に向ける。ワイルドカードが登録できなければ `console.<base_domain>` とアプリごとの `<slug>.<base_domain>` を個別に登録する |
| 事業者側のフィルター | VPS の手前にパケットフィルターがある場合は、そちらでも 80/tcp と 443/tcp を開ける |
| `config.toml` | 5 章のとおりに記入する |
| GitHub App の秘密鍵 | `/etc/koroku/github-app.pem` に置く（root 所有、0600） |
| restic のパスワード | `/etc/koroku/restic.key` に置く（root 所有、0600） |
| 起動 | `sudo systemctl restart koroku`、`journalctl -u koroku -f` でログを確認 |
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
| `[github] app_id`、`client_id`、`client_secret` | GitHub App の値。権限は Contents と Metadata の Read-only、callback は `https://<console_host>/auth/github/callback` |
| `[github] private_key_file` | `/etc/koroku/github-app.pem` |
| `[github] allowed_user_id` | ログインを許可する GitHub ユーザーの数値 ID（1 件） |
| `[github] require_two_factor` | 既定 `true`。GitHub の 2 段階認証が有効なアカウントでなければログインさせない。ログインが `two_factor_unknown` で断られる（GitHub が状態を返さない）ときだけ `false` にする（ARCHITECTURE 10.3） |
| `[storage] endpoint`、`region`、`bucket`、`access_key_id`、`secret_access_key` | オブジェクトストレージ（R2 なら endpoint は `https://<アカウント ID>.r2.cloudflarestorage.com`、region は `auto`）。バケットは先に作っておく（`koroku` も restic もバケットは作らない）。このセクションがないと、ログは VPS 内にだけ残り、バックアップは無効になる |
| `[backup] restic_password_file` | `/etc/koroku/restic.key`（0600）。中身は自分で決めたパスワード 1 行。ファイルがない、または他人が読める権限だとバックアップは無効になる |
| `[backup] hour`、`timezone` | 日次バックアップの時刻（既定は `Asia/Tokyo` の 3 時） |
| `[logs] retention_days`、`archive_interval_hours` | ログのローカル保持日数（既定 14）と退避の間隔（既定 4 時間） |

`[github]` を書かないと誰もログインできない（API は 401 を返す）。`koroku serve --insecure-no-auth` は開発機専用で、unit ファイルには書かない。

`listen`、`data_dir`、`master_key_file`、`[deploy]`、`[caddy] admin_url`、`access_log_dir` は例の値のままでよい。変える場合は `koroku.service` と `provision.sh` が前提にしているパス（`/etc/koroku`、`/var/lib/koroku`、`/var/log/caddy/apps`、`127.0.0.1:2019`）と合わせる。

## 6. 秘密情報の置き場所

| 秘密 | 場所 | 作り方 |
| --- | --- | --- |
| マスターキー（環境変数の暗号化） | `/etc/koroku/master.key`（0600） | `koroku` が初回起動時に生成する。失うと保存済みの環境変数を復号できない |
| GitHub App の秘密鍵 | `/etc/koroku/github-app.pem`（0600） | GitHub App の設定画面で発行して置く |
| GitHub App の client secret | `/etc/koroku/config.toml`（0600） | 同上 |
| R2 の Access Key ID / Secret Access Key | `/etc/koroku/config.toml`（0600） | バケット限定の API トークンを発行して書く |
| restic のリポジトリパスワード | `/etc/koroku/restic.key`（0600） | 自分で決めて置く。失うとバックアップを復元できない |
| private なイメージのレジストリ認証 | `/root/.docker/config.json` | VPS 上で `sudo docker login ghcr.io` |
| TLS 証明書と ACME アカウント | `/var/lib/caddy`（Caddy が管理） | 自動。バックアップ対象外で、失っても再発行される |

`/etc/koroku` は 0700、`/var/lib/koroku` も 0700 で、どちらも root 専用。

## 7. VPS の外に控えるもの

ホストを失ったときに、これがないと復旧できない。パスワードマネージャなどに保存する。

- restic のリポジトリパスワード（`/etc/koroku/restic.key` の中身）
- R2 の endpoint、バケット名、Access Key ID、Secret Access Key
- GitHub App の秘密鍵（なくしても GitHub 側で再発行できる）

`master.key` と `config.toml` は基盤のバックアップ（`/etc/koroku`）に含まれるので、上の 2 つがあれば取り戻せる。バックアップが一度も成功していない間は `master.key` も控えておく。

## 8. 判断したことと理由

### 8.1 restic は公式リリースのバイナリを使う

Ubuntu 24.04 の apt が配る restic は 0.16.4（2024 年初めの版）で、24.04 の間は 0.16 系のまま変わらない。`restore <snapshot>:<subpath>` と `backup --stdin --stdin-filename` は 0.16 でも使えるが、バックアップは「復元できること」がすべてなので、修正が入り続けている現行版に固定するほうを選んだ。

- `provision.sh` の `RESTIC_VERSION` と `RESTIC_SHA256` に版とチェックサムを固定し、ダウンロードしたファイルを照合してから `/usr/local/bin/restic` に置く。
- 欠点は、更新が自動で届かないこと。上げるときは 2 つの値を書き換えてスクリプトを再実行する（チェックサムはそのリリースの `SHA256SUMS` にある）。固定した版より新しいものが入っていれば、スクリプトは何もしない。
- systemd がサービスに渡す `PATH` は `/usr/local/bin` が先なので、apt 版が入っていても公式版が使われる。

### 8.2 ファイアウォールと HTTP/3

- ufw で TCP の 22、80、443 だけを許可する。既定のポリシー（受信は拒否、送信は許可）は Ubuntu の初期値のまま。既存のルールは消さない。
- **Docker が publish したポートは ufw を通らない。** アプリのポートを外から隠しているのは ufw ではなく、`koroku` が生成する override（全ポートを消すか `127.0.0.1` に限定する）である。
- **HTTP/3 は既定で使わない（`443/udp` は閉じたまま）。** 設定の再投入時に接続が保たれることを確かめたのは HTTP/1.1 と HTTP/2 だけで（ARCHITECTURE 12 章）、設計上の公開ポートも 22、80、443 としか決めていないため。Caddy 自体は HTTP/3 を有効にしたまま `Alt-Svc` で広告するが、UDP が届かなければクライアントは HTTP/2 で通信を続ける。開けると決めたら `--http3` を付けて再実行する。きれいに無効化するには、生成する Caddy 設定でプロトコルを `h1`、`h2` に絞る変更が要る（未対応）。

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
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` | 使うソケットは Docker ソケット（unix）、GitHub・R2・Caddy・アプリへの TCP（IPv4/IPv6）、名前解決時の netlink だけ |
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

## 11. ホスト全損からの復旧

ARCHITECTURE 10.2 の「ホスト全損」の手順。必要なのは 7 章で控えたもの（restic のパスワード、R2 の接続情報）と、`koroku` のバイナリ。

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

## 検証済みの範囲

実機がないので、次の方法で確かめた（2026-11-18）。

| 確認 | 方法 | 結果 |
| --- | --- | --- |
| シェルの構文と静的検査 | 使い捨ての `ubuntu:24.04` コンテナで `bash -n` と `shellcheck`（0.9.0） | 指摘なし |
| unit ファイル | `systemd-analyze verify`（systemd 255）。`koroku.service` と、パッケージの `caddy.service` に drop-in を重ねたもの | 警告なし |
| Caddy の初期設定 | `caddy validate`（Caddy 2.11.9） | `Valid configuration` |
| スクリプトの通し実行 | `deploy/test/dry-run.sh`。使い捨ての `ubuntu:24.04` コンテナで 3 回続けて実行 | 37 項目すべて成功 |

通し実行で確かめたこと:

- Docker、Caddy の apt リポジトリが 24.04 で解決でき、Docker 29.10.0、Compose 5.8.0、Caddy 2.11.9 が入り、版の確認を通る。restic 0.19.1 はチェックサムが一致する。Ubuntu 自身の apt が配るのは Caddy 2.6.2、restic 0.16.4。
- 既存の `daemon.json`（無関係な設定と別のログオプション入り）に合成され、元の設定が残る。壊れた `daemon.json` は上書きせずに止まる。
- 2 回目、3 回目の実行で、管理対象のファイル、鍵、権限が 1 バイトも変わらない。Docker と Caddy を再起動しない。swap と ufw を二重に有効化しない。
- `config.toml` に例の値がある間は `koroku` を起動せず、書き換えたあとの実行で起動する。
- drop-in の `ExecStart` を `caddy` ユーザーでそのまま実行すると、初回は初期設定（admin API のみ、80 番は応答なし）で起動する。`koroku` が設定を投入したあとに Caddy を再起動すると、`koroku` が止まっていても投入済みの設定で戻る（`--resume`）。
- `caddy` ユーザーが `/var/log/caddy/apps` に書ける。

コンテナには systemd もカーネルも本物のネットワークもないので、`systemctl`、`swapon`、`ufw`、`sshd`、`docker info` は呼び出しを記録するだけの代役に差し替えた。つまり、これらが実際に何をするかは確かめていない。

## 未検証（実機で確認が必要）

最初の VPS で、上から順に確かめる。

- [ ] **スクリプトの通し実行**: 事業者の Ubuntu 24.04 イメージで最後まで通るか。特に、初回起動直後の `unattended-upgrades` と apt のロックが競合したときの待ち合わせ、`full-upgrade` 中に対話プロンプトで止まらないこと。
- [ ] **カーネル更新**: `REBOOT REQUIRED` が表示されるか。再起動後に Docker、Caddy、`koroku`、アプリのコンテナ、swap が自動で戻るか。
- [ ] **`koroku.service`**: systemd の下で起動するか。hardening を付けた状態で、`docker compose` の `build` / `pull` / `up` / `logs` / `exec`、`git fetch`、`restic` の `backup` / `restore`（volume への書き戻しを含む）が通るか。
- [ ] **停止動作**: デプロイ中に `systemctl restart koroku` したとき、8.4 節のとおりになるか（デプロイは `failed (interrupted)`、子プロセスは残らない、アプリのコンテナは動き続ける）。
- [ ] **Caddy の drop-in**: パッケージ導入時の最初の起動が drop-in を使うか（使っていなければスクリプトが再起動する）。`Type=notify` と `--resume` の組み合わせ、Caddy のパッケージ更新時や再起動後に経路が戻ること。
- [ ] **ufw**: 有効化しても実行中の SSH セッションが切れないこと。Docker の iptables ルールと共存し、コンテナからの外向き通信とイメージのビルドができること。アプリのポート（`127.0.0.1` に bind）が外から届かないこと（受け入れシナリオ 4）。
- [ ] **事業者側のパケットフィルター**: 既定で有効か、80/443 を開ける操作が要るか。
- [ ] **swap**: OS イメージに最初から swap があるか。`/swapfile` が再起動後も有効か。
- [ ] **Docker のログ設定**: 実際のデーモンが `daemon.json` を読み、新しいコンテナのログが 10MB × 3 で回るか（コンテナではデーモンを起動していない）。
- [ ] **証明書**: Let's Encrypt（staging、本番）での発行。`443/udp` を閉じた状態でブラウザが支障なく HTTP/2 で通信するか。
- [ ] **needrestart**: `NEEDRESTART_MODE=l` で、スクリプトの途中にサービスが自動再起動されないこと。
- [ ] **SSH のポート検出**: `sshd -T` が待受ポートを返すこと（22 番以外にしている場合）。
- [ ] **`bin/dev make build` のバイナリ**: 通し実行で使ったのは、フロントエンドを含めずにビルドしたバイナリ。
- [ ] **restic 0.19.1 とバックアップ機能の組み合わせ**: バックアップの実装が入ってから、実際の R2 バケットで `backup`、`restore <snapshot>:<subpath>`、`forget --prune`、`check` を確かめる。
- [ ] **復旧手順（11 章）**: `koroku restore-system` は、S3 互換サーバー（MinIO）と本物の restic を使った結合テストで「バックアップ → 別ディレクトリを新ホストに見立てて復元 → 復元した DB にアプリが入っている」まで確認した。実際の R2 と、新しい VPS での一連の手順（再デプロイ、volume のリストア、DNS の切り替え）は未確認。
