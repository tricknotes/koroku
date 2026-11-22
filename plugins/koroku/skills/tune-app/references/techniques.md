# 効く手と落とし穴

koroku で動いているいくつかの Rails / Rack アプリで実際に測った結果からまとめた。数字は手元の Docker での目安で、アプリによって変わる。必ず自分で測ってから入れる。

## 別プロセスのワーカーを Web サーバーの中で動かす

いちばん効く。Sidekiq や Solid Queue のワーカーを別サービスにすると、Rails がもう 1 つ丸ごと載る（150〜200 MiB ほど）。ジョブが少ない個人規模のアプリなら、Puma の中でスレッドとして動かせば十分なことが多い。

- **Solid Queue**: `config/puma.rb` で `plugin :solid_queue` と `solid_queue_mode :async`（fork しない）。環境変数（例 `SOLID_QUEUE_IN_PUMA`）があるときだけ読み込むようにして、ほかの環境の振る舞いを変えない。
- **Sidekiq**（7 以上）: 埋め込みモード（`Sidekiq.configure_embed`）。Sidekiq は Web と同じプロセスでは 2〜3 スレッドまでを勧めている。

  ```ruby
  if ENV["SIDEKIQ_IN_PUMA"]
    sidekiq = nil
    after_booted do
      sidekiq = Sidekiq.configure_embed do |config|
        config.queues = %w[default]
        config.concurrency = Integer(ENV.fetch("SIDEKIQ_CONCURRENCY", 2))
      end
      sidekiq.run
    end
    # Single mode runs this hook in the signal trap, where Sidekiq cannot take its locks.
    after_stopped { Thread.new { sidekiq&.stop }.join }
  end
  ```

  - シングルモードの Puma は停止のフックをシグナルのハンドラーの中で呼ぶ。そこでは Sidekiq がロックを取れず `ThreadError: can't be called from trap context` になるので、別スレッドで止めて待つ。
  - 埋め込みモードは `config/sidekiq.yml` を読まない。`max_retries` などはイニシャライザーの `Sidekiq.configure_server` に移す（`configure_server` のブロックは埋め込みでも適用される）。
- **DB の接続プール**を、ジョブのスレッドの数だけ増やす（`database.yml` の `pool` / `max_connections`）。
- Compose から worker サービスを消す。koroku は `up --remove-orphans` で起動するので、デプロイすると古いコンテナは消える。
- Web サービスのメモリ上限は、ジョブの分だけ上げる（例 512m → 640m）。消えたサービスの上限がなくなるので、合計は下がる。
- 重いジョブ（画像処理など）が Web と同じプロセスで動くようになる。その間は Web のメモリが増え、応答も遅くなりうる。利用者に伝える。

## 定期ジョブだけのために常駐しているプロセスをなくす

定期ジョブのために Rails を丸ごと読み込んだプロセス（`clockwork`、`rufus-scheduler`、`sidekiq-scheduler` だけを動かす worker など）が常駐していれば、それだけで 150〜200 MiB 使う。まず Compose のサービスを見て、何が定期ジョブを起動しているかを確かめる。

- **koroku の定期タスク**: 管理 UI のタスクタブで、登録したタスクに cron の予定（例 `0 3 * * *`）を書くと、koroku がその時刻に `docker compose run --rm --no-deps` で 1 回だけ実行する（prepare-app の requirements.md「定期タスク」）。常駐するプロセスがいらず、実行のたびに Rails の起動の時間とメモリがかかる。ジョブをその場で実行するコマンドにする（`bin/rails runner 'NightlyCleanupJob.perform_now'`。`perform_later` はキューに入れるだけで、ワーカーがいなければ実行されない）。数時間に 1 回や日次のような、間隔の長いジョブに向く。
- **アクセスがない間の休止を使うアプリ**: 休止の間は Puma もワーカーも止まるので、下の Puma の中のスケジューラー（Solid Queue の `recurring.yml`、Sidekiq の埋め込み、`rufus-scheduler`）はどれも止まり、時刻どおりに走らない。休止を使う（使いたい）アプリでは、定期ジョブは koroku の定期タスクに移す。休止中は、タスクのサービスの `depends_on` にある DB などだけが起動して実行されるので、DB を `depends_on` に書いておく。

