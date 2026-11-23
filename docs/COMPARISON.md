# ほかの選択肢との比較

koroku は「手元の `compose.yaml` を、自分の VPS 1 台で、安全柵付きで動かす」ことに絞ったホスティング基盤です。同じように自分のサーバーでアプリを動かす仕組みはほかにもあり、目的によってはそちらが合います。この文書は、選ぶときの手がかりとして、koroku と主なセルフホストの PaaS・デプロイツールの違いをまとめます。

ほかの製品の内容は、各製品の公式ドキュメントに書かれていることだけを載せています。どの製品も更新が速いので、決める前にリンク先で確かめてください。

## 一言で

| 製品 | 一言 |
| --- | --- |
| **koroku** | **手元の Compose を、自分の VPS 1 台に、安全柵付きで** |
| [Coolify](https://coolify.io/) | 何でも載る、多機能なセルフホスト PaaS |
| [Dokploy](https://dokploy.com/) | Coolify に近く、Docker Swarm で複数台に広げられる |
| [Dokku](https://dokku.com/) | `git push` と buildpack で、Heroku に近い使い方を 1 台に |
| [CapRover](https://caprover.com/) | Docker Swarm の上に、ワンクリックで入れるアプリと Web UI |
| [Kamal](https://kamal-deploy.org/) | Dockerfile のアプリを、手元の CLI から複数のサーバーへ無停止で |
| [Piku](https://github.com/piku/piku) | いちばん小さい `git push` の PaaS |

## koroku が向いている人

自分で書いたアプリを 3〜5 個持ち、1 人（か 2〜3 人）で運用している開発者です。

- アプリは手元ですでに `docker compose up` で動いている（Rails + PostgreSQL など）
- マネージドな PaaS では、アプリと DB の数だけ月額がかさむ。staging も持つと倍になる
- メモリ 4GB 程度の VPS 1 台にまとめたいが、手で組んだ運用で本番を壊したくはない
- インフラの面倒を見る時間は、なるべく短くしたい

## koroku を選ぶ場面

| 場面 | koroku がすること |
| --- | --- |
| 手元の `compose.yaml` をそのまま本番に持っていきたい | リポジトリの Compose ファイルを書き換えず、koroku が生成した override を重ねるだけ。手元と本番の差が小さい |
| 小さな VPS に staging と本番を詰めたい | 本体は Go のバイナリ 1 つと SQLite。サービスごとのメモリ上限、メモリが足りないときに本番を最後まで守る仕組み、アクセスがない間の休止がある |
| 壊れたデプロイで本番を落としたくない | 起動か HTTP の確認で失敗すると、直前の版に自動で戻す。権限の強い設定はデプロイ前の検査で断る。選べば、公開サービスを無停止で入れ替える |
| バックアップを「設定したつもり」で終わらせたくない | named volume、DB のダンプ、koroku 自身を毎日オブジェクトストレージへ退避し、週に 1 回、退避先を検査する。サーバーを丸ごと失っても `koroku restore-system` で戻せる |
| AI エージェントに運用を手伝わせたい | Claude Code の Skill が、リポジトリを koroku 向けに整え、読み取り専用のトークンでデプロイの失敗の理由を読み、メモリの使い方を詰める |
| Heroku から引っ越したい | staging と本番、本番への昇格、pull request ごとのレビューアプリ、環境変数の画面など、Heroku に近い使い心地 |

## ほかを選んだほうがよい場面

| 場面 | 候補 |
| --- | --- |
| 複数のサーバーに広げたい、負荷に合わせて増やしたい | Coolify、Dokploy、Kamal |
| 既製の OSS（アクセス解析、チャットなど）をテンプレートから立てたい | Coolify、CapRover |
| DB やワーカーも含めて、いつでも断なく入れ替えたい | Kamal（koroku の無停止は公開サービスだけ） |
| Docker を使わず、`git push` だけで済ませたい | Dokku、Piku |
| チームや複数の利用者で、1 つの基盤を分け合いたい | Coolify |

## 機能の比較

◯ はできる、△ は条件付きでできる（表の下に条件を書く）、— は提供していないか、公式ドキュメントで確かめられなかったもの。

| | koroku | Coolify | Dokploy | Dokku | CapRover | Kamal |
| --- | :-: | :-: | :-: | :-: | :-: | :-: |
| **使い方** | | | | | | |
| Web UI で操作する | ◯ | ◯ | ◯ | — | ◯ | — |
| リポジトリの Compose ファイルをそのまま使う | ◯ | △<sup>1</sup> | △<sup>2</sup> | — | △<sup>3</sup> | — |
| buildpack で Dockerfile なしにビルドする | — | ◯ | ◯ | ◯ | — | — |
| テンプレートから既製のアプリを立てる | — | ◯ | ◯ | — | ◯ | — |
| **規模** | | | | | | |
| 複数のサーバーに広げる | — | ◯ | ◯ | △<sup>4</sup> | ◯ | ◯ |
| 本体が DB サーバー（PostgreSQL、Redis）を常駐させない | ◯ | — | — | ◯ | ◯ | ◯ |
| **デプロイの安全** | | | | | | |
| 失敗したら自動で前の版に戻す | ◯ | — | △<sup>5</sup> | ◯ | — | ◯ |
| 前の版へ手動で戻す | ◯ | △<sup>6</sup> | ◯ | — | ◯ | ◯ |
| 無停止デプロイ | △<sup>7</sup> | △<sup>8</sup> | △<sup>9</sup> | ◯ | △<sup>10</sup> | ◯ |
| 権限の強い設定をデプロイ前に断る | ◯ | — | — | — | — | — |
| **データと運用** | | | | | | |
| アプリのデータ（volume、DB）をバックアップする | ◯ | △<sup>11</sup> | ◯ | △<sup>12</sup> | — | — |
| 基盤自身をバックアップから復旧する | ◯ | — | — | — | △<sup>13</sup> | — |
| 古いログをオブジェクトストレージへ退避する | ◯ | — | — | — | — | — |
| メモリが足りないときに本番を優先して守る | ◯ | — | — | — | — | — |
| アクセスがない間はアプリを休止する | ◯ | — | — | — | — | — |
| **連携と利用者** | | | | | | |
| GitHub への push で自動でデプロイする | ◯ | ◯ | ◯ | △<sup>14</sup> | △<sup>15</sup> | — |
| pull request ごとのレビューアプリ | ◯ | ◯ | ◯ | — | — | — |
| 複数の利用者に権限を分ける | △<sup>16</sup> | ◯ | ◯ | — | — | — |
| AI エージェント（Claude Code）向けの Skill | ◯ | — | — | — | — | — |

1. Compose を解析して、ラベル、ネットワーク、`restart` を足す。手を加えずに使う "Raw" の形もあるが、そのときは経路の設定を自分で書く。
2. デプロイ時に Traefik のラベルを足す。公開するサービスを共有のネットワークに入れる。
3. 実験的な機能。`image`、`environment`、`ports`、`volumes` など一部のキーだけを取り込み、`build:` は対応しない。
4. 本体は 1 台。k3s のスケジューラを使うと複数台に置ける。
5. Swarm で動かすアプリで、ヘルスチェックに失敗したとき。
6. Compose のアプリは対象が限られる。
7. アプリごとに選び、公開サービス（リクエストを受けるサービス）だけを入れ替える。DB とワーカーは作り直す。マイグレーションは古い版のコードと両立する形で書く。
8. Compose のアプリは対象外。
9. Compose を Swarm の stack として動かすとき（`build:` は使えない）。
10. volume のないアプリだけ。
11. Coolify の管理する DB を S3 互換のストレージへ退避する。
12. DB のプラグインごとに用意されている。
13. 基盤の設定だけ。アプリは再デプロイが要る。
14. Dokku のサーバーへの `git push` を受ける。
15. Webhook を受ける。
16. オーナー 1 人と、オーナーがアプリごとにロール（閲覧 / デプロイ / 管理）を与えたメンバー。チームや組織は持たない。

## マネージドな PaaS との違い

Heroku のようなマネージドな PaaS は、サーバーの面倒をまったく見なくてよい代わりに、アプリと DB ごとに料金がかかり、使える構成も PaaS の決まりに従います。koroku は、サーバー 1 台の料金で何個でも並べられ、手元の Compose の構成をそのまま使えます。その代わりに、サーバーの OS の更新、サーバーが止まったときの対応、バックアップの退避先の用意は自分で持ちます（[OPERATIONS.md](OPERATIONS.md)）。

## 参照した文書

- Coolify: [Docker Compose](https://coolify.io/docs/applications/build-packs/docker-compose)、[Rollbacks](https://coolify.io/docs/applications/deployments/rollbacks)、[Installation](https://coolify.io/docs/get-started/installation)、[Multi-server deployments](https://coolify.io/docs/core/infrastructure/scaling/multi-server-deployments)
- Dokploy: [Docker Compose の domains](https://docs.dokploy.com/docs/core/docker-compose/domains)、[Rollbacks](https://docs.dokploy.com/docs/core/applications/rollbacks)、[Volume backups](https://docs.dokploy.com/docs/core/volume-backups)、[Architecture](https://docs.dokploy.com/docs/core/architecture)
- Dokku: [Builder management](https://dokku.com/docs/deployment/builders/builder-management/)、[Scheduler management](https://dokku.com/docs/deployment/schedulers/scheduler-management/)、[Zero downtime deploys](https://dokku.com/docs/deployment/zero-downtime-deploys/)
- CapRover: [Docker Compose](https://caprover.com/docs/docker-compose.html)、[Zero downtime](https://caprover.com/docs/zero-downtime.html)、[Backup and restore](https://caprover.com/docs/backup-and-restore.html)
- Kamal: [kamal rollback](https://kamal-deploy.org/docs/commands/rollback/)、[Accessories](https://kamal-deploy.org/docs/configuration/accessories/)
- Piku: [README](https://github.com/piku/piku)
