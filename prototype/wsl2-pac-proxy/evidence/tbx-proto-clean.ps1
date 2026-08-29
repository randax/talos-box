$log = "$env:LOCALAPPDATA\Temp\tbx-proto-clean.log"
function W($m) { $m | Tee-Object -FilePath $log -Append | Out-Null }
Remove-Item $log -ErrorAction SilentlyContinue
W "=== prototype #495 pre-clean $(Get-Date -Format o) ==="
W ("elevated: " + ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))

W "--- removing ALL NRPT rules (the .k8s.test leftover) ---"
Get-DnsClientNrptRule | Remove-DnsClientNrptRule -Force -ErrorAction SilentlyContinue
W ("NRPT rules remaining: " + ((Get-DnsClientNrptRule | Measure-Object).Count))

W "--- removing the 172.30.0.0/16 route ---"
Remove-NetRoute -DestinationPrefix '172.30.0.0/16' -Confirm:$false -ErrorAction SilentlyContinue
$r = Get-NetRoute -DestinationPrefix '172.30.0.0/16' -ErrorAction SilentlyContinue
W ("routes to 172.30.0.0/16 remaining: " + (($r | Measure-Object).Count))

W "--- hosts file: any k8s.test entries? ---"
$h = Select-String -Path "$env:SystemRoot\System32\drivers\etc\hosts" -Pattern 'k8s\.test' -ErrorAction SilentlyContinue
W ("hosts entries for k8s.test: " + (($h | Measure-Object).Count))

Clear-DnsClientCache
W "=== clean ==="
