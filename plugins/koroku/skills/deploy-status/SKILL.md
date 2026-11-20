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
- アプリごとに、環境（`production` / `staging`）、稼働中のリリース、最新のデプロイを出す。最新のデプロイが `failed` か `canceled` なら、続けてデプロイログの末尾、サービスの状態、実行ログの最後の行、そのコミットで必要な環境変数のうち未設定のもの（キーだけ）を出す。

個別に読むときのサブコマンド:

| コマンド | 内容 |
| --- | --- |
| `koroku-status.sh apps` | すべてのアプリ |
| `koroku-status.sh deployments <slug>` | 直近 10 件のデプロイ |
| `koroku-status.sh log <id> [--lines N]` | デプロイログ（既定は全文） |
| `koroku-status.sh runtime <slug> [--service web] [--lines N]` | 実行ログの最後の N 行（既定 100） |
| `koroku-status.sh env <slug> [--ref <branch>\|--sha <sha>]` | 必要な環境変数と設定済みかどうか（値は出ない） |

エラーの意味: `the token was refused` はトークンの期限切れ・失効・打ち間違い（作り直してもらう）。`not available with a read-only token` はトークンで読めない API。`rate limited` は 1 分あたりの回数の上限（1 分待つ）。

### 2. 原因を見分ける

デプロイログは `==> <時刻> <手順> <表題>` の行で手順に分かれている。最後の `failed` の行と、その直前の手順の出力を読む。

| 失敗した手順 | よくある原因 | 直す場所 |
| --- | --- | --- |
| `fetch` | コミットが push されていない、submodule や LFS に頼っている | push する。submodule / LFS をやめる |
| `prepare` / `validate` | Compose の構文、ポリシー検査の違反（特権、ホストの名前空間、bind mount、`container_name` など）、`${VAR:?}` の必須変数が未設定 | Compose ファイル。未設定の変数は利用者に管理画面で入れてもらう |
| `build` | Dockerfile、依存の取得、`.dockerignore` の漏れ | Dockerfile とその周辺 |
| `pull` | イメージ名やタグの誤り、private イメージ | Compose の `image:` |
| `start` | コンテナがすぐ終了する、ヘルスチェックが通らない、ワンショットのサービスが誰にも待たれていない | ログの `Last output of the containers:` に続く行を読む |
| `check` | 公開サービスが `0.0.0.0:<port>` で待ち受けていない、ヘルスチェックパスが 5xx を返す | アプリの設定、ポート、ヘルスチェックパス |

- `rollback` の行があれば、koroku は直前の成功リリースに戻している（`Rolled back: deployment #N is running again.`）。本番が止まっているとは限らないので、そう伝える。
- 実行ログ（`runtime`）は、起動はしたが動かない場合（エラーを出して再起動を繰り返す、DB に繋がらない）の手がかりになる。

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
