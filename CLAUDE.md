# bulkhead

サプライチェーン攻撃の軽減を目的とした、開発環境のサンドボックス（Go 版 — 作り直し中）。
TS 版は隣の `../bulkhead-ts/`（退避済み。仕様・ADR・脅威モデルの正本はまだそちら）。

- 仕様案: [docs/rewrite-spec-draft.md](docs/rewrite-spec-draft.md) — 決定・未決（§12）・進め方と
  **実機で未確認のもの**（§13）。
- いまあるもの: 引数なしの TUI と、`create` / `configure` / `rename` / `up` / `down` /
  `restart` / `list` / `status` / `ssh` / `open` / `forward` / `logs` / `init` / `root-shell` / `key` /
  `remote` / `gpu` / `settings` / `var` / `secret` / `token` / `doctor` / `completion` / `path` /
  `snapshot` / `rollback` / `export` / `import` / `archive` / `unarchive` / `reset` / `destroy` /
  `migrate` / `version`。まだ無いもの（`prune`、`--json`）は仕様案 §13。
- 配布: `.github/workflows/`（`ci.yaml` は3つの OS でテスト、`release.yaml` はタグ `vX.Y.Z` で
  下書きのリリース）。バイナリを作るのは `scripts/build-release.sh` の1か所。アクションは
  `actions/*` だけを SHA で固定する。署名を入れるときの形は仕様案 §10。
- 構成:
  - `cmd/bh` — 引数の解釈と表示。各機能は「データを返すコア関数」と「表示する薄い CLI 関数」に
    分ける（TUI は `cmd/bh/tui.go` の backend からコア関数を呼ぶ）。
  - `internal/config` — 設定ディレクトリの読み書き（env.json・マシンの設定 settings.json・
    鍵レジストリ・トークンの記録・TS 版の読み取り）。
  - `internal/plan` — 設定 → コンテナ仕様の純関数。**境界の不変条件はここのテストが固定する。**
  - `internal/ops` — 起動ゲートつきの手順（up / down / restart / reset / destroy / 移行）と
    状態の要約。
  - `internal/engine` — Docker に触れる唯一の場所。`enginetest` はテスト用の偽物。
  - `internal/image` — 埋め込んだ `containers/` と環境の dev.Dockerfile からイメージを用意。
  - `internal/sshkey` / `internal/sshconfig` — マシン鍵とフィンガープリント / `~/.ssh` の
    `Host bulkhead-<name>` と Include 行。
  - `internal/forward` — ポート転送の書き方（Docker と同じ `このマシン:環境`、`host:` で逆方向）・
    ssh に渡す引数・名前つきの組のロック。local と remote を区別しない。
  - `internal/snapshot` — スナップショットのファイル形式（読み書きと検証）。
  - `internal/vault` — secret と token の値の置き場（OS の資格情報ストア、またはファイル）。
  - `internal/github` / `internal/huggingface` — token を出すサービスごとの知識（発行ページ・
    確認・失効）。サービスを足すときは `cmd/bh/token.go` の一覧にも足す。
  - `internal/doctor` — 検査（純関数。入力は設定とコンテナの実状態）。
  - `internal/tui` — 全画面の UI（Bubble Tea）。`Backend` インターフェース越しにしか動かない。
- 設定ディレクトリは `~/.config/bh`（`BH_CONFIG_DIR`）。TS 版の `~/.config/bulkhead`
  （`BULKHEAD_CONFIG_DIR`）とは別で、環境が移るのは `bh migrate` を通したときだけ。
  `~/.ssh` 側も TS 版と別（`~/.ssh/bh/config` と `# >>> bh >>>` のブロック）。
  開発中の検証では `BH_CONFIG_DIR` / `BULKHEAD_CONFIG_DIR` / `BH_SSH_CONFIG`（ssh の設定
  ファイルの場所）を作業用の場所へ向け、`BH_SECRET_STORE=file` を付ける（付けないと、secret と
  token の値が利用者のキーチェーンに入る）。`HOME` は変えない — docker context の解決が変わる。
- 環境へ渡す値は3つの経路（仕様案 §7）: var（env.json）/ secret / token。secret と token の
  **値は記録のファイルに書かない** — `internal/vault` 経由で読み書きし、`tokens.json` /
  `secrets.json` には値以外だけを置く。テストや検証の出力に、この作業環境自身の `GH_TOKEN` などを
  出さない（子プロセスには要る環境変数だけを渡す）。
- **データを消す操作は4つだけ。** ボリュームを消すのは `ops.removeVolumes` の1か所で、呼ぶのは
  `Destroy` / `Reset` / `Archive`（`internal/ops` のテストがソースを数えて確かめている）。
  `ops.Reset` は home と nix 以外のボリュームを断る（reset は2段階: 既定は nix だけ、
  `--home` で home も）。ボリュームの中身を変えるのは `engine.ReplaceVolume`（`bh rollback` と、
  作ったばかりのボリュームへ入れる `unarchive` / `import`）と `engine.RestoreFromImage`
  （reset の既定の段階が、home の中の決まったパスをイメージの内容へ戻す）。どれも確認つきで、
  rollback と archive は、頼りにするスナップショットを最後まで読み通してから消す（仕様案 §8）。
  migrate / undo / restart などほかの経路に足さない。移行の守り事は仕様案 §11。
- ボリュームの中身を消すスクリプトをこの環境で走らせるテスト（`internal/engine` の
  `TestReplaceScriptOnDirectories` / `TestRestoreScriptOnDirectories`）は、既定では動かない
  （`BH_RUN_REPLACE_SCRIPT=1` を付けたときだけ）。ファイルを消すので、人が承知して動かす。
- このリポジトリの作業環境には Docker が無いことがある。そのときは偽の Engine のテストまでで
  進め、実機で確かめていないものを仕様案 §13 の一覧に足す（`bh version` で接続を確かめられる）。

## 道具は flake の devShell から

Go などの道具はこのリポジトリの `flake.nix` で用意する（home-manager や `go install` で足さない）。

```
nix develop --command go test ./...
nix develop --command go vet ./...
nix develop --command govulncheck ./...
nix build            # result/bin/bh
```

実機の検証（Docker が要る。資源は `bulkhead-<id>-*` の名前で作られ、最後の `destroy` で消える）:

```
export BH_CONFIG_DIR=/tmp/bh-config BH_SSH_CONFIG=/tmp/bh-ssh/config    # ~/.ssh/config に触れない
nix develop --command go build -o dist/bh ./cmd/bh
./dist/bh create verify-a --port 23901
./dist/bh up verify-a
scripts/verify.sh ./dist/bh verify-a
./dist/bh destroy verify-a --yes
```

TS 版の環境の移行（`bh migrate`）は、`scripts/verify-migrate.sh before <env>` → 移行 →
`… after <bh> <env>` で、データに触れていないことを docker から確かめる（読むだけ）。

flake は git が追跡しているファイルしか見ない — 新しいファイルは `git add -N` してから。

## 依存を変えるとき（仕様案 §15）

- 公開から日の浅い版は採らない。上げる理由と見たものをコミットに残す。
- go.mod / go.sum を変えたら flake.nix の `vendorHash` も同じコミットで更新する
  （`pkgs.lib.fakeHash` に戻して `nix build` すると正しいハッシュが出る。据え置くと古い依存が
  使い回されてビルドが止まる）。
- `go run <外部>@latest` / `go install <外部>@latest` は使わない。
- Docker に触れるコードは `internal/engine` に閉じる。
