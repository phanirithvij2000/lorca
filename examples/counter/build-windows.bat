@echo off
go generate
go build -ldflags "-w -s" -o lorca-example.exe
@REM go build -ldflags "-H windowsgui -w -s" -o lorca-example.exe
