#!/usr/bin/env bash
# 区分「整个模型族不可用」还是「只有 -contributor-free 变体被限」：
# 用住宅出口 + 官方标识，打同族的付费版 jev-1.13 / muse-spark-1.2 / muse-spark-1.3。
set -u

API=http://127.0.0.1:9000
ADMIN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
ZKEY=$(sed -n 's/^ZEN_KEY_01=//p' /opt/zenproxy/.env)
TMPID=variant-probe
UA="opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

MODELS="jev-1.13 jev-1.13-free muse-spark-1.2 muse-spark-1.3 muse-spark-1.2-contributor-free space-bunny-free big-pickle"

# 挑两个有住宅 IP 的出口做对照
curl -s -m 20 -H "X-Admin-Token: $ADMIN" "$API/admin/api/proxies" > /tmp/px2.json
EXITS=$(python3 -c "
import json
ex=json.load(open('/tmp/px2.json'))['exits']
live=[e['id'] for e in ex if e.get('exit_ip')]
print(' '.join(live[:2]))
")
echo "  用到的住宅出口：$EXITS"

curl -s -m 20 -H "X-Admin-Token: $ADMIN" -X POST -H 'Content-Type: application/json' \
  -d "{\"id\":\"$TMPID\",\"name\":\"variant probe\",\"upstream\":\"zen\",\"api_key\":\"$ZKEY\",\"proxy\":\"$(echo $EXITS | cut -d' ' -f1)\",\"enabled\":true}" \
  "$API/admin/api/accounts" >/dev/null && echo "  临时账号已建"

cat > /tmp/vp.py <<'PY'
import json, os, random, string, subprocess, time
chunk = open('/tmp/chunk_title.txt', encoding='utf-8').read()
hexs='0123456789abcdef'; b62=string.ascii_letters+string.digits
def cid(p): return p+''.join(random.choice(hexs) for _ in range(12))+''.join(random.choice(b62) for _ in range(14))
api, acct, ua = os.environ['API'], os.environ['ACCT'], os.environ['UA']
admin, model = os.environ['ADMIN'], os.environ['MODEL']
exit_id = os.environ['EXIT']

subprocess.run(['curl','-s','-m','20','-H',f'X-Admin-Token: {admin}','-X','POST',
                '-H','Content-Type: application/json','-d',json.dumps({"id":acct,"proxy":exit_id}),
                f'{api}/admin/api/accounts'], capture_output=True)
time.sleep(0.5)

body={"model":model,"max_tokens":150,
      "messages":[{"role":"system","content":chunk},{"role":"user","content":"Say OK"}],"stream":True}
s=cid('ses_')
cmd=['curl','-sS','-m','60','-o','/tmp/o2.bin','-w','%{http_code}','-X','POST',
     f'{api}/{acct}/v1/chat/completions','-H','Content-Type: application/json',
     '-H',f'User-Agent: {ua}','-H','x-opencode-client: cli','-H','x-opencode-project: global',
     '-H',f'x-opencode-request: {cid("msg_")}','-H',f'x-opencode-session: {s}','-H',f'x-opencode-session-id: {s}',
     '--data-binary',json.dumps(body)]
code=subprocess.run(cmd,capture_output=True,text=True).stdout.strip()
raw=open('/tmp/o2.bin','rb').read().decode('utf-8','replace')
try:
    d=json.loads(raw); kind=(d.get('error') or {}).get('type') or 'OK'
except Exception:
    kind='OK(stream)' if 'chat.completion.chunk' in raw else (raw[:30] or '空')
print('  %-38s HTTP=%-4s %s' % (model, code, kind))
PY

for exit_id in $EXITS; do
    echo
    echo "=== 出口 $exit_id ==="
    for m in $MODELS; do
        API="$API" ACCT="$TMPID" UA="$UA" MODEL="$m" ADMIN="$ADMIN" EXIT="$exit_id" python3 /tmp/vp.py
    done
done

echo
echo "=== 清理 ==="
curl -s -m 20 -H "X-Admin-Token: $ADMIN" -X DELETE "$API/admin/api/accounts/$TMPID" >/dev/null && echo "  临时账号已删"
rm -f /tmp/vp.py /tmp/px2.json /tmp/o2.bin
