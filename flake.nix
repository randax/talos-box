{
  description = "Talos Linux VM clusters on your laptop";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = if self ? shortRev then "0.0.0+${self.shortRev}" else "0.0.0";
    in
    {
      overlays.default = final: prev: {
        talos-box = final.callPackage ./nix/package.nix { inherit version; };
      };

      packages = forAllSystems (pkgs: rec {
        talos-box = pkgs.callPackage ./nix/package.nix { inherit version; };
        default = talos-box;
      });

      nixosModules.default = import ./nix/module.nix;

      checks = forAllSystems (
        pkgs:
        import ./nix/checks.nix {
          inherit nixpkgs pkgs;
          module = self.nixosModules.default;
          package = self.packages.${pkgs.stdenv.hostPlatform.system}.talos-box;
        }
      );
    };
}
