# 作り直し版の仕様案（Go + Docker Engine API 直）

- Date: 2026-11-12
- 状態: **草案**（決定ではない。§12 の未決を決めてから ADR に起こす）
- 前提（2026-11-12）: 言語は Go を第一候補、Docker は CLI / Compose を介さず Engine API を
  直接呼ぶ。TS 版との互換は保証しないが、既存環境は引き継げるようにする。
- 決定 2026-11-12:
  - **override は廃止する**（いま使っている環境が無い。細かい設定を別の経路で受けるのは
    「あれば嬉しい」止まりで、最初の版には入れない — §5）。
  - **移行で引き継ぐのは設定とボリュームだけ**（コンテナは作り直しに対して安全 — §11）。
  - **bulkhead 自身がサプライチェーン攻撃の入口にならないこと**を設計の要件に加える（§15）。
    対策の軸は「悪い版を取り込む機会を減らす」と「取り込んだら気づく」。**依存の数を
    減らすことは主な対策にしない。**
  - Engine API のクライアントは Docker 公式の `moby/moby/client`、TUI は Bubble Tea を使う
    （保守されている・発行元の確かなものは使う）。
  - 開発環境は flake.nix の devShell で作る（home-manager には載せない — §14）。
  - TS 版のリポジトリは `bulkhead-ts` に改名して退避し、このリポジトリを `bulkhead` とする。
- Windows は実機で検証できる（ビルドを用意すれば）。クロスビルドは確認済み（§2）。
- TS 版の文書へのリンクは、隣に置いた TS 版の checkout（`../bulkhead-ts/`）を指す。
- 出発点（TS 版）: [tool-design](../../bulkhead-ts/docs/tool-design.md)（TS 版の仕様）、[threat-model](../../bulkhead-ts/docs/security/threat-model.md)、
  [rename-resource-id](../../bulkhead-ts/docs/research/rename-resource-id-2026-10-08.md)、[feature-recon](../../bulkhead-ts/docs/research/feature-recon-2026-10-01.md)、
  `.claude/ACTIVE_DESIGN.md` の「作り直し版で最初から設計する」4項目。

## 1. 変えないもの・変えるもの

**変えない**（境界そのもの）:

- 脅威モデルと不変条件5項目（threat-model「境界の前提条件」）。
- holder + dev の2コンテナ構成。holder だけが `NET_ADMIN` を持ち、dev は `cap_drop: ALL` +
  `no-new-privileges` で holder の netns に相乗りする。公開ポートは holder 側。
- `containers/` の中身（`firewall.sh` / `healthcheck.sh` / dev の Dockerfile / `bh-guest`）。
  言語に依らないのでそのまま持ち越し、バイナリへ埋め込む。
- 接続は SSH、ボリュームは workspace / home / nix + hostkeys、マシン鍵1ペア + 公開鍵レジストリ。

**変える**:

| 項目 | TS 版 | 作り直し版 |
| --- | --- | --- |
| 配布 | clone + `vp install` + checkout へのリンク | 単体バイナリ（`containers/` を埋め込み） |
| Docker の操作 | `docker` / `docker compose` の子プロセス | Engine API（Go クライアント） |
| 構成の記述 | 生成した compose.yaml + 利用者の override | 設定から組み立てたコンテナ仕様を API へ直接渡す。override は廃止 |
| 資源の識別 | 名前から導出 | id（名前と分離）+ 所有ラベル |
| ホスト → ゲストの受け渡し | 環境変数 + bind + volume 2本 | 読み取り専用の volume 1本に集約（§6） |
| データの退避 | backup / restore と export / import の2系統 | 1つのバンドル形式と配管（§8） |
| トークン | `github` と `token` の2系統 | 環境変数とシークレットの2メニュー、受け渡しは1本（§7） |
| remote | `bh remote ...` に別系統で集約 | 登録だけ `remote`、操作は local と同じコマンド（§9） |

## 2. 確認したこと（2026-11-12）

