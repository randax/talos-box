# PROTOTYPE — THROWAWAY. Issue #495.

Answers: can a Windows browser reach a WSL2-hosted cluster by name through a PAC
file plus an HTTP CONNECT proxy, with **no Windows route, no NRPT rule and no
hosts entry**, and **no elevation**?

**Verdict: yes.** Every item validated on `PC-MVUTWZRMBUBA` (Win11 24H2, AD-joined
+ Intune, WSL 2.7.12, cluster `wslproto`) on 2026-08-29.

## Run it

    ./prototype/wsl2-pac-proxy/run.sh        # starts on 127.0.0.1:5390, logs to /tmp/tbx-pac-proxy.log

Then on Windows, with no elevation:

    reg.exe add "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" \
      /v AutoConfigURL /t REG_SZ /d "http://127.0.0.1:5390/tbx.pac" /f

## How it stands in for the real thing

#462 decided the proxy lives **inside `tbxd`** and resolves **in-process** via the
global lookup closure. This prototype resolves over UDP to the cluster bridge
gateway (`172.30.0.1:53`) instead, which is behaviourally identical as seen from
Windows and needs no `tbxd` changes. It deliberately does **not** use the system
resolver: WSL's generated `resolv.conf` points at the Windows NAT DNS proxy and
cannot answer for cluster names (`getent` and `dig` both fail; `dig @172.30.0.1`
works).

## Evidence

`evidence/` holds the proxy logs from both runs and the PowerShell probes used.
Full results are in the resolution comment on #495.

## Known gap

The bare `wslproto` cluster has no CNI or ingress controller, so nothing serves
the ingress VIP `172.30.0.200`. The wildcard browser path is proven up to TCP
connect (the browser hands the proxy `myapp.wslproto.k8s.test`, which resolves
correctly to `172.30.0.200`); the final hop needs a cluster with a real ingress
workload. That belongs in the spec's QA battery, not here.
