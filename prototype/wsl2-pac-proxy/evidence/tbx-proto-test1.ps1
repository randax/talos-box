[Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

Write-Output "--- 1. PAC fetch over WSL localhost forwarding ---"
try { $p = Invoke-WebRequest "http://127.0.0.1:5390/tbx.pac" -UseBasicParsing -TimeoutSec 5
      Write-Output ("  HTTP {0}  Content-Type: {1}" -f $p.StatusCode, $p.Headers['Content-Type'])
      Write-Output ($p.Content -split "`n" | % { "    $_" }) }
catch { Write-Output ("  FAILED: " + $_.Exception.Message) }

Write-Output ""
Write-Output "--- 2. through the proxy explicitly (browser-independent) ---"
$P = "http://127.0.0.1:5390"
foreach ($u in @("https://wslproto-cp-1.wslproto.k8s.test:6443/","http://myapp.wslproto.k8s.test/","https://172.30.0.2:6443/","https://www.microsoft.com/")) {
  $sw = [Diagnostics.Stopwatch]::StartNew()
  try   { $r = Invoke-WebRequest $u -Proxy $P -UseBasicParsing -TimeoutSec 12 -ErrorAction Stop
          Write-Output ("  {0,-52} -> HTTP {1} in {2} ms" -f $u, $r.StatusCode, $sw.ElapsedMilliseconds) }
  catch { $c = ""
          if ($_.Exception.Response) { $c = " (HTTP " + [int]$_.Exception.Response.StatusCode + ")" }
          Write-Output ("  {0,-52} -> {1}{2} in {3} ms" -f $u, $_.Exception.Message.Split([Environment]::NewLine)[0], $c, $sw.ElapsedMilliseconds) }
}