- **Solid Queue**: `config/recurring.yml` に書いたジョブは、Puma の中で動かした Solid Queue（上の `plugin :solid_queue`）のスケジューラーがそのまま実行する。別のスケジューラーのサービスは要らない。同じジョブが 2 回入らないよう DB で調整されるので、プロセスが複数あっても重ならない。
- **Sidekiq**: `sidekiq-cron` や `sidekiq-scheduler` は Sidekiq のプロセスの中で動くので、埋め込みモード（上）で Puma の中に移せる。埋め込みでもスケジュールが読み込まれ、時刻どおりにジョブが入ることをログで確かめる。
- **`clockwork` / `rufus-scheduler` の常駐プロセス**: 次のどれかに移す。どれにするかは、ジョブの数と重さ、休止を使うかを見せて利用者に選んでもらう。
  - koroku の定期タスクに移す（上）。ジョブが数個で、どれも 1 回のコマンドで実行できるなら、Compose の変更がいちばん少ない。
  - ジョブキューの定期実行（Solid Queue の `recurring.yml`、`sidekiq-cron`）に書き換える。ジョブキューがすでにあるなら、これがいちばん少ない変更で済む。
  - `rufus-scheduler` を Puma の起動のフック（`after_booted`）で動かす。Puma のワーカーを fork する構成では、ワーカーの数だけ同じジョブが走るので、シングルモードのときだけにする。
  - `supercronic` などの cron を小さなコンテナで動かし、実行のたびに `bin/rails runner` を起動する。常駐は cron だけ（数 MiB）になる。代わりに、実行のたびに Rails の起動の時間とメモリがかかり、その間はホストのメモリの使用量が一時的に増える。数時間に 1 回のような、間隔の長いジョブに向く。koroku の定期タスクでも同じことができ、こちらは休止中も動き、実行の記録が管理 UI に残る。
- どれも、Web のプロセスと同じ場所でジョブが走る、またはジョブのたびに起動する。前のスケジューラーと比べて、実行の時刻（タイムゾーン、`TZ`）、同時に走らせない制御、失敗したときのやり直しが変わらないかを確かめる。アプリを止めている間は定期ジョブも動かないことも、利用者に伝える。
- まとめたスケジューラーのサービスは Compose から消す（`up --remove-orphans` で古いコンテナは消える）。

## Puma のスレッドとプロセス

- シングルモード（`WEB_CONCURRENCY` を 0 か未設定。ワーカーを fork しない）にする。ワーカーを増やすとメモリはほぼ倍になる。
- スレッドは Rails の既定の 3。5 から 3 にすると、負荷の後に残るメモリが少し減る。小さな Rack アプリなら 2 でもよい。1 にすると、外部の API の待ちなどで 1 つのリクエストが止まったとき、サイト全体が止まる。

## jemalloc と Ruby の GC

組み合わせて入れる。片方だけでは効かないことが多い。

```dockerfile
ENV LD_PRELOAD=/usr/local/lib/libjemalloc.so \
    MALLOC_CONF=narenas:2,dirty_decay_ms:1000,muzzy_decay_ms:0,background_thread:true \
    RUBY_GC_HEAP_GROWTH_FACTOR=1.1 \
    RUBY_GC_HEAP_FREE_SLOTS_MAX_RATIO=0.1 \
    RUBY_GC_MALLOC_LIMIT_MAX=8000000 \
    RUBY_GC_OLDMALLOC_LIMIT_MAX=16000000
```

- **`MALLOC_CONF` だけでは効かないことがある。** 負荷のあと Puma が暇になると Ruby の GC が走らず、ゴミがメモリを持ったままになる。jemalloc は、まだ確保されたままのページを OS に返せない。GC の設定で早めに GC を走らせると、そこで解放されたページを jemalloc の background thread が 1 秒ほどで返す。ある例では、`MALLOC_CONF` だけで 0%、GC の設定だけで約 15%、両方で約 25% 減った。
- jemalloc が設定を読んでいるかは、コンテナの中で `MALLOC_CONF=stats_print:true` を付けて `ruby -e 1` を走らせ、`opt.narenas` や `opt.dirty_decay_ms` の値を見る。
- 代わりに GC の回数が増える。ある例では、応答時間が p50 で約 1 割、p95 で 2〜3 割長くなった。`measure.sh --bench` で比べて利用者に見せる。
- ベースイメージが Debian 系なら `libjemalloc2` を入れて `/usr/lib/<arch>-linux-gnu/libjemalloc.so.2` を `LD_PRELOAD` に置く。

