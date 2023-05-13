#!/bin/bash

# 更新
./cloudflared update

# 运行
cd ..
go build main.go
./main &> /dev/null & disown
nohup github_actions/cloudflared tunnel --url http://127.0.0.1:7681 &> ./cf.log & disown
tail -f ./cf.log
