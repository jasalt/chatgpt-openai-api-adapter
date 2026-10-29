import base64, json, sys, datetime as dt
src, out = sys.argv[1], sys.argv[2]
d = json.load(open(src))
def claims(t):
    p = t.split('.')[1]; p += '=' * (-len(p) % 4)
    return json.loads(base64.urlsafe_b64decode(p))
c = claims(d['access_token'])
auth = c.get('https://api.openai.com/auth', {})
email = c.get('https://api.openai.com/profile', {}).get('email', '')
iso = lambda ms: dt.datetime.fromtimestamp(ms/1000, dt.timezone.utc).astimezone().isoformat(timespec='seconds')
o = {
  "id_token": d.get('id_token', ''),
  "access_token": d['access_token'],
  "refresh_token": d['refresh_token'],
  "account_id": d.get('account_id') or auth.get('chatgpt_account_id', ''),
  "last_refresh": dt.datetime.now().astimezone().isoformat(timespec='seconds'),
  "email": email,
  "type": "codex",
  "expired": iso(d['expires_at']),
}
plan = auth.get('chatgpt_plan_type', '')
name = f"codex-{email}-{plan}.json" if plan else f"codex-{email}.json"
open(out, 'w').write(json.dumps(o, indent=2)); 
print("claims keys:", list(c)); print("email set:", bool(email), "plan:", plan, "expired:", o['expired'], "file:", name)
