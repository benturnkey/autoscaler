{
  description = "Development shell for autoscaler-codex";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            awscli2
            bashInteractive
            gcc
            git
            gnumake
            go
            gopls
            gotools
            which
          ];

          shellHook = ''
            export GIT_PAGER=cat
            export GO111MODULE=on

            echo "autoscaler-codex dev shell"
            echo "Tools: go, make, git, aws, gcc"
            echo "Examples:"
            echo "  cd cluster-autoscaler && make build BUILD_TAGS=aws"
            echo "  cd cluster-autoscaler && make test-unit BUILD_TAGS=aws"
          '';
        };
      });
}