## YJIT

- Rails 7.2 以降は既定で YJIT を有効にする（`config.yjit`）。止めるには `config.yjit = false` を明示する。イニシャライザーを空にしても止まらない。動いているプロセスで `RubyVM::YJIT.enabled?` を見て確かめる。
- 止めると、ある例ではメモリが約 35 MiB 減り、応答時間が 1.4〜1.5 倍、スループットが約 7 割になった。`--yjit-mem-size` を 32 から 16 にすると、その中間だった。
- ほかの手で目標に届くなら残す。止めるなら性能の表を付けて利用者に判断してもらう。

## 依存を減らす（小さな Rack / Sinatra アプリ）

- ライブラリごとの常駐メモリを測る（`ruby -e 'require "x"; GC.start; puts File.read("/proc/self/status")[/VmRSS.*/]'` を、require の前後で比べる）。ある例では、Bundler 約 7 MB、Sinatra とその依存 約 15 MB、`net/http` と OpenSSL 約 10 MB、`rss` gem 約 13 MB だった。
- 重い gem を標準ライブラリで置き換える（例: RSS の読み取りを `rss` から REXML に）。
- 実行時に Bundler を読み込まない: `bundle install --standalone` と `BUNDLE_BIN=... bundle binstubs <server> --standalone` で、Bundler なしで起動する binstub を作る（`bundle binstubs --path` は今の Bundler にはない）。
- 本番の group にあって使っていない gem（以前のホスティング先専用のメトリクス、無効にした APM など）は、読み込まれるだけでメモリを使う。

## Go のサイドカー（Caddy など）

- `GOGC=50` で、ある例では約 13 MiB 減った。代わりに CPU が少し増える。
- `GOMEMLIMIT` や `GOGC` は、常駐が数 MB の小さなプロセスにはほとんど効かない。

## PostgreSQL

- `shared_buffers` を既定の 128MB から 32MB にすると、ある例では約 10 MiB 減った。データが大きいとクエリが遅くなりうるので、利用者の判断に残す。
- ヘルスチェックは `pg_isready -h 127.0.0.1` で TCP を確かめる。公式イメージは初回の起動時に、UNIX ソケットだけで待ち受ける仮のサーバーで初期化してから起動し直すので、ソケットで確かめると早すぎる。

## イメージを小さくする

- マルチステージにして、コンパイラ（`build-essential`）、Node.js と `node_modules`、ビルドのキャッシュを最終イメージに入れない。アセットのビルドに要る環境変数は、ビルドの段だけで仮の値を渡す。
- gem をインストールしたあと、`$BUNDLE_PATH/ruby/*/cache`、ネイティブ拡張のビルドにだけ要るファイル（`*.c`、`*.o`、`Makefile`、`mkmf.log`）、ビルド済み gem が同梱するほかの Ruby バージョン向けの拡張を消す。
- `bootsnap precompile` をビルドの段で走らせておくと、起動（とデプロイごとのマイグレーション）が速くなる。
- `.dockerignore` で `.git`、`log`、`tmp`、`node_modules`、テストのデータをビルドのコンテキストから外す。

## ビルドを速くする

- Ember（Embroider + Vite）のアプリは、`@embroider/vite` が `build.minify` 未指定のとき terser を 3 パスで使う。`build: { minify: 'oxc' }` にすると Vite 8 の Oxc で圧縮され、ある例では Vite のビルドが約 3 割速くなり、gzip 後の大きさは 0.35% 増えただけだった。
- 時間のほとんどがフロントエンドのビルドなら、ソースが変わっていないときにビルドを省く（Make の依存関係にするなど）。テストが同じ出力先に開発用のビルドを書くなら、それを検知してビルドし直す。
