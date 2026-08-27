{
  module,
  package,
  pkgs,
}:

let
  daemonStub = pkgs.writeTextFile {
    name = "talos-box-daemon-stub";
    executable = true;
    destination = "/bin/tbxd";
    text = ''
      #!${pkgs.python3}/bin/python3
      import json
      import socket

      listener = socket.socket(fileno=3)
      while True:
          connection, _ = listener.accept()
          with connection:
              request = json.loads(connection.makefile().readline())
              if request["op"] != "status":
                  response = {"ok": False, "error": "unexpected smoke-test operation"}
              else:
                  response = {"ok": True, "data": []}
              connection.sendall(json.dumps(response).encode() + b"\n")
    '';
  };

  smokePackage = pkgs.runCommand "talos-box-smoke-package" { } ''
    mkdir -p $out/bin
    ln -s ${package}/bin/tbx $out/bin/tbx
    ln -s ${package}/bin/tbx-helper $out/bin/tbx-helper
    ln -s ${daemonStub}/bin/tbxd $out/bin/tbxd
  '';

  helperProbe = pkgs.writeTextFile {
    name = "talos-box-helper-probe";
    executable = true;
    destination = "/bin/helper-probe";
    text = ''
      #!${pkgs.python3}/bin/python3
      import json
      import socket

      connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
      connection.connect("/var/run/tbx-helper.sock")
      request = {
          "op": "helper.info",
          "args": {"protocolVersion": 5},
      }
      connection.sendall(json.dumps(request).encode() + b"\n")
      response = json.loads(connection.makefile().readline())
      assert response["ok"], response
      assert response["data"]["protocolVersion"] == 5, response
    '';
  };
in
pkgs.testers.runNixOSTest {
  name = "talos-box-module-smoke";
  requiredFeatures.kvm = false;

  nodes.machine = {
    imports = [ module ];

    virtualisation.talosbox = {
      enable = true;
      package = smokePackage;
    };

    users.users.alice = {
      isNormalUser = true;
      uid = 1000;
      linger = true;
      extraGroups = [
        "tbx"
        "kvm"
      ];
    };

    virtualisation.memorySize = 1024;
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("tbx-helper.socket")

    machine.succeed("test $(stat -c %a /var/run/tbx-helper.sock) = 660")
    machine.succeed("test $(stat -c %U /var/run/tbx-helper.sock) = tbx")
    machine.succeed("test $(stat -c %G /var/run/tbx-helper.sock) = tbx")
    machine.succeed("runuser -u alice -- ${helperProbe}/bin/helper-probe")
    machine.wait_for_unit("tbx-helper.service")

    machine.wait_for_unit("user@1000.service")
    user_environment = "XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"
    machine.succeed(f"runuser -u alice -- env {user_environment} systemctl --user start tbxd.socket")
    machine.succeed(f"runuser -u alice -- env {user_environment} ${smokePackage}/bin/tbx status")
    machine.succeed(f"runuser -u alice -- env {user_environment} systemctl --user is-active tbxd.service")
  '';
}
