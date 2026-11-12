# bulkhead

A sandbox for development environments, aimed at mitigating supply-chain attacks. Connect over
SSH and use it from VS Code, Claude Code, and the like.

Personal project — issues are welcome, PRs are generally not accepted. The documentation is
written in Japanese. MIT licensed.

サプライチェーン攻撃の軽減を目的とした、開発環境のサンドボックス。環境は Docker コンテナへ
隔離し、SSH で接続して使う。コマンド名は **`bh`**（引数なしで全画面の TUI、サブコマンドで CLI）。
対象は Windows / macOS / Linux。

**これは Go で作り直している途中の版**。TypeScript 版から機能を移している最中で、
実機で確かめていない部分がある — いまの状態は
[docs/rewrite-spec-draft.md](docs/rewrite-spec-draft.md) の §13。

## 前提

- **Docker**（Docker Desktop など）。bh は Docker Engine の API を直接呼ぶ。Docker Compose は
  使わない。イメージのビルドだけは `docker` コマンドを呼ぶので、PATH に必要。
- **ssh クライアント**（`ssh` と `ssh-keygen`）。

## 導入

配布物（単体のバイナリ）はまだ無い。手元でビルドする:

```
nix build                 # result/bin/bh（Nix がある場合）
```

または、Go 1.26 以降で:

```
go build -o bh ./cmd/bh
```

できた `bh` を PATH の通った場所へ置く。ほかに置くものは無い（コンテナの定義はバイナリに
埋め込んである）。

```
bh version       # ビルドと、接続先の Docker Engine
bh               # TUI
```

## 使い方

```
bh create web    # 環境を定義する（設定を書くだけ）
bh up web        # 起動する（初回はイメージをビルドする）
bh ssh web       # シェルに入る
bh open web      # VS Code で開く
bh down web      # 止める（コンテナとデータは残る）
```

`bh` だけで起動すると、同じ操作を全画面のメニューから行える。コマンドの一覧は `bh help`。

設定は `~/.config/bh`（Windows は `%APPDATA%\bh`。`BH_CONFIG_DIR` で変えられる）に置かれる。
環境のデータは Docker のボリュームにあり、**消すのは `bh destroy` と `bh reset` だけ**。

## TypeScript 版から移る

TypeScript 版の設定（`~/.config/bulkhead`）と環境はそのまま残る。環境は1つずつ引き取る:

```
bh migrate           # 引き取れる環境の一覧
bh migrate <name>    # 何をするかを示し、確認してから引き取る
```

ボリュームはコピーも削除もせず、そのまま使う。うまくいかなければ元のコンテナを起動し直し、
`bh migrate --undo <name>` で TypeScript 版へ戻せる。詳しくは仕様案の §11。
**最初の1つは、消えても困らない環境で試すこと。**

## 開発

道具は `flake.nix` の devShell で揃える:

```
nix develop --command go test ./...
nix develop --command go vet ./...
nix develop --command govulncheck ./...
```

構成と約束事は [CLAUDE.md](CLAUDE.md)。

## Issue / PR について

個人のプロジェクトとして、自分の使い方に合わせて作っている。
Issue は受け付けるが、返信や対応は約束しない。
PR は原則として受け付けない — 気づいた点は Issue で知らせてほしい。

## ライセンス

[MIT](LICENSE)
