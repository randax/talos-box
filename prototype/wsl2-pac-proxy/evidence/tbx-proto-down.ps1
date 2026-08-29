Write-Output "=== item 6: PAC URL unreachable (tbxd not running) ==="
Write-Output "AutoConfigURL is still set; nothing is listening on 127.0.0.1:5390."
Write-Output ""
$sw = [Diagnostics.Stopwatch]::StartNew()
try { $r = Invoke-WebRequest "http://127.0.0.1:5390/tbx.pac" -UseBasicParsing -TimeoutSec 8 -ErrorAction Stop
      Write-Output ("  PAC fetch  -> HTTP {0} in {1} ms" -f $r.StatusCode, $sw.ElapsedMilliseconds) }
catch { Write-Output ("  PAC fetch  -> {0} in {1} ms" -f $_.Exception.Message.Split([Environment]::NewLine)[0], $sw.ElapsedMilliseconds) }

Write-Output ""
Write-Output "  system-proxy path (honours AutoConfigURL like a browser does):"
foreach ($u in @("http://myapp.wslproto.k8s.test/","http://www.microsoft.com/")) {
  $sw = [Diagnostics.Stopwatch]::StartNew()
  try   { $r = Invoke-WebRequest $u -UseBasicParsing -TimeoutSec 20 -ErrorAction Stop
          Write-Output ("  {0,-38} -> HTTP {1} in {2} ms" -f $u, $r.StatusCode, $sw.ElapsedMilliseconds) }
  catch { Write-Output ("  {0,-38} -> {1} in {2} ms" -f $u, $_.Exception.Message.Split([Environment]::NewLine)[0], $sw.ElapsedMilliseconds) }
}
