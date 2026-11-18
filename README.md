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

単体のバイナリを [Releases](https://github.com/ys319/bulkhead/releases) に置く
（`bh-windows-amd64.exe` / `bh-windows-arm64.exe` / `bh-darwin-arm64` / `bh-darwin-amd64` /
`bh-linux-amd64` / `bh-linux-arm64`）。落として `bh`（Windows は `bh.exe`）の名前で PATH の
通った場所へ置く。ほかに置くものは無い。

```
gh attestation verify bh-windows-amd64.exe --repo ys319/bulkhead   # このリポジトリの release.yaml が作ったもの
```

バイナリには署名が無い — ブラウザで落とすと、Windows は「発行元不明」、macOS は開けない旨の
警告を出す。

手元でビルドする場合:

```
nix build                 # result/bin/bh（Nix がある場合）
```

または、Go 1.26 以降で:

```
go build -o bh ./cmd/bh
```

できた `bh` を PATH の通った場所へ置く。ほかに置くものは無い（コンテナの定義はバイナリに
埋め込んである）。

### Nix（flake）で入れる

このリポジトリの flake は `packages.<system>.default` に `bh` を出す（Linux / macOS の
x86_64・aarch64）。zsh と bash の補完も一緒に入るので、`eval "$(bh completion …)"` は要らない。
home-manager の構成（flake）へ足す例:

```nix
# flake.nix の inputs
bulkhead.url = "github:ys319/bulkhead";

# home-manager のモジュール（inputs を extraSpecialArgs で渡している場合）
{ pkgs, bulkhead, ... }:
{
  home.packages = [ bulkhead.packages.${pkgs.stdenv.hostPlatform.system}.default ];
}
```

`inputs.nixpkgs.follows` は付けないのを勧める — 付けなければ、このリポジトリの `flake.lock` が
固定した nixpkgs（Go のツールチェーン）でビルドされる。更新は `nix flake update bulkhead`。

```
bh version       # ビルドと、接続先の Docker Engine
bh               # TUI
```

## 使い方

```
bh create web    # 環境を定義する（設定を書くだけ）
bh up web        # 起動する（初回はイメージをビルドする）
bh ssh web       # シェルに入る
bh open web      # VS Code で開く（--in claude で Claude デスクトップアプリへ）
bh down web      # 止める（コンテナとデータは残る）
```

`bh` だけで起動すると、同じ操作を全画面のメニューから行える。コマンドの一覧は `bh help`、
1つのコマンドの使い方は `bh help <コマンド>`（`bh <コマンド> --help` でも同じ）。

`bh open web --in claude` は、Claude デスクトップアプリ（v2.110.0 以降）の新規セッション画面を、
SSH の接続先 `bulkhead-web`・フォルダ `/home/developer/workspace` を入れた状態で開く
（`claude://code/new` のリンクを渡すだけ）。接続するかどうかはアプリの側で確認される。
鍵・ポート・ユーザーはリンクに載せず、bh が `~/.ssh` に書いている `Host bulkhead-web` から
来る — VS Code と同じ経路。

設定は `~/.config/bh`（Windows は `%APPDATA%\bh`。`BH_CONFIG_DIR` で変えられる）に置かれる。
環境のデータは Docker のボリュームにあり、**消すのは `bh destroy` / `bh reset` / `bh archive`
（ボリュームごと）と `bh rollback`（中身の入れ替え）だけ**。どれも確認を求める。

## 環境へ渡す値

3つの経路がある。どれも環境の中では環境変数になり、新しいシェルから効く。

```
bh var set web NODE_ENV development     # 秘密でない変数。値は表示され、設定と一緒に複製される
bh secret set web OPENAI_API_KEY        # 自分が持っている秘密の値。入力は表示されない
bh token set web github push            # サービスが発行する資格情報（GitHub / Hugging Face）
```

token は `bh token set web github push` のように、環境・サービス・プリセットを続けて書く。
GitHub のプリセットは `read` / `push` / `contribute` で、発行ページが項目入りで開く。何が選べるかは
補完が出す（zsh は `eval "$(bh completion zsh)"`、bash は `eval "$(bh completion bash)"`、PowerShell は
`bh completion powershell | Out-String | Invoke-Expression` — サービス、プリセットとその意味、環境の持つ変数名や
スナップショットの名前まで）。期限が近づいたら
`bh token refresh web` で、同じトークンを再発行して貼り直す。`bh token remove` は失効もさせる。

secret と token の値は、OS の資格情報ストア（macOS のキーチェーンなど）に入り、設定ディレクトリの
ファイルには残らない。ストアが使えない場所（ssh 越しなど）では `bh secret store file` で、
自分だけが読めるファイルに置く。渡した値は環境の中のコードすべてが読める — 守りになるのは、
トークンに持たせる権限の範囲だけ。

## データを取っておく

止めた環境の workspace と home を1つのファイル（スナップショット）にできる。

```
bh snapshot create web        # 設定ディレクトリの snapshots/ に取る
bh rollback web latest        # そこへ戻す（今の状態は先にスナップショットに取られる）
bh export web ~/backup        # 好きな場所へ書き出す
bh import ~/backup/bh-web-….tar.gz web2   # ファイルから新しい環境を作る
bh archive web                # スナップショットを取ってボリュームを手放す（一覧には残る）
bh unarchive web              # 戻す
```

ツールの入り方が壊れた、nix ストアが膨らんだ、というときは `bh reset web`。nix ストアと
キャッシュだけを消し、次の起動で `init.sh` がツールを入れ直す（workspace と home のデータは
残る）。home もまっさらにするのは `bh reset web --home`（home は先にスナップショットに取られる）。

スナップショットには nix ストアとキャッシュが入らない。アーカイブから戻したときやインポートした
ときは、最初の起動で `init.sh` がもう一度走ってツールを入れ直す（`init.sh` に書いていない
ツールは戻らない）。home の中身（ログイン情報・鍵・履歴）はそのまま入るので、ファイルは環境と
同じように扱う。

## TypeScript 版から移る

TypeScript 版の設定（`~/.config/bulkhead`）と環境はそのまま残る。環境は1つずつ引き取る:

```
bh migrate           # 引き取れる環境の一覧
bh migrate <name>    # 何をするかを示し、確認してから引き取る
```

ボリュームはコピーも削除もせず、そのまま使う。うまくいかなければ元のコンテナを起動し直し、
`bh migrate --undo <name>` で TypeScript 版へ戻せる。詳しくは仕様案の §11。
**最初の1つは、消えても困らない環境で試すこと。**

移行でデータに触れていないことは、bh とは別に確かめられる（読むだけ。ボリュームの作成時刻、
workspace の全ファイル、SSH のホスト鍵などを前後で比べる）:

```
scripts/verify-migrate.sh before <name>
bh migrate <name>
scripts/verify-migrate.sh after <bh> <name>
```

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
