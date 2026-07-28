@echo off
set GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn
start /B cipherlake.exe --config config.yaml
echo cipherlake started
