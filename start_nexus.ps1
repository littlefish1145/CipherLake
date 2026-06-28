$env:GOLANG_PROTOBUF_REGISTRATION_CONFLICT = "warn"
Start-Process -FilePath "nexus.exe" -ArgumentList "--config", "config.yaml" -NoNewWindow
Write-Output "nexus started"
