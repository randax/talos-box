{
  config,
  lib,
  pkgs,
  ...
}:

let
  inherit (lib)
    literalExpression
    mkEnableOption
    mkIf
    mkPackageOption
    ;
  cfg = config.virtualisation.talosbox;
  defaultPackage = pkgs.callPackage ./package.nix { };
  helperCapabilities = [
    "CAP_NET_ADMIN"
    "CAP_NET_BIND_SERVICE"
    "CAP_NET_RAW"
  ];
in
{
  options.virtualisation.talosbox = {
    enable = (mkEnableOption "Talos Box virtual-machine clusters") // {
      description = ''
        Whether to enable Talos Box virtual-machine clusters.

        Add each operator to both the `tbx` and `kvm` groups, for example:
        `users.users.alice.extraGroups = [ "tbx" "kvm" ];`.
      '';
    };

    package = (mkPackageOption pkgs "talos-box" { default = null; }) // {
      default = defaultPackage;
      defaultText = literalExpression "pkgs.callPackage <talos-box/nix/package.nix> { }";
    };

    qemu.package = mkPackageOption pkgs "qemu_kvm" { };

  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = builtins.elem pkgs.stdenv.hostPlatform.system [
          "x86_64-linux"
          "aarch64-linux"
        ];
        message = "virtualisation.talosbox is supported only on x86_64-linux and aarch64-linux.";
      }
    ];

    environment.systemPackages = [
      cfg.package
      cfg.qemu.package
    ];

    users.groups.tbx = { };
    users.users.tbx = {
      isSystemUser = true;
      group = "tbx";
    };

    boot.kernelModules = [
      "tun"
      "bridge"
      "vhost_net"
    ];

    services.resolved.enable = true;
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
          if (subject.user == "tbx" && (
              action.id == "org.freedesktop.resolve1.set-dns-servers" ||
              action.id == "org.freedesktop.resolve1.set-domains" ||
              action.id == "org.freedesktop.resolve1.revert"
          )) {
              return polkit.Result.YES;
          }
          return polkit.Result.NOT_HANDLED;
      });
    '';

    systemd.sockets.tbx-helper = {
      description = "Talos Box privileged helper socket";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "/var/run/tbx-helper.sock";
        SocketUser = "tbx";
        SocketMode = "0660";
        SocketGroup = "tbx";
        RemoveOnStop = true;
      };
    };

    systemd.services.tbx-helper = {
      description = "Talos Box privileged network helper";
      requires = [ "tbx-helper.socket" ];
      after = [
        "tbx-helper.socket"
        "network-pre.target"
      ];
      path = [ pkgs.systemd ];
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/tbx-helper";
        Environment = "TBX_HELPER_SOCKET=/var/run/tbx-helper.sock";
        User = "tbx";
        Group = "tbx";
        AmbientCapabilities = helperCapabilities;
        CapabilityBoundingSet = helperCapabilities;
        StateDirectory = "tbx";
        StateDirectoryMode = "0700";
        NoNewPrivileges = true;
        DynamicUser = false;
        PrivateNetwork = false;
        PrivateTmp = true;
        ProtectHome = true;
        ProtectSystem = "strict";
        ProtectControlGroups = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        DeviceAllow = [ "/dev/net/tun rw" ];
      };
    };

    systemd.user.sockets.tbxd = {
      description = "Talos Box user daemon socket";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "%h/.talosbox/tbxd.sock";
        SocketMode = "0600";
        DirectoryMode = "0700";
        RemoveOnStop = true;
      };
    };

    systemd.user.services.tbxd = {
      description = "Talos Box user daemon";
      requires = [ "tbxd.socket" ];
      after = [ "tbxd.socket" ];
      path = [
        cfg.qemu.package
        pkgs.xz
      ];
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/tbxd";
        Environment = "TBX_HELPER_SOCKET=/var/run/tbx-helper.sock";
        Restart = "on-failure";
      };
    };
  };
}
