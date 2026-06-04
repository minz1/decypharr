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
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.buildGoModule {
            pname = "decypharr";
            version = "2.3-minz";
            src = ./.;
            vendorHash = "sha256-rv2LBdkyrsIyGvxoxXNrY8uSrKdAWXwGWbuElrISbKA=";
            subPackages = [ "." ];
            ldflags = [ "-s" "-w" ];
            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [ pkgs.fuse ];
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
