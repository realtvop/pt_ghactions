#!/bin/bash

# 更新
./cloudflared update

# 运行
cd ..
go build main.go
./main &> /dev/null & disown
cd github_actions
nohup ./cloudflared tunnel --url http://127.0.0.1:8081 &> ./cf.log & disown
tail -f ./cf.log
