{
  buildGoModule,
  lib,
  version ? "0.0.0",
}:

buildGoModule {
  pname = "talos-box";
  inherit version;

  src = ../.;

  vendorHash = "sha256-pctW9tZ/ec+hfpuEwDMjJjOL4RyXGm4ipK75bjA4GPU=";

  env.CGO_ENABLED = 0;

  subPackages = [
    "cmd/tbx"
    "cmd/tbxd"
    "cmd/tbx-helper"
  ];

  ldflags = [
    "-X github.com/randax/talos-box/internal/version.Version=${version}"
  ];

  meta = {
    description = "Talos Linux VM clusters on your laptop";
    homepage = "https://github.com/randax/talos-box";
    mainProgram = "tbx";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  };
}
