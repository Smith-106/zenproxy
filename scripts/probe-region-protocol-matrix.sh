#!/usr/bin/env bash
# 矩阵实验：muse-spark-1.3-contributor-free × (出口国家) × (chat / messages)，全程带官方标识。
# 目的：区分「国家限制」和「协议不支持」——刚才两次观察到的结论不一致，必须钉死。
set -u

API=http://127.0.0.1:9000
KEY_ADMIN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
ZKEY=$(sed -n 's/^ZEN_KEY_01=//p' /opt/zenproxy/.env)
TMPID=matrix-probe
MODEL=${MODEL:-muse-spark-1.3-contributor-free}
UA="opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

curl -s -m 20 -H "X-Admin-Token: $KEY_ADMIN" "$API/admin/api/proxies" > /tmp/px.json
python3 - <<'PY' > /tmp/exits.txt
import json, urllib.request
ex = json.load(open('/tmp/px.json'))['exits']
for e in sorted([x for x in ex if x.get('exit_ip')], key=lambda x: x['id']):
    try:
        cc = json.load(urllib.request.urlopen('http://ip-api.com/json/%s?fields=countryCode' % e['exit_ip'], timeout=8)).get('countryCode', '?')
    except Exception:
        cc = '?'
    print('%s %s %s' % (e['id'], e['exit_ip'], cc))
PY

echo "=== 建临时账号 $TMPID（不动线上的 zen-01） ==="
curl -s -m 20 -H "X-Admin-Token: $KEY_ADMIN" -X POST -H 'Content-Type: application/json' \
  -d "{\"id\":\"$TMPID\",\"name\":\"matrix probe\",\"upstream\":\"zen\",\"api_key\":\"$ZKEY\",\"proxy\":\"$(head -1 /tmp/exits.txt | cut -d' ' -f1)\",\"enabled\":true}" \
  "$API/admin/api/accounts" >/dev/null && echo "  已建"

cat > /tmp/mx.py <<'PY'
import json, os, random, string, subprocess, sys
chunk = open('/tmp/chunk_title.txt', encoding='utf-8').read()
hexs='0123456789abcdef'; b62=string.ascii_letters+string.digits
def cid(p): return p+''.join(random.choice(hexs) for _ in range(12))+''.join(random.choice(b62) for _ in range(14))
api, acct, ua, model = os.environ['API'], os.environ['ACCT'], os.environ['UA'], os.environ['MODEL']

def post(path, identity, prompt):
    if path.endswith('/messages'):
        body={"model":model,"max_tokens":120,"messages":[{"role":"user","content":"Say OK"}]}
        if prompt: body["system"]=chunk
    else:
        msgs=[]
        if prompt: msgs.append({"role":"system","content":chunk})
        msgs.append({"role":"user","content":"Say OK"})
        body={"model":model,"messages":msgs,"max_tokens":120}
    body["stream"]=True
    cmd=['curl','-sS','-m','45','-o','/tmp/o.bin','-w','%{http_code}','-X','POST',f'{api}/{acct}{path}','-H','Content-Type: application/json']
    if identity:
        s=cid('ses_')
        cmd+=['-H',f'User-Agent: {ua}','-H','x-opencode-client: cli','-H','x-opencode-project: global',
              '-H',f'x-opencode-request: {cid("msg_")}','-H',f'x-opencode-session: {s}','-H',f'x-opencode-session-id: {s}']
    cmd+=['--data-binary',json.dumps(body)]
    code=subprocess.run(cmd,capture_output=True,text=True).stdout.strip()
    raw=open('/tmp/o.bin','rb').read().decode('utf-8','replace')
    try:
        d=json.loads(raw)
        e=d.get('error',{})
        kind=e.get('type') or ('OK' if d.get('id') else '?')
    except Exception:
        kind='OK(stream)' if ('message_start' in raw or 'chat.completion.chunk' in raw) else (raw[:24] or '空')
    return code, kind

for line in open('/tmp/exits.txt'):
    exit_id, ip, cc = line.split()
    subprocess.run(['curl','-s','-m','20','-H',f'X-Admin-Token: {os.environ["ADMIN"]}','-X','POST',
                    '-H','Content-Type: application/json','-d',json.dumps({"id":acct,"proxy":exit_id}),
                    f'{api}/admin/api/accounts'], capture_output=True)
    import time; time.sleep(0.4)
    c1,k1 = post('/v1/chat/completions', True, True)
    c2,k2 = post('/v1/messages', True, True)
    print('  %-15s %-16s %-3s  chat=%-4s %-26s  messages=%-4s %s' % (exit_id, ip, cc, c1, k1, c2, k2))
PY

echo
echo "=== 矩阵（$MODEL，全程带官方标识） ==="
printf '  %-15s %-16s %-3s  %-33s %s\n' "出口" "IP" "国" "chat/completions" "messages"
API="$API" ACCT="$TMPID" UA="$UA" MODEL="$MODEL" ADMIN="$KEY_ADMIN" python3 /tmp/mx.py

echo
echo "=== 清理 ==="
curl -s -m 20 -H "X-Admin-Token: $KEY_ADMIN" -X DELETE "$API/admin/api/accounts/$TMPID" | head -c 80 | sed 's/^/  /'
echo
curl -s -m 20 -H "X-Admin-Token: $KEY_ADMIN" "$API/admin/api/accounts" | python3 -c "
import json,sys; print('  剩余账号：%s' % ', '.join(a['id'] for a in json.load(sys.stdin)['accounts']))"
rm -f /tmp/mx.py /tmp/px.json /tmp/exits.txt /tmp/o.bin
