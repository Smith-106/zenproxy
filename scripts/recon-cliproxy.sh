#!/usr/bin/env bash
# 侦察已在运行的 cli-proxy-api 容器：端口、网络、配置位置、暴露的模型。
# 配置里含各家凭据，只报告"有没有"，绝不打印值。
set -u

echo "=== 容器概况 ==="
docker inspect cli-proxy-api --format '  image={{.Config.Image}}
  status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}}
  started={{.State.StartedAt}}'

echo
echo "=== 端口映射 ==="
docker inspect cli-proxy-api --format '{{range $p, $c := .NetworkSettings.Ports}}  {{$p}} -> {{$c}}
{{end}}'

echo
echo "=== 网络 ==="
docker inspect cli-proxy-api --format '{{range $net, $cfg := .NetworkSettings.Networks}}  {{$net}}  ip={{$cfg.IPAddress}}  aliases=[{{range $cfg.Aliases}}{{.}} {{end}}]
{{end}}'

echo "=== 挂载 ==="
docker inspect cli-proxy-api --format '{{range .Mounts}}  {{.Source}} -> {{.Destination}} ({{.Mode}})
{{end}}'

echo "=== 环境变量（键名，值一律打码） ==="
docker inspect cli-proxy-api --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -E 's/=.*/=<masked>/' | sed 's/^/  /'

echo
echo "=== 启动命令 ==="
docker inspect cli-proxy-api --format '  entrypoint={{.Config.Entrypoint}}  cmd={{.Config.Cmd}}'

echo
echo "=== 容器内进程与监听端口 ==="
docker exec cli-proxy-api sh -c 'ps aux 2>/dev/null | head -6; echo "---"; netstat -lntp 2>/dev/null | head -8 || ss -lntp 2>/dev/null | head -8' 2>&1 | sed 's/^/  /'

echo
echo "=== 配置文件在哪 ==="
docker exec cli-proxy-api sh -c 'ls -la /CLIProxyAPI/ 2>/dev/null | head -15; echo "--- 找 config ---"; find / -maxdepth 4 -name "config.yaml" -not -path "/proc/*" 2>/dev/null | head -5' 2>&1 | sed 's/^/  /'
