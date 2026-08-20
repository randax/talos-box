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
      # VERSION is the release contract: the release workflow refuses a tag
      # that disagrees with it. The flake reports VERSION plus the commit it
      # was built from ("0.2.0+abc1234", or "+dirty" for an uncommitted
      # working tree), so a pinned flake still identifies its exact source;
      # the release tarballs carry the bare tag via ldflags.
      baseVersion = nixpkgs.lib.removeSuffix "\n" (builtins.readFile ./VERSION);
      version = if self ? shortRev then "${baseVersion}+${self.shortRev}" else "${baseVersion}+dirty";
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
