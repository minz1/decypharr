{
  description = "Decypharr — debrid mock qBittorrent with stable FUSE inodes";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in rec {
          frontend = pkgs.callPackage ./nix/frontend.nix { nodejs = pkgs.nodejs_24; pnpm = pkgs.pnpm_12; };
          default = pkgs.buildGo127Module {
            pname = "decypharr";
            version = "2.3-minz";
            src = ./.;
            vendorHash = "sha256-b8ue0IaCYI1qaFaWsLnp5P+VZyNPNEMleTLMbwLGfQ4=";
            subPackages = [ "." ];
            ldflags = [ "-s" "-w" ];
            nativeBuildInputs = [ pkgs.pkg-config pkgs.makeBinaryWrapper ];
            buildInputs = [ pkgs.fuse ];
            preBuild = ''
              cp -r --no-preserve=mode ${frontend}/. pkg/server/assets/build/
            '';
            # The vendored-modules fetch needs no UI build.
            overrideModAttrs = _: { preBuild = ""; };
            postInstall = ''
              wrapProgram $out/bin/decypharr \
                --prefix PATH : /run/wrappers/bin \
                --suffix PATH : ${lib.makeBinPath [ pkgs.rclone ]} \
                --prefix LD_LIBRARY_PATH : ${lib.makeLibraryPath [ pkgs.fuse ]}
            '';
            meta = with lib; {
              description = "Debrid mock qBittorrent with stable FUSE inodes";
              homepage = "https://github.com/minz1/decypharr";
              license = licenses.mit;
              platforms = [ "x86_64-linux" "aarch64-linux" ];
              mainProgram = "decypharr";
            };
          };
        }
      );

      # Each UI gate runs one command over the frontend's source and pinned node_modules.
      checks = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          frontend = self.packages.${system}.frontend;
          uiCheck = name: tools: command: frontend.overrideAttrs (old: {
            pname = "decypharr-${name}";
            pnpmDeps = frontend.pnpmDeps;
            nativeBuildInputs = old.nativeBuildInputs ++ tools;
            buildPhase = command;
            installPhase = "touch $out";
          });
        in {
          typecheck = uiCheck "typecheck" [ pkgs.typescript ] "pnpm typecheck";
          lint = uiCheck "lint" [ pkgs.oxlint ] "pnpm lint";
          fmt = uiCheck "fmt" [ pkgs.oxfmt ] "pnpm fmt:check";
          test = uiCheck "test" [ ] "pnpm test";
          inherit frontend;
          package = self.packages.${system}.default;
          # pkg/server's tests against the built UI: manifest entry names and the minified settings scripts.
          go-ui = self.packages.${system}.default.overrideAttrs (old: {
            pname = "decypharr-go-ui";
            subPackages = [ "pkg/server" ];
            nativeCheckInputs = (old.nativeCheckInputs or [ ]) ++ [ pkgs.nodejs ];
            postInstall = "";
          });
          templates = pkgs.runCommand "decypharr-templates" {
            nativeBuildInputs = [ pkgs.djlint ];
            src = lib.fileset.toSource {
              root = ./.;
              fileset = lib.fileset.unions [ ./.djlintrc ./pkg/server/templates ];
            };
          } ''
            export HOME=$TMPDIR
            cd $src
            djlint pkg/server/templates --lint
            djlint pkg/server/templates --check
            touch $out
          '';
        }
      );

      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go_1_27
              golangci-lint
              nodejs_24
              pnpm_12
              typescript
              oxlint
              tsgolint
              oxfmt
              djlint
            ];
          };
        }
      );

      overlays.default = final: _prev: {
        decypharr = self.packages.${final.system}.default;
      };

      # Import this in your nixosConfigurations. It defines all options and
      # wires them to env vars. The package is auto-set to this flake's build.
      nixosModules.default = { pkgs, lib, ... }: {
        imports = [ ./nix/module.nix ];
        services.decypharr.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };

      nixosModules.decypharr = self.nixosModules.default;
    };
}
