{
  nixpkgs,
  pkgs,
  module,
  package,
}:

let
  makeSystem =
    extraModule:
    nixpkgs.lib.nixosSystem {
      system = pkgs.stdenv.hostPlatform.system;
      modules = [
        module
        extraModule
      ];
    };

  evaluated = makeSystem {
    system.stateVersion = "25.05";
    virtualisation.talosbox.enable = true;
  };

  disabled = makeSystem {
    system.stateVersion = "25.05";
  };

  overridePackage = pkgs.runCommand "talos-box-package-override" { } ''
    mkdir -p $out/bin
    touch $out/bin/tbx $out/bin/tbxd $out/bin/tbx-helper
  '';
  overrideQemu = pkgs.runCommand "talos-box-qemu-override" { } ''
    mkdir -p $out/bin
  '';
  overridden = makeSystem {
    system.stateVersion = "25.05";
    virtualisation.talosbox = {
      enable = true;
      package = overridePackage;
      qemu.package = overrideQemu;
    };
  };

  options = pkgs.nixosOptionsDoc {
    options = evaluated.options.virtualisation.talosbox;
  };

  helperService = evaluated.config.systemd.services.tbx-helper.serviceConfig;
  helperSocket = evaluated.config.systemd.sockets.tbx-helper.socketConfig;
  tbxdService = evaluated.config.systemd.user.services.tbxd.serviceConfig;
  tbxdSocket = evaluated.config.systemd.user.sockets.tbxd.socketConfig;
  overriddenHelper = overridden.config.systemd.services.tbx-helper.serviceConfig;
  overriddenTbxd = overridden.config.systemd.user.services.tbxd.serviceConfig;
  capabilities = [
    "CAP_NET_ADMIN"
    "CAP_NET_BIND_SERVICE"
    "CAP_NET_RAW"
  ];
in
{
  inherit package;

  module-eval =
    assert evaluated.config.virtualisation.talosbox.enable;
    assert !(disabled.config.users.groups ? tbx);
    assert !(disabled.config.users.users ? tbx);
    assert !(disabled.config.systemd.sockets ? tbx-helper);
    assert !(disabled.config.systemd.services ? tbx-helper);
    assert !(disabled.config.systemd.user.sockets ? tbxd);
    assert !(disabled.config.systemd.user.services ? tbxd);
    assert evaluated.config.users.groups.tbx.gid == null;
    assert helperSocket.ListenStream == "/var/run/tbx-helper.sock";
    assert helperSocket.SocketUser == "tbx";
    assert helperSocket.SocketGroup == "tbx";
    assert helperSocket.SocketMode == "0660";
    assert helperService.User == "tbx";
    assert helperService.Group == "tbx";
    assert helperService.Environment == "TBX_HELPER_SOCKET=/var/run/tbx-helper.sock";
    assert helperService.AmbientCapabilities == capabilities;
    assert helperService.CapabilityBoundingSet == capabilities;
    assert helperService.NoNewPrivileges;
    assert !helperService.DynamicUser;
    assert !helperService.PrivateNetwork;
    assert builtins.elem "AF_NETLINK" helperService.RestrictAddressFamilies;
    assert helperService.DeviceAllow == [ "/dev/net/tun rw" ];
    assert !(evaluated.config.security.wrappers ? tbx-helper);
    assert evaluated.config.services.resolved.enable;
    assert evaluated.config.security.polkit.enable;
    assert
      builtins.match ".*org.freedesktop.resolve1.set-domains.*" evaluated.config.security.polkit.extraConfig
      != null;
    assert tbxdSocket.ListenStream == "%h/.talosbox/tbxd.sock";
    assert tbxdSocket.SocketMode == "0600";
    assert tbxdSocket.DirectoryMode == "0700";
    assert tbxdService.Environment == "TBX_HELPER_SOCKET=/var/run/tbx-helper.sock";
    assert tbxdService.Restart == "on-failure";
    assert builtins.elem overridePackage overridden.config.environment.systemPackages;
    assert builtins.elem overrideQemu overridden.config.environment.systemPackages;
    assert overriddenHelper.ExecStart == "${overridePackage}/bin/tbx-helper";
    assert overriddenTbxd.ExecStart == "${overridePackage}/bin/tbxd";
    assert builtins.elem overrideQemu overridden.config.systemd.user.services.tbxd.path;
    pkgs.runCommand "talos-box-module-eval" { } ''
      touch $out
    '';

  options-doc = options.optionsCommonMark;

  vm-smoke = import ./vm-test.nix {
    inherit module package pkgs;
  };
}
