{
  description = "bulkhead — a sandbox for development environments";

  # Keep nixpkgs the only input: every extra flake is one more party to trust.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [
        "aarch64-linux"
        "x86_64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.govulncheck
            pkgs.capslock
          ];
          # The toolchain comes from flake.lock and nowhere else: never let
          # the go command download a different one on its own.
          GOTOOLCHAIN = "local";
          # A build must not rewrite go.mod / go.sum behind our back.
          GOFLAGS = "-mod=readonly";
          CGO_ENABLED = "0";
        };
      });

      packages = forAllSystems (pkgs: {
        default = pkgs.buildGoModule {
          pname = "bh";
          version = "0.0.0-dev";
          # Only what the build reads, so docs edits do not change the output.
          src = pkgs.lib.fileset.toSource {
            root = ./.;
            fileset = pkgs.lib.fileset.unions [
              ./go.mod
              ./go.sum
              ./cmd
              ./containers
              ./internal
            ];
          };
          # Hash of every module the build downloads — a second pin on top of
          # go.sum. It changes whenever a dependency changes; update it in the
          # same commit, after reviewing what changed.
          vendorHash = "sha256-qtjqqRt8X0JGxleOKh12YJWuW4AbbQvadp2Xsw7Tgjc=";
          subPackages = [ "cmd/bh" ];
          env.CGO_ENABLED = "0";
          # There is no git checkout in the build for the go command to read
          # the commit from: say which one this is (bh version).
          ldflags = [ "-X main.commit=${self.shortRev or self.dirtyShortRev or "unknown"}" ];
          # The completion of zsh and bash comes with the package: a profile
          # that has bh has it without a line in ~/.zshrc.
          nativeBuildInputs = [ pkgs.installShellFiles ];
          postInstall = pkgs.lib.optionalString (pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform) ''
            installShellCompletion --cmd bh \
              --bash <($out/bin/bh completion bash) \
              --zsh <($out/bin/bh completion zsh)
          '';
          meta = {
            description = "A sandbox for development environments (Docker), against supply-chain attacks";
            mainProgram = "bh";
          };
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
