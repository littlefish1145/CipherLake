@echo off
set GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn
start /B nexus.exe --config config.yaml
echo nexus started