| 主張 | 根拠 | 確度 |
| --- | --- | --- |
| Go クライアントは `github.com/moby/moby/client`（旧 `github.com/docker/docker/client` は v29 で更新終了）。生成は `client.New(client.FromEnv)`、API 版の折衝は既定で有効 | [pkg.go.dev](https://pkg.go.dev/github.com/moby/moby/client)（v0.6.2、対応 API 1.40〜1.56） | 一次資料 |
| `FromEnv` が読むのは `DOCKER_HOST` / `DOCKER_API_VERSION` / `DOCKER_CERT_PATH` / `DOCKER_TLS_VERIFY` だけ。docker context（`~/.docker/config.json` の currentContext）は読まない | 同上（context の記述なし） | 一次資料に記載が無いことの確認。bh 側で解決する（下の行） |
| docker CLI の接続先の決まり方は **`DOCKER_HOST` > `DOCKER_CONTEXT` > config.json の currentContext > 既定**。context の実体は `<DOCKER_CONFIG か ~/.docker>/contexts/meta/<名前の sha256>/meta.json` の `Endpoints.docker.Host` | 実測（docker CLI 29.8.1 — 一時の設定ディレクトリに context を作り、環境変数の組み合わせで接続先を確認） | 実測。同じ順を `internal/engine` に実装しテストで固定。TLS つき・`ssh://` の context は未対応（エラーで止める） |
| `POST /containers/create` は `NetworkMode: container:<id>`・Healthcheck・DeviceRequests・GroupAdd・Sysctls・Tmpfs・Init・SecurityOpt・CapAdd / CapDrop・RestartPolicy・PortBindings を持つ | [Engine API v1.52 の OpenAPI 定義](https://docs.docker.com/reference/api/engine/version/v1.52.yaml) | 一次資料 |
| `POST /build` の `version` は `1`（classic・deprecated・既定）と `2`（BuildKit）。`session` 引数は OpenAPI 定義に載っていない | 同上 | 一次資料。BuildKit でローカルのコンテキストを送るにはセッションの実装が要る、は二次情報 — **要スパイク** |
| `GET /system/df` は `type` で対象を絞れる（volume だけ取れる） | 同上 | 一次資料 |
| 手元のデーモンは API 1.56・既定ビルダーは BuildKit（`_ping` の `Builder-Version: 2`） | 実測（Docker Desktop、Engine 29.9.3） | 実測 |
| 既存の workspace / home / nix / hostkeys ボリュームは compose のラベル4つ（project / volume / version / config-hash）を持ち、`_github` ボリュームはラベル無し。holder イメージは環境ごとに1つずつある | 実測（`docker volume inspect` / `docker image ls`） | 実測 |

**依存の規模の実測**（2026-11-12 — Go 1.26.10、`go version -m` でバイナリに入ったモジュールを
数えた。Engine への `_ping` だけを行う最小のプログラムと、ライブラリを import しただけのもの）。
**参考の数字で、選定の決め手にはしない**（決定 2026-11-12 — §15 の方針。公式クライアントと
Bubble Tea を使う）:

| 対象 | 入るモジュール（windows/amd64） | 備考 |
| --- | --- | --- |
| 公式クライアント `moby/moby/client` v0.6.2 | 20（ビルドリストは 48） | OpenTelemetry 5つ・containerd/errdefs・opencontainers・go-winio など |
| 標準ライブラリの `net/http` を unix ソケットへ向けた自前の実装 | 2（go-winio・x/sys）。Linux / macOS は 0 | named pipe のためだけに go-winio が要る |
| Bubble Tea v1.3.10 / v2.1.1（`charm.land/bubbletea/v2`） | 19 / 15 | |
| tview / tcell | 9 / 7 | |
| `golang.org/x/term` / `golang.org/x/crypto/ssh` | 2 / 1 | Go チームの保守 |

どちらの最小プログラムも linux/arm64・windows/amd64・windows/arm64・darwin/arm64 へ cgo なしで
ビルドでき（5〜7 MB）、linux 版は手元のデーモンに繋がった。**Windows と macOS での実行は未確認。**

Go の `os/exec` は 1.19 以降、カレントディレクトリへ解決された実行ファイルを `ErrDot` で拒否する
（Windows の `.\go.exe` も同じ — [pkg.go.dev/os/exec](https://pkg.go.dev/os/exec)）。
[ADR 0040](../../bulkhead-ts/docs/decisions/0040-windows-process-spawn.md) が TS 版で手当てした経路は標準ライブラリが
塞いでいる（PATH に空・相対の要素がある場合の扱いは実装時に確認）。

## 3. 資源モデル

環境は **id**（Docker 資源とデータの識別、作成後は不変）と **名前**（人が使う識別、変えられる）を
持つ。新規の id は短い乱数（12桁の16進）、TS 版から引き継いだ環境は **id = 旧来の名前**
（rename-resource-id の合意どおり — データ移行ゼロ）。

| 資源 | 名前 | 備考 |
| --- | --- | --- |
| ネットワーク | `bulkhead-<id>` | 環境ごとの bridge（リゾルバは実行時に読む — 落とし穴索引） |
| コンテナ | `bulkhead-<id>-holder` / `bulkhead-<id>-dev` | |
| ボリューム | `bulkhead-<id>_{workspace,home,nix,hostkeys,inject}` | 区切りは TS 版（compose 既定）と同じ `_`。引き継ぎのため変えない |
| イメージ | `bulkhead-holder:<資産ハッシュ>` / `bulkhead-dev-base:<資産ハッシュ>` / `bulkhead-<id>-dev:<ハッシュ>` | holder も共有にする（TS 版は環境ごとに同じものを作っていた） |

**ラベル**（新しく作る資源すべてに付ける。名前空間は未決 — §12）:

- `owner=<設定ディレクトリの ID>` — 同じデーモンを複数の設定ディレクトリが共有しても所有を
  判定できる（[ADR 0039](../../bulkhead-ts/docs/decisions/0039-remove-prune.md) の再導入条件）。
- `env=<id>` / `role=holder|dev|workspace|...`
- コンテナには `spec=<コンテナ仕様のハッシュ>` — 設定と実体のずれの検出と「作り直しが要るか」の
  判定に使う（compose の config-hash の役目）。

**ボリュームのラベルは作成後に変えられない。** 引き継いだボリュームには所有ラベルが付かない
ので、帰属は設定側（環境ディレクトリ）から辿る。一括掃除（prune）を戻す場合の対象は
「owner が自分で、どの環境からも参照されていない資源」だけにし、**ラベルの無い資源は名前が
`bulkhead-` で始まっていても消さない**（一覧に出して手で消してもらう）。

**環境ディレクトリの名前は id にする**（案 — 段階1 はこの形で実装した。リリース前なら
変えられる）: `environments/<id>/env.json` の中に `name` を持つ。

- 名前変更は env.json の1項目の書き換え（原子的書き込み）+ SSH 断片の再生成だけになり、
  ディレクトリの rename も Docker の操作も要らない。
- env.json が壊れても id は読めるので、broken 環境の資源を見失わない（rename-resource-id の
  「最重要の未決」が構造で消える）。壊れた環境の表示名は id になる。
- 代償: 手編集のとき、新規環境のディレクトリ名が読めない（`bh path <name>` で引く）。
  名前の重複は読み込み時の検証で止める。

### 設定ディレクトリの形（段階1 の実装）

```
<設定ディレクトリ>/
  bulkhead.json                  # {"version": 1, "id": "<12桁>"} — 作り直し版の印と owner ラベルの値
  environments/<id>/env.json     # 環境の設定。"version": 1 と "name" を持つ
  id_ed25519, id_ed25519.pub     # マシン鍵（bh が Go で生成。TS 版のものをそのまま使える）
  keys.json                      # 追加の公開鍵（TS 版と同じ形）
```

- 設定ディレクトリは **TS 版とは別の場所**（決定 2026-11-12）: `BH_CONFIG_DIR` > `%APPDATA%\bh` >
  `$XDG_CONFIG_HOME/bh` > `~/.config/bh`。TS 版の `BULKHEAD_CONFIG_DIR` は見ない（§11）。
- **`bulkhead.json` が無く環境がある設定ディレクトリは TS 版のものとみなし、読みも書きも
  しない**（別の場所にしたので普通は起きない。`BH_CONFIG_DIR` で TS 版の場所を指した場合の歯止め）。`version` の無い env.json は broken として
  一覧に出る。
- env.json は TS 版の項目（sshPort / publish / restart / authorizedKeys / gpu / cdiDevices /
  dockerSocket、remote は host / sshPort）に `version` と `name` を足したもの。知らない項目は
  エラー（手編集の打ち間違いを黙って無視しない）。書くときは全項目を書き出す。
- 読めない env.json は他の環境を止めない（broken として id で一覧に出る。`bh path <id>` と
  `bh destroy <id>` は効く）。名前の重複と local 同士の sshPort の重複は全体のエラー。

## 4. ライフサイクル（Engine API での手順）

設定からコンテナ仕様を組み立てる層は**純関数**にする（TS 版の `generate.ts` に相当。
不変条件に触れる項目はここのテストで固定する）。Docker に触れる層はインターフェースで分け、
フェイクで自動テストする。

**up**:

1. 設定を読み、検証する。コンテナ仕様とそのハッシュを組み立てる。
2. イメージを揃える（資産ハッシュのタグが無ければビルド。dev.Dockerfile があれば環境の
   イメージも）。
3. ネットワークとボリュームを揃える（無ければラベル付きで作る）。
4. inject ボリュームの中身をホストの状態に揃える（§6）。
5. holder: 仕様ハッシュが同じコンテナがあれば起動、違えば作り直して起動。
   **healthy になるまで待つ**。ならなければ holder のログ末尾を添えて失敗し、dev には触れない
   （compose の `depends_on: service_healthy` が担っていた起動ゲートを bh が持つ）。
6. dev: `NetworkMode: container:<holder の ID>` で作る。**holder を作り直したら dev も必ず
   作り直す**（参照先は ID）。holder を再起動しただけでも dev は起動し直す（netns が変わる）。

**down**: dev → holder の順に止める。**消さない**（[ADR 0041](../../bulkhead-ts/docs/decisions/0041-stop-keeps-containers.md)
は API 直でもそのまま成り立つ — 消すとボリュームが未使用に見える）。

**restart**: 止める → 両方消す → up。常に両方作り直す。`--rebuild [--pull] [--no-cache]` で
イメージも作り直す（TS 版の `rebuild` を吸収）。

**reset**: コンテナを消す → home と nix を消す → コンテナを作り直す（起動はしない）。

**destroy**: シークレットの失効 → コンテナ・ボリューム・ネットワーク・環境のイメージを消す →
環境ディレクトリを消す → known_hosts の該当行を掃除 → SSH 断片を再生成。

**起動中は変えられない設定**（TS 版の `LOCKED_WHILE_RUNNING`）は同じ線引きを保つ。

注意点:

- **restart ポリシーによる自動復帰では起動ゲートが評価されない**（threat-model — 実測で最大
  およそ 35ms の窓）。compose も内部では同じ `container:<id>` なので、API 直にしても
  良くも悪くもならない見込み。**要実測**。
- 端末を渡す操作（root-shell・ログの追従）は exec / logs の API を自前で端末につなぐ。
  raw モードと端末サイズの追従が要り、Windows のコンソールが一番の不確定要素。
- 対話の SSH は引き続きシステムの `ssh` を呼ぶ（`~/.ssh/config` の ProxyJump などを ssh 自身に
  解決させる）。鍵の生成と known_hosts の掃除は Go の中で行い、`ssh-keygen` への依存を外す。

## 5. override の廃止（決定 2026-11-12）

`compose.override.yaml` と `bh compose` は無くなる。いま override を使っている環境は無い。

- 得るもの: 「利用者の override で不変条件を無効化できる」（limitations）が消える。設定に
  書けることがそのまま bh の知っていることになる。
- 失うもの: 任意の compose 記述という逃げ道。
- **細かい設定を受ける別の経路は、最初の版には入れない。** 要るものが出てきたら、その項目を
  env.json の第一級項目へ昇格させる（GPU = ADR 0019、Docker ソケット = ADR 0029 と同じ流れ）。
  候補は追加のマウント（feature-recon ④ — 脅威モデルの見直しが先）と追加の公開ポート
  （holder 側に置く）。汎用の「任意の項目を書ける欄」は作らない — 不変条件を壊せる口が戻る。
- doctor の実状態検査は残す — docker CLI で手で作り直された場合と、bh 自身の不具合を拾うため。

## 6. ホストからゲストへの受け渡し（inject ボリューム）

TS 版は経路が4つある（環境変数 `SSH_AUTHORIZED_KEYS`・init.sh の bind・`_github` volume・
`_tokens` volume）。これを **読み取り専用でマウントする volume 1本**（`/etc/bulkhead/inject/`）に
まとめる案:

```
/etc/bulkhead/inject/
  authorized_keys     # sshd の AuthorizedKeysFile をここへ向ける
  init.sh             # 共通の初期化フック
  env                 # 1行に1本 NAME=value（環境変数とシークレット）
  github/token, meta  # git の credential helper と GH_TOKEN 用
```

- **鍵の付け外しがコンテナの作り直しなしで効く**（TS 版は環境変数なので「reboot to apply」）。
- **ホストのファイルシステムを bind しなくなる**（Docker ソケットのオプトインを除く）。
  Windows のドライブレターや、コンテナの中から bh を動かすときのパスの食い違いが消える。
- 向きはホスト → ゲストの一方向のまま（ADR 0035）。受け渡しの一覧が1行になる。
- 書き込みは TS 版と同じ「使い捨てコンテナ・`--network none`・中身は stdin」が実績のある方法。
  起動していないコンテナへ API でアーカイブを送る方法（プロセスを動かさずに済む）は**要スパイク**。
- sshd の StrictModes が volume 上の authorized_keys を受け付けるかは**要実機確認**。

## 7. 環境変数とシークレット

決定 2026-11-12 の方針（2つのメニューに分ける）に沿った案:

| | 環境変数 | シークレット |
| --- | --- | --- |
| 中身 | KEY / VALUE + コメント | サービス / VALUE + ユーザー名・コメント |
| 置き場所 | env.json（秘密ではない。複製・export に乗る） | `environments/<id>/secrets.json`（0600。複製・export に乗らない） |
| CLI | `bh var list / set / unset <env>` | `bh secret list / set / remove <env>` |
| ゲストへ | inject の `env` | inject の `env`（+ サービスごとの追加ファイル） |

GitHub は**シークレットのサービスの1つ**にまとめる（案）。サービスごとに持てる追加の能力を
決めておき、GitHub だけが全部を持つ: 発行画面の誘導（テンプレート URL）・検証（持ち主・期限・
リポジトリ）・失効（destroy / 置き換え時）・git の credential helper。Hugging Face は発行画面の
誘導だけ、custom は何も持たない。保護ではないこと（threat-model）は変わらない。

## 8. データの退避（1つのバンドル形式）

TS 版は同じ tar の配管を backup / restore と export / import が別々に使い、予定のアーカイブで
3つ目になる。**バンドル形式を1つ**（manifest + ボリュームごとの tar.gz。TS 版の export の
形式を版上げして使う）にし、置き場所と範囲の違いとして整理する:

| 操作 | 範囲 | 置き場所 |
| --- | --- | --- |
| `bh snapshot create / list / restore / remove <env>` | 3ボリューム（home + nix はペア） | 設定ディレクトリの `snapshots/<id>/` |
| `bh export <env> [path]` / `bh import <file> [name]` | 同じ（+ dev.Dockerfile） | 任意のファイル |
| `bh archive / unarchive <env>`（後回し） | home から抜粋したエージェントの状態だけを退避し、home と nix を消す | 同じ `snapshots/` |

- 設定を運ばない・arch 不一致は `--workspace-only` だけ、は TS 版の決定を保つ（ADR 0033）。
- **TS 版の export バンドルを新しい import が読めるようにする** — 引き継ぎ（§11）が
  使えない場合の逃げ道になる。

## 9. コマンド体系

名前はなるべく変えない。変わるのは統合したところだけ。

| TS 版 | 作り直し版 | |
| --- | --- | --- |
| `create` / `configure` / `list` / `status` / `up` / `down` / `logs` / `ssh` / `open` / `reset` / `destroy` / `doctor` / `key ...` / `gpu list` / `path` / `completion` / `root-shell` | 同じ | |
| `restart` / `rebuild` | `restart [--rebuild]` | 統合 |
| `init-run` | `init <env>` | 改名 |
| （なし） | `rename <env> <new>` | 新規 |
| `remote add / configure / remove` | 同じ | 登録の管理だけ残す |
| `remote list / status / ssh / open` | `list --all` / `status` / `ssh` / `open` | local と同じコマンドで扱う |
| `github ...` / `token ...` | `secret ...` / `var ...` | §7 |
| `backup` / `restore` | `snapshot ...` | §8 |
| `export` / `import` | 同じ | 形式は版上げ |
| `compose` | 廃止 | §5 |
| （なし） | `migrate` | §11。移行が済んだら撤去 |
| （なし） | `prune`（後回し） | §3 の条件で再導入 |

TUI は TS 版の並び（`src/ui/menus.ts` と tool-design）を仕様として移す。変わるのは
Settings → Tokens が Variables と Secrets の2つになること、Remote の行が local と同じ操作を
持つこと、環境メニューに Rename が入ること。更新は Engine API の events、ログは logs の
ストリームから取る（常駐の子プロセスが無くなる）。

## 10. 配布

- GitHub Releases に macOS / Linux / Windows × amd64 / arm64 の単体バイナリを置く
  （cgo なしでクロスビルド）。導入は「1ファイルを PATH に置く」だけ。
- `containers/` は埋め込み、イメージのタグを資産のハッシュにする。bh を更新すると次の up で
  新しいイメージが作られ、古いイメージのまま動いている環境は doctor が warn で示す
  （TS 版の「ビルドと checkout のずれの警告」と bh-guest の版の確認がこれに置き換わる）。
- 署名・公証（macOS / Windows）、自己更新、パッケージマネージャ対応は未決（最初は入れない）。
- **ビルドの経路が残る課題**（§12-2）。イメージのビルドだけは API 直が重い。

## 11. TS 版からの移行

**方針（決定 2026-11-12）: 引き継ぐのは設定とボリュームだけ。** ボリュームはコピーせず
そのまま使い、コンテナ・ネットワーク・イメージは作り直す。引き継ぎは明示のコマンド
（`bh migrate`）で行い、自動ではやらない。

**決定 2026-11-12（実装済み）: 1件ずつ移す。設定ディレクトリは別にする。ボリュームには触れない。**

- 作り直し版の設定ディレクトリは `~/.config/bh`（上書きは `BH_CONFIG_DIR`）。TS 版の
  `~/.config/bulkhead`（`BULKHEAD_CONFIG_DIR`）とは別で、互いのファイルを読まない。
  環境が一方から他方へ移るのは `bh migrate` を通したときだけ。
- `bh migrate` — TS 版の環境の一覧と、それぞれ移せるか（`ready` / `blocked: 理由`）。
- `bh migrate <name>` — 1件を移す。**やることを現状つきで表示し、確認してから**動く
  （`--yes` で省略）。
- `bh migrate --undo <name>` — TS 版へ返す。`bh destroy`（データを消す）を使わずに
  作り直し版から外す唯一の経路。

守ること（テストで固定 — `internal/ops/adopt_test.go` と `cmd/bh/migrate_test.go`）:

1. **ボリュームを消さない・作り直さない・コピーしない。** migrate と undo のどの経路にも
   ボリュームの削除は無い。TS 版だけが使っていた `_github` / `_tokens` も残し、
   「使わなくなったが残してある」と表示するだけ。
2. **ボリュームがどのコンテナからも参照されない瞬間を作らない。** 新しいコンテナを
   （起動せずに）先に作り、古いコンテナを外すのはその後。
3. **古いコンテナは、新しいコンテナが起動ゲートを通るまで消さない。** 通らなかったら
   新しい方を消し、古い方を holder → （healthy を待って）dev の順で起動し直して、
   「何も移していない」と報告する。やり直しは同じコマンド。
4. **TS 版のコンテナだと確かめてから触る。** 名前が合っていても compose のプロジェクト
   ラベルが違えば止まる。
5. **TS 版の設定ディレクトリで変えるのは1点だけ**: 移した環境のディレクトリを
   `environments/<name>` から `environments.migrated/<name>` へ移す（rename。削除なし）。
   TS 版からは見えなくなるので、同じ環境を新旧両方から起動する事故が起きない。
6. **途中で止まっても同じコマンドで続きができる**（Docker の入れ替え → 設定の書き込み →
   旧設定の退避、の順。どの段でも再実行できる）。

手順（local。remote は設定だけ）:

1. 移せるかの確認（下の「止める条件」）。Docker には触れない。
2. 表示と確認。
3. 鍵: 作り直し版にマシン鍵が無ければ TS 版のものを**写す**（元は残る）。既に別の鍵が
   あれば、TS 版の公開鍵を keys.json に登録してその環境に入れる（既存の SSH 設定が
   そのまま通る）。環境が参照している追加の鍵も keys.json へ写す。
4. イメージを揃える（環境は動いたまま）。
5. ネットワークと、新しい holder / dev を作る（起動しない）。id = 旧来の名前なので、
   ボリュームは同じ名前のものがそのまま使われる。
6. 動いていた場合: 古い dev → holder を止め（SSH のポートを空ける）、新しい方を
   起動ゲートつきで起動する。止まっていた場合: 何も起動しない（止まったまま）。
7. 古いコンテナとネットワークを消す。
8. `environments/<id>/env.json` を書き、TS 版のディレクトリを退避する。

止める条件（移さずに TS 版に残す）: `compose.override.yaml` がある / 参照している鍵が
keys.json に無い / 作り直し版に同じ名前・同じ id・同じ SSH ポートの環境がある /
env.json が読めない。

一緒に移るもの（2026-11-12 夜に追加 — **実機では未確認**）:

- 環境ディレクトリの中の利用者のファイル（`dev.Dockerfile` と、それが COPY するファイル）と
  トークンの記録（`github.json` / `tokens.json`）を、作り直し版の環境ディレクトリへ**写す**
  （元は退避先に残る）。TS 版が自分用に生成したもの（`compose.yaml` など）は写さない。
- トークンの中身は `_github` / `_tokens` ボリュームにあり、ボリュームはそのまま使うので
  何もしなくても付いてくる（マウント先も TS 版と同じ）。
- `init.sh`: 作り直し版にまだ無ければ TS 版のものを写す。既にあって中身が違えば写さず、
  その旨を表示する（以後に走るのは作り直し版のもの）。
- GPU の設定（`gpu` / `cdiDevices`）は env.json ごと移る。
- SSH 設定の `Host bulkhead-<name>` は作り直し版が自分の断片（`~/.ssh/bh/config`）に書く。
  TS 版の断片に残る同名の項目は、TS 版が SSH 設定を書き直すまで残る（先に読まれるのは
  作り直し版の Include）。

保たれるもの: sshPort と hostkeys が同じなので **known_hosts の警告は出ない**。

undo: 新しいコンテナとネットワークを消し（ボリュームは残す）、旧設定を
`environments/<name>` へ戻し、作り直し版の設定を消す。その後は TS 版の `bh up` で起動する。
**その間はボリュームがどのコンテナからも参照されない**ので、`docker volume prune -a` を
しないよう表示する。

実機で確認したこと（2026-11-12、compose で TS 版と同じ形の環境を作って）:

- 稼働中の環境の移行（約 5 秒）。workspace / home / nix の中身、SSH のホスト鍵、ボリュームの
  作成時刻が前後で同じ。古いコンテナとネットワークが消え、TS 版の一覧から消える。
- 新しい holder をわざと失敗させた移行 → 古いコンテナが動き直し、設定はどちらも元のまま。
- 移行した環境で `scripts/verify.sh` が 52 項目すべて通過。
- undo → ボリュームは無傷、旧設定が戻る。compose で起動し直せて、中身もそのまま。
- 停止中の環境の移行（止まったまま、新しいコンテナが作成済みでボリュームを参照）→ `bh up`。
- `dockerSocket: true` の環境の移行と起動（dev からソケットに届く。ほかの制限はそのまま）。

未確認: **TS 版が実際に生成した compose.yaml の環境**（このマシンからはホスト側の設定が
見えないため、同じ形を手で作って試した）。最初の1件は、消えても困らない環境で試すこと。

## 12. 未決（決める順）

1. **環境ディレクトリの名前を id にするか**（§3）。しない場合は rename-resource-id §4-1 の
   broken 環境の保護を別に決める。**段階1 は「id にする」で実装済み** — `docker ps` に出る名前が
   `bulkhead-<12桁>-dev` になる点（名前はホスト名と `bh list` で引く）を含めて確認が要る。
2. **ビルドの経路。**
   (a) ビルドだけ `docker buildx build` を呼ぶ — 実装は最小。docker CLI が前提として残る。
   (b) BuildKit のクライアントを組み込む — CLI が要らなくなる。
   (c) holder と dev の基底をビルド済みで配る — 利用者のビルドが dev.Dockerfile だけになる。
       non-goals の「独自のコンテナイメージ配布を行わない（当面）」を改めることになる。
   案: (a) で始め、ビルドをインターフェースの裏に置いて後から差し替えられるようにする。
3. **GitHub トークンをシークレットのサービスの1つにまとめるか**（§7）。
4. **ラベルの名前空間**（`bulkhead.*` か、逆 DNS 形式か）。
5. **新旧の併存中のコマンド名**（新版を一時的に別名で置くか）。
6. **CLI を先に作って TUI を後にする順でよいか。**
7. **依存の更新の待機日数**（§15-B。案: 7日）と、**Socket の GitHub App を入れるか**
   （リポジトリの持ち主のアカウントでの操作になる）。
8. **apt パッケージを snapshot.debian.org で固定するか**（§15-F）。
9. **エディタと devShell のつなぎ方**（§14 — direnv を入れるか、`nix develop` から起動するか）。

決まったもの（2026-11-12）: Engine API のクライアントは公式の `moby/moby/client`、TUI は
Bubble Tea。docker context の解決順は実測して実装済み（§2）。

## 13. 進め方（案）

| 段階 | 内容 | 終わりの目安 |
| --- | --- | --- |
| 0. スパイク | docker context の解決と接続 / holder → dev を API で起動して起動ゲートと verify.sh 相当を通す / ビルドの経路 / exec の端末接続（Windows 含む）/ inject への書き込みと sshd の StrictModes | §2 の「要確認」「要スパイク」が消える |
| 1. コア | 設定・コンテナ仕様（純関数）・Docker 層 + `create / up / down / restart / status / list / ssh / open / logs / doctor` + `migrate` + Releases への自動ビルド | 日常の運用を新版へ移せる |
| 2. 設定まわり | `configure` / `key` / `remote` / `var` / `secret` / `init` / `root-shell` / `reset` / `destroy` / `rename` | TS 版の CLI と同等 |
| 3. TUI | TS 版の並びを移す | TS 版の checkout を退避できる |
| 4. データ | `snapshot` / `export` / `import` / `archive` / `prune` | |

TS 版のテスト（compose 生成・doctor・events・設定スキーマ）は、段階1〜2 の仕様として読み替える。

段階0 のスパイクは Windows 向けにもビルドし、実機で流す（named pipe への接続・docker context・
exec の端末接続が Windows 固有の不確定要素）。

進捗（2026-11-12）:

- **docker context の解決と接続** — 実装済み（`internal/engine`、`bh version` が接続先と Engine の
  版を出す）。Linux の dev コンテナで、既定のソケットと docker context 経由の両方を確認。
- **holder → dev を API で起動** — 実装済み。コンテナ仕様は純関数（`internal/plan` — 不変条件を
  テストで固定）、手順は `internal/ops`（起動ゲート・down・削除。フェイクの Engine でテスト）、
  入口は一時コマンド `bh spike up / down / rm <id>`（設定ファイルを読む本物のコマンドができたら
  消す）。イメージは埋め込んだ `containers/` から作り、タグは中身のハッシュ。
- **実機での検証** — `scripts/verify.sh`（TS 版の verify.sh を名前指定に移したもの）が
  **49 項目すべて通過**（Docker Desktop / Engine 29.9.3、linux/arm64）。内容は、遮断4帯域と
  ホスト・兄弟コンテナへの到達不可（DROP の計数で確認）、WAN / DNS、dev の権限ゼロ、netns の
  共有、公開ポートが holder だけ、鍵でのログイン（注入していない鍵は拒否）、down / up で
  同じコンテナが戻る、停止中もボリュームが未使用に見えない、作り直してもボリュームと
  ホスト鍵が残る、など。
- **起動ゲートの実機確認** — 外へ出られないネットワークで holder の自己検証を失敗させ、
  `bh spike up` が holder のログつきで失敗し、**dev が作られない**ことを確認。
- **公開ポート経由の SSH** — Docker の VM 側のホストネットワークからログインできた。
  **ホスト OS のループバックからの接続は未確認**（この検証はコンテナの中から行った）。

スパイクで分かったこと:

- **holder だけが再起動すると、dev は前の名前空間に取り残される**（両方 running に見えるが
  通信できない）。TS 版は `bh restart` で作り直すしかなかった。作り直し版の up は
  「dev の起動時刻が holder より古い」ことで検出し、dev を入れ直す（テストで固定、実機で確認）。
- ビルドは `docker build -`（コンテキストを標準入力の tar で渡す）で足り、一時ディレクトリへの
  展開が要らない。TS 版と同じ Dockerfile なのでビルドキャッシュが効き、初回でも 17 秒だった。
- flake の `vendorHash` は、依存の中身が変わったのに据え置くとビルドが止まる（古い固定出力が
  使い回され、足りないパッケージで失敗する）。`pkgs.lib.fakeHash` に戻してビルドし、表示された
  ハッシュを入れる。

段階1 の進捗（2026-11-12）:

- **設定の読み書き**（`internal/config` — §3 の形）と **マシン鍵**（`internal/sshkey` — Go で生成。
  `ssh-keygen` への依存なし。秘密鍵だけ残っていれば公開鍵を導出し、作り直さない）。
- **コマンド**: `create` / `up` / `down` / `restart` / `list` / `status` / `ssh` / `path` /
  `destroy`（名前の入力で確認、`--yes` で省略）。一時コマンド `bh spike` は削除した。
- **まだ実現できない設定は up が止める**: `gpu`（off 以外）・`dockerSocket: true`・
  `dev.Dockerfile`。黙って無視して起動すると成功に見えるため。
- **状態の要約**（`running` / `starting` / `stopped` / `not created` / `degraded`）。`degraded` は
  「docker ps では生きて見えるが環境としては壊れている」もの: 片方だけ動いている、holder が
  unhealthy、dev が古い名前空間にいる、再起動を繰り返している。`bh up` が直す。
- **実機**: `scripts/verify.sh <bh> <env>`（bh の本物のコマンドで動かす形に改めた）が
  **52 項目すべて通過**。Go で生成したマシン鍵で、公開ポート経由のログインも通った
  （Docker の VM 側のホストネットワークから）。macOS での起動は利用者が確認済み。

段階1 で分かったこと:

- **既定の restart ポリシー（unless-stopped）だと、自己検証に失敗した holder が再起動を
  繰り返したまま残る**（スパイクは `no` 固定だったので見えなかった）。作り直し版の up は、
  起動ゲートが開かなかったら holder を止めてから失敗する（ログは残り、次の up でやり直せる）。
  テストで固定、実機で確認。
- 依存: `golang.org/x/crypto` v0.58.0（鍵の生成とフィンガープリント）。v0.59.0 は前日の公開
  だったので見送った（§15-B の待機）。govulncheck は x/crypto の openpgp（使っていない
  非推奨パッケージ）をモジュール単位で報告する — 呼び出しは無い。

- **migrate**（§11 — 1件ずつ、確認つき、undo あり）と **Docker ソケットのオプトイン**
  （`dockerSocket: true`）。ソケットの gid は Engine 側で実測する — TS 版は `alpine:3` を
  引いていたが、作り直し版は手元の holder イメージで `stat` するので、外から何も取らない。
  オプトインした環境は `up` の警告・`list` の NOTE・`status` の boundary 行で常に示す。

段階1〜3 の進捗（2026-11-12 夜 — **この節の内容は Docker の無い環境で書いた**）:

進め方の決定（2026-11-12 夜）: **TS 版にあった機能を、TUI を含めて先に移す。** §6（inject
ボリューム1本）と §7（var / secret への再編）は、実機でのスパイクが要る再設計なので後に回し、
いまは TS 版の仕組み（`_github` / `_tokens` ボリューム、`init.sh` の読み取り専用 bind、
`bh github` / `bh token`）をそのまま移した。移行した環境のトークンが何もせずに使い続けられる
のはその帰結。データの退避（§8 — snapshot / export / import / archive）と `prune` も後回し。

- **設定を編集するだけのコマンド**: `configure`（起動中は変えられない設定を止める）/ `rename`
  （新規 — id が変わらないので Docker の資源は動かない）/ `key`（レジストリ・attach / detach）/
  `remote add | configure | remove` / `create --from` / `down --all`。
- **SSH 設定**（`internal/sshconfig`）: `~/.ssh/bh/config` に `Host bulkhead-<name>` を書き、
  `~/.ssh/config` には印つきの Include 1行だけを足す。TS 版とは印もディレクトリも別。
  `open`（VS Code）はこの項目経由。`destroy` は known_hosts の該当行を消す。
- **コンテナの中へ届くもの**: `init.sh`（共通。雛形から作り、以後は触らない）/ `logs`（2つの
  コンテナを行単位で混ぜ、どちらの行かを示す）/ `init`（bh-guest 経由で今すぐ実行）/
  `root-shell`（特権つき root の exec + 端末の raw モード）。
- **GPU**（legacy / cdi。CDI はデーモンの一覧と突き合わせ、使い捨てコンテナへ実際に差して
  から、デバイスノードの gid を足す）と **dev.Dockerfile**（環境ごとのイメージ。トークンの
  記録と env.json はビルドコンテキストに入れない）、`restart --rebuild [--pull] [--no-cache]`。
- **トークン**: `github set | status | revoke` と `token list | set | remove`。ファイルの形と
  ボリュームの中身は TS 版と同じ。書き込みは holder イメージの使い捨てコンテナ
  （`--network none`、中身は標準入力）。
- **doctor**: ホストの準備と、コンテナの実状態に対する不変条件の検査（`internal/doctor` —
  純関数。期待値は `internal/plan` と同じ事実から）。**補完**（`completion zsh|bash`）。
- **reset**: home と nix を消して作り直す。ボリュームを消すコードは `ops.removeVolumes` の
  1か所で、呼ぶのは Destroy と Reset だけ（テストがソースを数える）。
- **TUI**（`internal/tui` — Bubble Tea v2）: TS 版の並び（一覧の4区画・状態を名前の色で示す・
  環境メニュー・Settings の Runtime / SSH・Maintenance・Keys・Machine key・確認ダイアログ・
  起動中は変えられない設定のグレーアウト・events 購読と r・下段のメッセージと m）。
  端末を明け渡すもの（ssh・logs・root shell・エディタ・再ビルド）は子プロセスへ渡して戻る。
  環境名を省いたコマンド（`up` / `down` / `restart` / `logs` / `status` / `ssh` / `open`）は
  端末なら選択を出す。
- **ディスク使用量**: `status` と TUI の詳細に出す（一覧では取らない）。

**実機での確認（2026-11-13、macOS・Docker Desktop 29.9.3・arm64）。**
`scripts/verify-features.sh <bh>` が、専用の設定ディレクトリに使い捨ての環境を作って機能を
確かめ、続けて同じ環境で `scripts/verify.sh`（境界）を流し、最後に消す。`75b6ee0` のビルドで:

- **境界（verify.sh）は 54 項目すべて通過** — `init.sh` の bind とトークン用のボリューム2つが
  増えた後の dev でも保たれている。
- 通ったもの: `init.sh`（初回に1回走る・中から書き換えられない）/ トークンのボリュームが中から
  書き換えられない / `logs`（印つきの行、follow の Ctrl-C）/ `status` の disk 行 / 鍵の付け外しと
  ssh の Host 項目 / dev.Dockerfile / `reset` / `rename` / `down --all` / `destroy` の後始末。
- 手で通ったもの: `root-shell`、`bh github set`、TUI の基本操作（macOS）。
- 見つかって直したもの:
  - doctor が正常な環境に FAIL を出した。macOS の一時ディレクトリは `/var/folders` →
    `/private/var/folders` のシンボリックリンクで、Docker Desktop は bind をリンクの先の
    パスで報告する。照合にリンクを解いたパスを足した。
  - `bh open` が、開いている VS Code を前に出すだけだった（開くフォルダを渡していなかった）。
  - TUI から始めた `bh logs` を Ctrl-C で止めると、TUI ごと終わった。
  - TUI から始めたプログラムが失敗すると、全画面がすぐ戻って出力が読めなかった（ssh の
    「ホスト鍵が変わった」の警告など）。失敗したときは Enter を待つ。
  - known_hosts がポートごとだったので、前に別の環境が使ったポートで警告が出た。環境の id の
    名前（`HostKeyAlias bh-<id>`）で覚えるようにした。
  - 値つきの行があるメニュー（Runtime など）が画面いっぱいに広がった。
  - スクリプト側の間違い2つ（`bh init` の再実行の行数の読み方、トークンの検査が飛ばされる）。

**修正後の再実行（同日、`07c050d` のビルド）: 55 項目通過・失敗 0**（飛ばしたのは手で見る
`bh github set` だけ）。境界は 54 項目すべて通過。これで初めて通ったもの: 起動直後の環境で
doctor が何も出さない / `bh token set` が新しいシェルに届き `remove` で消える（所有者 uid 1000・
モード 600）/ `bh init` の再実行。

**まだ実機で確かめていないもの。** (手) は手で見るしかないもの。

| 対象 | 確かめること |
| --- | --- |
| 上の修正 (手) | `bh open` が新しいウィンドウを開く / TUI の Logs から Ctrl-C で TUI へ戻る / 失敗した ssh の後で Enter を待つ / `HostKeyAlias` つきの項目で VS Code Remote SSH がつながる / メニューの幅 / 下段の並び（メッセージ → 操作説明） |
| GitHub トークン (手) | `revoke`、`destroy` 時の失効、git と gh から使えること。Windows のコンソールでの非表示入力 |
| restart --rebuild (手) | `--pull --no-cache` で作り直され、コンテナが新しいイメージで起動する |
| GPU (手・対応機のみ) | legacy / cdi の起動、`bh gpu list`、デバイスノードの gid |
| 補完 (手) | `eval "$(bh completion zsh)"` / bash で、コマンド・サブコマンド・環境名が補完される |
| events (手) | TUI を開いたまま別の端末で `bh up` / `bh down` → 一覧が追従する |
| migrate の追加分 (手) | dev.Dockerfile・トークン・init.sh を持つ TS 版の環境の移行 |
| Windows (手) | named pipe への接続、`init.sh` の bind（ドライブレターつきのパス。doctor の照合も）、root-shell、TUI |

作り直すときに入れる要望（2026-11-13）:

- **GitHub トークンの更新。** 設定済みのトークンを、同じ指定（preset・owner・リポジトリ）のまま
  発行し直す流れが要る。いまは `bh github set` を最初からやり直すしかない。§6・§7 の再設計と
  一緒に決める。

残り: データの退避（§8）/ `prune` / `--json`（形を決めてから — TS 版の形は compose の座標を
含んでいて、そのままは移せない）/ Releases への自動ビルド / §6・§7 の再設計 / ビルドの経路の
決定（§12-2）/ restart ポリシーでの自動復帰時の挙動の実測。

TS 版と違うところ（移植で変えたもの）:

- `rebuild` は `restart --rebuild`、`init-run` は `init`（§9 のとおり）。`remote list / status /
  ssh / open` は無く、local と同じコマンドで扱う。`logs` は compose への素通しをやめ、
  `--tail` / `--no-follow` / `--timestamps` を持つ。
- `bh up` / `bh restart` は起動の前に、記録にあるトークンを2つのボリュームへ書き直す（TS 版と
  同じ）。`destroy` は Docker 側を消した後で GitHub トークンを失効させる。
- doctor: compose.override.yaml と生成物の検査は無くなり、**コンテナの実状態を `internal/plan` が
  作るはずの仕様と比べる**。ホストのパスの bind は（init.sh とオプトインしたソケット以外）
  すべて fail。「ビルドと checkout のずれ」の警告は、イメージが今の bh の作るものと違うときの
  warn に置き換えた。
- TUI の GitHub トークンの発行は preset だけを選ぶ（owner と期限を変えるのは CLI の
  `bh github set --owner / --days`）。
- 環境名を省いたときの選択は、状態を引かずに名前と接続先だけを並べる。

## 14. 開発環境（flake.nix の devShell）

プロジェクトの道具はリポジトリの `flake.nix` に置き、利用者の home-manager には載せない。
役割分担は bulkhead 自身の方針（システム = dev.Dockerfile、利用者 = init.sh、日々の道具 = Nix）の
延長で、**プロジェクトごとの道具 = そのリポジトリの devShell**。

```nix
{
  description = "bulkhead";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [ "aarch64-linux" "x86_64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.govulncheck ];
          GOTOOLCHAIN = "local";      # go コマンドに別の版を取りに行かせない
          GOFLAGS = "-mod=readonly";  # ビルドの途中で go.mod を書き換えさせない
          CGO_ENABLED = "0";
        };
      });
      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
```

- 実測（2026-11-12、aarch64-linux の dev コンテナ）: この内容で `nix develop` が約1分で立ち、
  Go 1.26.10・gopls 0.22.0・govulncheck 1.3.0 が入った。
- inputs は nixpkgs だけにする（flake-utils などの補助 flake を足さない — 入力が増えるぶん
  信頼する先が増える）。`flake.lock` をコミットし、道具の版をそこで固定する。
- 使い方は `nix develop`（対話）か `nix develop --command go test ./...`（単発）。CI も同じ
  devShell を使い、手元と CI の道具を一致させる。
- 上の抜粋は devShell だけ。リポジトリの flake.nix には `packages.default`（`buildGoModule` +
  `vendorHash`）も入れてあり、`nix build` で `result/bin/bh` ができる（§15-A・§15-D）。
  検査の道具として capslock も devShell に入れた。Nix の利用者は flake から直接入れられる。
- Windows に Nix は無い（WSL を除く）。Windows 向けのバイナリは devShell からクロスビルドする。
- エディタは devShell の外で動くので、gopls を見つけさせる手当てが要る（§12-9）。

## 15. bulkhead 自身のサプライチェーン対策

bh はホストで動き、Docker ソケットに触れる（＝ホストの root 相当）。**bh が侵害されると
境界ごと無意味になる。**

**方針（決定 2026-11-12）: 依存の数を減らすことを主な対策にしない。** 保守されている、
発行元の確かなライブラリは使う（Docker 公式クライアント・Bubble Tea）。公式クライアントが
侵害されるときは Docker Desktop や docker CLI も同じ目に遭っている — 運命を共にしている先を
避けても得るものは小さく、自前で書き直せば不具合の入口が増える。軸にするのは次の2つ:

- **悪い版を取り込む機会を減らす**（§A・§B）
- **取り込んでしまったら気づく**（§C・§D）

攻撃面を小さくすること自体は良いので、新しい直接依存を足すときに理由を1行残す程度は続ける。

### A. 版を固定する（勝手に変わらない）

- **Go のモジュール**: go.mod は範囲ではなく版そのものを指し、明示して上げるまで同じ版が
  使われる。go.sum が中身のハッシュを持ち、チェックサムデータベース（sum.golang.org）が
  「世界中で同じ中身」を確かめる。
- **二重の固定**: flake の `vendorHash` が、ビルドで取得する全モジュールのハッシュを持つ
  （実装済み）。依存が1つでも変わるとビルドが止まるので、変えた事実がコミットに必ず残る。
- **道具**: Go・gopls・検査の道具は flake.lock で固定した nixpkgs から（§14）。
  `GOTOOLCHAIN=local` で go コマンドに別の版を取りに行かせない。
- **それ以外の入力**: GitHub Actions はコミットの SHA、コンテナのベースイメージは
  ダイジェスト、ビルド中に取ってくるものは sha256（§F）。

これで、新しいコードが入ってくる入口は**「依存を上げる変更」だけ**になる。

### B. 取り込む機会を減らす

- **公開されたばかりの版を採らない。** 悪意のある版の多くは数日のうちに見つかって取り下げ
  られる。Dependabot の `cooldown`（gomod と github-actions に対応。何も書かなくても3日）を
  長めに設定する（日数は §12-7）。セキュリティ更新は待機の対象外。
- **更新は PR 経由だけ。自動マージしない。** まとめて、頻度を下げて行う — 取り込む回数
  そのものが機会になる。
- **依存のコードが動く場面を限る。** Go にはインストール時に走るスクリプトが無い（npm の
  postinstall に当たるもの）。動くのは、ビルドした成果物・テスト・`go generate` の中だけ。
  `go run <外部>@latest` / `go install <外部>@latest` は使わず、道具は flake から入れる。
- **依存を取得してビルド・テストする場所を、Docker ソケットの無い環境にする**（§G）。

### C. 検出する — 依存を上げるとき

PR に対して自動で走らせ、人が読むのは結果にする（個人で依存の差分を全部は読めない前提）。

| 手段 | 何が分かるか | 状態 |
| --- | --- | --- |
| **Socket**（GitHub App） | 依存の変更に含まれる既知の悪意あるパッケージ・怪しい挙動 | Go 対応は 2025-04 に一般提供。PR の走査が Go で効くかは導入時に確認。Socket Firewall の無料版は Go 非対応 |
| **capslock**（Google） | パッケージが持つ「能力」（ネットワーク・ファイル・プロセスの起動・unsafe など）。**版の間の差分を取れる** — 「表示用のライブラリが急にネットワークへ出るようになった」を機械的に拾う | devShell に入れた（0.3.2）。bh に流して能力の一覧が出るところまで確認。基準を保存して CI で差分を見る形は未実装（結果は OS ごとに変わる） |
| **govulncheck** | 既知の脆弱性を、実際に呼んでいる経路つきで | devShell に入れた。流した結果は下 |
| GitHub の dependency review | PR で増減した依存と、その既知の脆弱性 | 未設定 |
| 人が読む | 直接依存の更新は変更履歴とリリースの差分 | 運用 |

govulncheck を試しに流した結果（2026-11-12）: Go 1.26.10 の標準ライブラリに1件
（GO-2026-6859 — HTTP/1 サーバ側の問題で、bh はクライアントとしてしか使っていない。
Go 1.26.11 で修正済み）。flake.lock の nixpkgs が新しい Go を持てば消える。依存モジュールの
既知の脆弱性は 0 件。

### D. 検出する — ビルドと配布物

- **再現できるビルド**: 同じコミットを手元の `nix build` で作り、配布物とハッシュを突き
  合わせる。CI が改ざんされても食い違いで分かる。同じマシンで作り直して一致することは
  確認済み。**別のマシンや CI との一致は未確認。**
- 配布物に**来歴の証明**（どのコミットを・どのワークフローが作ったか）を付ける。GitHub の
  artifact attestation と `gh attestation verify` を想定（機能の名前と設定は実装時に確認）。
- リリース後の差し替えを禁じる設定、タグと main の保護、アカウントの2要素認証（パスキー）。
- 配布物に入っている依存の版は `go version -m` で読める。govulncheck は配布物も走査できる。

### E. 動いているとき

- **自己更新と更新の確認は入れない。** bh が外へ出す通信は、シークレットの検証・失効で
  GitHub の API を呼ぶときだけ（と、利用者が頼んだ発行画面をブラウザで開くこと）。
- capslock の結果を bh 自身にも使う: 「ネットワークに出るパッケージ」の一覧を基準として
  持ち、増えたら CI で止める。

### F. コンテナに入るもの

- ベースイメージ（`debian:trixie-slim` / `alpine:3.22`）は**タグではなくダイジェストで固定**する。
- ビルド中に取ってくるものは sha256 で固定する。grml の zshrc は固定済み。**Nix の
  インストーラは TS 版では固定していない**（`containers/dev/Dockerfile` — 取ってきてそのまま
  実行している）ので固定する。
- ソケットの gid の実測に使っている `alpine:3`（動くタグを別に pull している）をやめ、
  手元でビルド済みの holder イメージを使い回す。
- apt のパッケージは版を固定していない（Debian のアーカイブ署名を信頼している）。
  snapshot.debian.org で日付を固定する案は、保守の手間との兼ね合いで未決（§12-8）。
- 埋め込んだ `containers/` のハッシュをイメージのタグにする（§10）ので、「このバイナリが
  作ったイメージか」は名前で分かる。

### G. 開発環境

- bulkhead の開発には Docker が要るので、開発用の環境は Docker ソケットをオプトインした
  **隔離対象外**の環境になりやすい。そこで依存を取得したり道具を動かしたりすると、侵害が
  そのままホストへ届く。
- Docker に触れる層をインターフェースで分ける（§4）のはこのためでもある: **フェイクで回る
  単体テストと日常の編集、依存の更新は、ソケットの無い通常の環境で行う。** 実機に触れる
  テストだけを、ソケットありの環境（か CI）に分ける。
- PR は原則受け付けない方針（TS 版の README）を保つ。fork からのワークフロー実行に
  シークレットを渡さない。

### H. 守れないもの

- Go のツールチェーン・nixpkgs・GitHub の侵害。
- **Docker 本体と公式クライアント** — bulkhead は Docker を信頼の土台にしている（§方針）。
- 作者のアカウントの侵害（正規のワークフローから悪意のある版が出る — 来歴の証明では
  防げない。手元のビルドとの突合と、更新を急がない運用で被害を減らす）。
