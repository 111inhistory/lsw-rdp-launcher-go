# Manual test fixture: run in a temporary task so the launching SSH session can exit.
$ErrorActionPreference = 'Stop'
Start-Sleep -Seconds 3
$client = [System.Net.Sockets.TcpClient]::new([System.Net.Sockets.AddressFamily]::InterNetworkV6)
try {
    $client.Connect('::1', 22)
    Start-Sleep -Seconds 4
} finally {
    $client.Dispose()
}
