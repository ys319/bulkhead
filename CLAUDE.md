# bulkhead

サプライチェーン攻撃の軽減を目的とした、開発環境のサンドボックス（Go 版 — 作り直し中）。
TS 版は隣の `../bulkhead-ts/`（退避済み。仕様・ADR・脅威モデルの正本はまだそちら）。

- 仕様案: [docs/rewrite-spec-draft.md](docs/rewrite-spec-draft.md) — 決定・未決（§12）・進め方（§13）。
- いまあるもの: `create` / `up` / `down` / `restart` / `list` / `status` / `ssh` / `path` /
  `destroy` / `migrate` / `version`。まだ無いもの（open・logs・doctor・gpu・dev.Dockerfile・
  シークレットほか）は仕様案 §13。
- 構成: `cmd/bh`（引数の解釈と表示だけ）/ `internal/config`（設定ディレクトリの読み書き）/
  `internal/sshkey`（マシン鍵とフィンガープリント）/ `internal/plan`（設定 → コンテナ仕様の
  純関数。不変条件はここのテスト）/ `internal/ops`（起動ゲートつきの手順と状態の要約）/
  `internal/engine`（Docker に触れる唯一の場所。`enginetest` はテスト用の偽物）/
  `internal/image`（埋め込んだ `containers/` からイメージを用意）。
- 設定ディレクトリは `~/.config/bh`（`BH_CONFIG_DIR`）。TS 版の `~/.config/bulkhead`
  （`BULKHEAD_CONFIG_DIR`）とは別で、環境が移るのは `bh migrate` を通したときだけ。
  開発中の検証では両方の変数を作業用の場所へ向ける。
- **ボリュームを消すコードは `ops.Destroy` の1か所だけ**（`bh destroy` — 名前の入力で確認）。
  migrate / undo / restart などほかの経路に足さない。移行の守り事は仕様案 §11。

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
export BH_CONFIG_DIR=/tmp/bh-config
nix develop --command go build -o dist/bh ./cmd/bh
./dist/bh create verify-a --port 23901
./dist/bh up verify-a
scripts/verify.sh ./dist/bh verify-a
./dist/bh destroy verify-a --yes
```

flake は git が追跡しているファイルしか見ない — 新しいファイルは `git add -N` してから。

## 依存を変えるとき（仕様案 §15）

- 公開から日の浅い版は採らない。上げる理由と見たものをコミットに残す。
- go.mod / go.sum を変えたら flake.nix の `vendorHash` も同じコミットで更新する
  （`pkgs.lib.fakeHash` に戻して `nix build` すると正しいハッシュが出る。据え置くと古い依存が
  使い回されてビルドが止まる）。
- `go run <外部>@latest` / `go install <外部>@latest` は使わない。
- Docker に触れるコードは `internal/engine` に閉じる。
