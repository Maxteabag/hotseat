"""Disposable offline wheel test; performs no real login or account switch."""
import argparse,base64,json,os,subprocess,tempfile,venv,sys
from pathlib import Path
from zipfile import ZipFile
parser=argparse.ArgumentParser(description="Verify bundled Codex helpers in an installed wheel using fixture credentials")
parser.add_argument("wheel",type=Path)
args=parser.parse_args()
wheel=args.wheel.resolve()
root=Path(tempfile.mkdtemp(prefix='hotseat-portable-'))
environment=root/'venv';venv.EnvBuilder(with_pip=True).create(environment)
python=str(environment/'bin/python')
with ZipFile(wheel) as z:
 for p in ['hotseat/codex_accounts.py','hotseat/compare_limits.py','hotseat/bundled.py']:assert p in z.namelist(),p
subprocess.run([python,'-m','pip','install','--no-index','--no-deps',str(wheel)],capture_output=True,check=True)
home=root/'home';home.mkdir();codex=home/'.codex';profiles=codex/'profiles';profiles.mkdir(parents=True)
def creds(email,account):
 body=base64.urlsafe_b64encode(json.dumps({'email':email}).encode()).decode().rstrip('=')
 return {'auth_mode':'chatgpt','tokens':{'id_token':'x.'+body+'.x','access_token':'fixture','refresh_token':'fixture','account_id':account}}
for name in ['old','work']:
 p=profiles/name;p.mkdir();(p/'auth.json').write_text(json.dumps(creds(name+'@example.com',name)))
(codex/'auth.json').write_text(json.dumps(creds('old@example.com','old')))
(profiles/'.current_profile').write_text('old')
bin=root/'bin';bin.mkdir()
fake=bin/'codex';fake.write_text('#!'+sys.executable+'\n'+'''
import sys,json
for line in sys.stdin:
 m=json.loads(line)
 if m.get('method')=='initialize':r={}
 elif m.get('method')=='account/rateLimits/read':r={'rateLimits':{'planType':'pro'},'rateLimitsByLimitId':{'codex':{'primary':{'usedPercent':40,'windowDurationMins':10080,'resetsAt':2000000000}}}}
 else:continue
 print(json.dumps({'id':m['id'],'result':r}),flush=True)
''');fake.chmod(0o700)
env=dict(os.environ,HOME=str(home),CODEX_HOME=str(codex),PATH=str(bin)+':/usr/bin:/bin',HOTSEAT_PLUGINS='none')
env.pop('PYTHONPATH',None)
def run(*args):return subprocess.run([python,'-I','-m',*args],cwd=root,env=env,capture_output=True,text=True,check=True)
listed=run('hotseat','codex-account','list').stdout;assert 'work@example.com' in listed
quota=json.loads(run('hotseat','codex','--json').stdout);assert len(quota['accounts'])==2
assert all(a['usage']['windows'][0]['used']==.4 for a in quota['accounts'])
result=json.loads(run('hotseat.tui_bridge','switch','--provider','codex','--alias','work','--acknowledged').stdout)
assert result['ok']
assert json.loads((codex/'auth.json').read_text())['tokens']['account_id']=='work'
assert json.loads((profiles/'old/auth.json').read_text())['tokens']['account_id']=='old'
assert not (home/'dotfiles').exists()
print(json.dumps({'installed_wheel':'passed','bundled_profile_cli':'passed','quota_without_dotfiles':'passed','tui_switch_without_external_helper':'passed','outgoing_credentials_preserved':True,'fixture_root':str(root)}))
