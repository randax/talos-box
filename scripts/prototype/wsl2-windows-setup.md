# PROTOTYPE — Windows-side setup for the WSL2 smoke test

Companion to `wsl2-smoke.sh` (wayfinder ticket #461). These steps run on
**Windows**, not inside the Ubuntu distro. Throwaway with the branch.

## Create `.wslconfig` (raise the WSL memory cap)

The file must live at `C:\Users\<you>\.wslconfig` — the Windows user profile,
not the WSL home, and with no hidden `.txt` extension. Creating it from
PowerShell avoids both mistakes:

```powershell
@"
[wsl2]
memory=12GB

[experimental]
autoMemoryReclaim=disabled
"@ | Set-Content "$env:USERPROFILE\.wslconfig" -Encoding ascii
```

Verify it:

```powershell
Get-Content "$env:USERPROFILE\.wslconfig"
```

## Apply it

```powershell
wsl --shutdown
```

Wait ~10 seconds (WSL delays the actual VM teardown), and make sure nothing
else is holding the VM — `wsl -l -v` should list every distro as `Stopped`
(Docker Desktop counts).

## Confirm from inside Ubuntu

Reopen the Ubuntu shell (Start menu → Ubuntu, or `wsl` in PowerShell), then:

```sh
grep MemTotal /proc/meminfo   # expect ~12 GiB (~12300000 kB)
cd ~/talos-box && ./scripts/prototype/wsl2-smoke.sh env   # passes the gate now
```

## If MemTotal still shows ~8 GiB

- `ls -la /mnt/c/Users/*/.wslconfig*` from inside Ubuntu — confirms the file
  exists and isn't named `.wslconfig.txt`.
- Section header must be exactly `[wsl2]`.
- Re-run `wsl --shutdown` and wait the full 10 seconds before reopening.
