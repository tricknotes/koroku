<img src="docs/assets/logo.svg" alt="" width="72" height="72" align="right">

# koroku

**The one-person PaaS** — *Platform as a Self-hosted application*

Your compose.yaml, on one VPS you own, with guardrails.

[English](README.md) | 日本語

koroku は、GitHub にある Docker Compose のアプリを、自分の VPS 1 台にブラウザーの管理画面からデプロイするホスティング基盤です。Heroku のような使い心地を、1 人で運用できる規模で、自分のサーバーに用意します。

> [!NOTE]
> v0.1.0 は実験的な最初の版です。設定や API は今後も変わることがあります。

## 解決する課題

- **PaaS の料金がアプリの数だけかさむ。** 小さな個人のアプリや staging でも、アプリと DB ごとに月額がかかります。koroku なら、サーバー 1 台の料金で 3〜5 個のアプリを並べられます。
- **VPS を手で運用すると壊れやすい。** `docker compose up`、リバースプロキシ、証明書、バックアップの cron、ログの置き場を自分で組むと、失敗したデプロイで本番が落ちたり、バックアップが実は取れていなかったりします。koroku はこれらを 1 つにまとめ、安全柵を付けます。
- **既存のセルフホスト PaaS は、手元の Compose と噛み合わないか、大きすぎる。** buildpack や独自の設定ファイルを前提にするものや、複数台向けで本体だけでメモリを多く使うものが多くあります。koroku はリポジトリの Compose ファイルを書き換えずに使い、本体は Go のバイナリ 1 つと SQLite で動きます。

## できること

- **デプロイ**: ブランチかコミットを選ぶか、GitHub に push すると、取得・ビルド・起動・HTTP の確認まで自動で進みます。起動に失敗したら、直前に動いていた版へ自動で戻ります。選べば、公開サービスを無停止で入れ替えます。
- **安全柵**: `privileged` や Docker ソケットのマウントなど権限の強い設定はデプロイ前に断り、アプリのポートは外に出さず、アプリ同士は互いに見えません。本番のアプリには、デプロイと削除の前に確認が入ります。
- **URL と TLS**: アプリごとに `https://<アプリ名>.<ベースドメイン>` と証明書が付きます。独自ドメインも足せます。
- **データの保護**: named volume、DB のダンプ、koroku 自身を毎日オブジェクトストレージへバックアップし、画面からリストアできます。サーバーを丸ごと失っても復旧できます。
- **小さなサーバーに詰める**: サービスごとのメモリ上限、メモリが足りないときに本番を最後まで守る仕組み、アクセスがない間の休止があります。
- **Compose のないリポジトリも動かす**: Compose ファイルを持たない public リポジトリは、fork せずに、管理画面に書いた Compose でデプロイできます。
- **Heroku に近い使い心地**: staging と本番、pull request ごとのレビューアプリ、環境変数、ログ、ワンショットのコマンドの実行を、同じ画面で扱えます。
- **AI エージェントと一緒に使う**: Claude Code の Skill が、アプリのリポジトリを koroku 向けに整え、デプロイの失敗の理由を読んで直します。

## 向いている使い方

- 自分が書いた 3〜5 個程度のアプリ（Web アプリ + DB など）を、1 台のサーバーで動かしたい
- アプリはすでに Docker Compose で動く（あるいは、動くように直せる）
- 運用するのは自分 1 人（と、アプリを任せる数人）

複数のサーバー、オートスケール、`git push` と buildpack でのデプロイ、既製のアプリのテンプレートが必要なら、ほかの選択肢が合います。違いは [docs/COMPARISON.md](docs/COMPARISON.md) にまとめてあります。

## しくみ

```text
            Internet
               | 80 / 443
+--------------v-------------------------------------- サーバー --+
|  Caddy          console.<ベースドメイン> -> koroku（管理画面）   |
|                 <アプリ>.<ベースドメイン> -> 各アプリ             |
|  koroku         API・管理画面・デプロイ・ログ・バックアップ        |
|                 docker compose を操作し、Caddy に経路を設定する   |
|  アプリ          1 アプリ = 1 Compose プロジェクト               |
+------------------------------------------------------------------+
       |                     |                          |
     GitHub          オブジェクトストレージ         Let's Encrypt
 （ログイン・取得）   （ログの退避・バックアップ）    （TLS 証明書）
```

特定のホスティング事業者、DNS サービス、オブジェクトストレージには依存しません。

## 必要なもの

| もの | 条件 |
| --- | --- |
| サーバー | Ubuntu 24.04 か 26.04（amd64）の VPS 1 台。目安は 2 vCPU、メモリ 4GB、ディスク 40GB。固定のグローバル IPv4 アドレスと、外から届く 80/443 番ポート |
| ドメイン | `*.<ベースドメイン>`（または管理画面とアプリごとの名前）を、サーバーの IP に向けられること |
| GitHub | 自分のアカウントに登録した GitHub App 1 つ |
| オブジェクトストレージ | S3 互換 API を持つもの。ログの退避とバックアップの置き場（なくても動きますが、バックアップが取れません） |
| 手元の環境 | `koroku` のバイナリをビルドするための Docker |

## はじめかた

1. 手元で `bin/dev make build` を実行し、管理画面込みの `bin/koroku` をビルドします。
2. サーバー、DNS、GitHub App、オブジェクトストレージを用意します。
3. サーバーにバイナリと `deploy/` を送り、`deploy/provision.sh` を実行します。
4. `/etc/koroku/config.toml` を書いて起動し、`sudo koroku check` で設定を確かめます。
5. `https://console.<ベースドメイン>` を開き、GitHub でログインします。

それぞれの手順の詳細は [docs/SETUP.md](docs/SETUP.md) にあります。

## セキュリティについて

koroku は Docker を操作できるので、サーバーの root と同じ力を持ちます。管理画面のログイン（GitHub の数値の ID で照合）が唯一の関門です。デプロイするのは自分と信頼できる人のリポジトリだけ、という前提で使ってください。詳しくは [docs/USAGE.md](docs/USAGE.md#セキュリティについて) と [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) の 10 章にあります。

## 文書

| 文書 | 内容 |
| --- | --- |
| [docs/COMPARISON.md](docs/COMPARISON.md) | ほかの選択肢との比較 |
| [docs/SETUP.md](docs/SETUP.md) | 初回セットアップ（サーバー、DNS、GitHub App、オブジェクトストレージ） |
| [docs/USAGE.md](docs/USAGE.md) | 管理画面の使い方、アプリのリポジトリを整える Skill |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | サーバーの構築、更新、復旧 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 設計 |
| [docs/API.md](docs/API.md) | HTTP API |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | koroku 自体の開発 |

## ライセンス

[MIT License](LICENSE) で公開しています。
